package apiv1

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/http"
	"strings"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/storage"
)

// NginxMailAuthHandler handles nginx mail proxy authentication requests.
//
// nginx sends Auth-User, Auth-Pass, Auth-Protocol, etc. as request headers.
// On success we respond with Auth-Status: OK plus Auth-Server/Auth-Port
// telling nginx where to proxy the connection.
// On failure we respond with Auth-Status: <error message>.
// HTTP status is always 200; body is ignored by nginx.
func NginxMailAuthHandler(w http.ResponseWriter, r *http.Request) {
	user := r.Header.Get("Auth-User")
	pass := r.Header.Get("Auth-Pass")
	protocol := strings.ToLower(r.Header.Get("Auth-Protocol"))

	if user == "" || pass == "" {
		setNginxAuthFailure(w)
		return
	}

	hash := sha256.Sum256([]byte(pass))
	passwordHash := hex.EncodeToString(hash[:])

	authenticated := false

	// Check the database first
	if storage.IMAPUserAuth(user, passwordHash) {
		authenticated = true
	}

	// Fall back to static config
	if !authenticated {
		if userCfg, ok := config.IMAPConfig.Users[user]; ok {
			if userCfg.PasswordHash == passwordHash {
				authenticated = true
			}
		}
	}

	if !authenticated {
		setNginxAuthFailure(w)
		return
	}

	// Extract port from the listener address for the requested protocol
	listenAddr := config.IMAPListen
	if protocol == "smtp" {
		listenAddr = config.SMTPListen
	}
	_, port, _ := net.SplitHostPort(listenAddr)

	w.Header().Set("Auth-Status", "OK")
	w.Header().Set("Auth-Server", config.NginxAuthServer)
	w.Header().Set("Auth-Port", port)
	w.WriteHeader(http.StatusOK)
}

// setNginxAuthFailure writes a failure response. nginx expects HTTP 200
// with Auth-Status containing the error message.
func setNginxAuthFailure(w http.ResponseWriter) {
	w.Header().Set("Auth-Status", "Invalid login or password")
	w.WriteHeader(http.StatusOK)
}
