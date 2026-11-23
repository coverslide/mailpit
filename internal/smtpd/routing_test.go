package smtpd

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
)

func setupRoutingTest(t *testing.T) {
	t.Helper()
	logger.NoLogging = true
	config.MaxMessages = 0
	oldDatabase := config.Database
	config.Database = filepath.Join(t.TempDir(), "mailpit.db")
	t.Cleanup(func() {
		storage.Close()
		config.Database = oldDatabase
	})
	if err := storage.InitDB(); err != nil {
		t.Fatal(err)
	}
}

func TestMailHandlerRoutesMessagesToIMAPUsers(t *testing.T) {
	setupRoutingTest(t)
	for _, user := range []string{"sender@example.com", "alice@example.com", "bob@example.com"} {
		if err := storage.RegisterIMAPUser(user); err != nil {
			t.Fatal(err)
		}
	}

	data := []byte("From: Sender <sender+campaign@example.com>\r\nTo: alice+tag@example.com, bob@example.com, outside@example.com\r\nSubject: routed\r\nMessage-ID: <routed@example.com>\r\n\r\nhello")
	origin := &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
	if _, err := mailHandler(origin, "sender+campaign@example.com", []string{"alice+tag@example.com", "bob@example.com", "outside@example.com"}, data, nil); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		user, folder string
		want         int64
	}{
		{"alice@example.com", "", 1},
		{"bob@example.com", "", 1},
		{"sender@example.com", "Sent", 1},
		{"outside@example.com", "", 0},
	} {
		total, _, _, err := storage.MailboxCounts(tc.folder, tc.user)
		if err != nil {
			t.Fatal(err)
		}
		if total != tc.want {
			t.Errorf("MailboxCounts(%q, %q) = %d, want %d", tc.folder, tc.user, total, tc.want)
		}
	}
}

func TestMailHandlerStoresSharedMessages(t *testing.T) {
	setupRoutingTest(t)
	data := []byte("From: sender@example.com\r\nTo: recipient@example.com\r\nSubject: shared\r\nMessage-ID: <shared@example.com>\r\n\r\nhello")
	origin := &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}
	if _, err := mailHandler(origin, "sender@example.com", []string{"recipient@example.com"}, data, nil); err != nil {
		t.Fatal(err)
	}
	total, _, _, err := storage.MailboxCounts("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("shared mailbox count = %d, want 1", total)
	}
}
