package apiv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/axllent/mailpit/internal/storage"
)

// IMAPUser represents an IMAP user for the API
type IMAPUser struct {
	Username string `json:"username"`
}

// IMAPUserRequest represents a request to create/update an IMAP user
type IMAPUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// GetIMAPUsersHandler returns all IMAP users
func GetIMAPUsersHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		httpError(w, "method not allowed")
		return
	}

	users, err := storage.GetIMAPUsers()
	if err != nil {
		httpJSONError(w, "failed to get users: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(users)
}

// CreateIMAPUserHandler creates a new IMAP user
func CreateIMAPUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		httpError(w, "method not allowed")
		return
	}

	var req IMAPUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpJSONError(w, "invalid request body")
		return
	}

	if req.Username == "" || req.Password == "" {
		httpJSONError(w, "username and password are required")
		return
	}

	hash := sha256.Sum256([]byte(req.Password))
	passwordHash := hex.EncodeToString(hash[:])

	if err := storage.SetIMAPUserPassword(req.Username, passwordHash); err != nil {
		httpJSONError(w, "failed to create user: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(IMAPUser{Username: req.Username})
}

// DeleteIMAPUserHandler deletes an IMAP user
func DeleteIMAPUserHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		httpError(w, "method not allowed")
		return
	}

	username := r.URL.Query().Get("username")
	if username == "" {
		httpJSONError(w, "username is required")
		return
	}

	if err := storage.DeleteIMAPUser(username); err != nil {
		httpJSONError(w, "failed to delete user: "+err.Error())
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
