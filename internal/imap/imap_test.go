package imap

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/axllent/mailpit/config"
)

func TestParseImapCommand(t *testing.T) {
	tests := []struct {
		line     string
		tag      string
		cmd      string
		argsLen  int
	}{
		{"a001 LOGIN user pass", "a001", "LOGIN", 2},
		{"a002 SELECT INBOX", "a002", "SELECT", 1},
		{"a003 FETCH 1:5 FLAGS", "a003", "FETCH", 2},
		{"a004 LOGOUT", "a004", "LOGOUT", 0},
		{"a005 UID FETCH 1:* FLAGS", "a005", "UID", 3},
		{"a006 STORE 1 +FLAGS (\\Deleted)", "a006", "STORE", 3},
		{"tag SEARCH UNSEEN", "tag", "SEARCH", 1},
		{"", "", "", 0},
	}

	for _, tt := range tests {
		tag, cmd, args := parseImapCommand(tt.line)
		if tag != tt.tag {
			t.Errorf("parseImapCommand(%q) tag = %q, want %q", tt.line, tag, tt.tag)
		}
		if cmd != tt.cmd {
			t.Errorf("parseImapCommand(%q) cmd = %q, want %q", tt.line, cmd, tt.cmd)
		}
		if len(args) != tt.argsLen {
			t.Errorf("parseImapCommand(%q) args len = %d, want %d (args: %v)", tt.line, len(args), tt.argsLen, args)
		}
	}
}

func TestSplitImapLine(t *testing.T) {
	tests := []struct {
		line string
		len  int
	}{
		{`a001 LOGIN user pass`, 4},
		{`a002 SELECT "Sent Items"`, 3},
		{`a003 FETCH 1:* (FLAGS UID)`, 5},
		{`a004 STORE 1 +FLAGS (\Deleted)`, 5},
	}

	for _, tt := range tests {
		result := splitImapLine(tt.line)
		if len(result) != tt.len {
			t.Errorf("splitImapLine(%q) len = %d, want %d (result: %v)", tt.line, len(result), tt.len, result)
		}
	}
}

func TestAuthenticateIMAP(t *testing.T) {
	// Setup config with test users
	password := "secret123"
	hash := sha256.Sum256([]byte(password))
	passwordHash := hex.EncodeToString(hash[:])

	config.IMAPConfig = config.ImapConfigStruct{
		Users: map[string]config.ImapUserConfig{
			"alice": {PasswordHash: passwordHash},
		},
	}
	config.IMAPConfigFile = "test-file" // non-empty to enable auth

	if !authenticateIMAP("alice", "secret123") {
		t.Error("authenticateIMAP(alice, secret123) should return true")
	}

	if authenticateIMAP("alice", "wrongpass") {
		t.Error("authenticateIMAP(alice, wrongpass) should return false")
	}

	if authenticateIMAP("bob", "secret123") {
		t.Error("authenticateIMAP(bob, secret123) should return false")
	}
}

func TestFormatFlags(t *testing.T) {
	tests := []struct {
		flags []string
		want  string
	}{
		{[]string{}, ""},
		{[]string{"\\Seen"}, "\\Seen"},
		{[]string{"\\Seen", "\\Deleted"}, "\\Seen \\Deleted"},
	}

	for _, tt := range tests {
		got := formatFlags(tt.flags)
		if got != tt.want {
			t.Errorf("formatFlags(%v) = %q, want %q", tt.flags, got, tt.want)
		}
	}
}

func TestFormatInternalDate(t *testing.T) {
	tm := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	got := formatInternalDate(tm)
	expected := "\"15-Jan-2024 10:30:00 +0000\""
	if got != expected {
		t.Errorf("formatInternalDate(%v) = %q, want %q", tm, got, expected)
	}
}

func TestParseMessageSet(t *testing.T) {
	messages := []imapMessage{
		{UID: 1},
		{UID: 2},
		{UID: 3},
		{UID: 4},
		{UID: 5},
	}

	tests := []struct {
		msgSet string
		exists int
		want   []int
	}{
		{"*", 5, []int{1, 2, 3, 4, 5}},
		{"1", 5, []int{1}},
		{"1:3", 5, []int{1, 2, 3}},
		{"1,3,5", 5, []int{1, 3, 5}},
		{"5:1", 5, []int{1, 2, 3, 4, 5}},
	}

	for _, tt := range tests {
		got := parseMessageSet(tt.msgSet, tt.exists, false, messages)
		if len(got) != len(tt.want) {
			t.Errorf("parseMessageSet(%q, %d) = %v, want %v", tt.msgSet, tt.exists, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseMessageSet(%q, %d) = %v, want %v", tt.msgSet, tt.exists, got, tt.want)
				break
			}
		}
	}
}

func TestAddRemoveFlags(t *testing.T) {
	existing := []string{"\\Seen"}

	added := addFlags(existing, []string{"\\Deleted"})
	if len(added) != 2 {
		t.Errorf("addFlags should have 2 flags, got %d", len(added))
	}

	removed := removeFlags(added, []string{"\\Seen"})
	if len(removed) != 1 || removed[0] != "\\Deleted" {
		t.Errorf("removeFlags should leave only \\Deleted, got %v", removed)
	}
}

func TestNilOrString(t *testing.T) {
	if nilOrString("") != "NIL" {
		t.Error("nilOrString('') should return NIL")
	}
	got := nilOrString("hello")
	if !strings.HasPrefix(got, "\"") || !strings.HasSuffix(got, "\"") {
		t.Errorf("nilOrString('hello') should be quoted, got %q", got)
	}
}

func TestSplitFetchAttrs(t *testing.T) {
	tests := []struct {
		attrs string
		want  []string
	}{
		{"FLAGS UID", []string{"FLAGS", "UID"}},
		{"(FLAGS UID INTERNALDATE)", []string{"(FLAGS UID INTERNALDATE)"}},
	}

	for _, tt := range tests {
		got := splitFetchAttrs(tt.attrs)
		if len(got) != len(tt.want) {
			t.Errorf("splitFetchAttrs(%q) = %v, want %v", tt.attrs, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("splitFetchAttrs(%q) = %v, want %v", tt.attrs, got, tt.want)
				break
			}
		}
	}
}

func TestIMAPCommandHandling(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	var state clientState = stateNotAuthenticated
	var user string
	var mbox *mailbox

	go func() {
		greeting, _ := bufio.NewReader(client).ReadString('\n')
		if !strings.Contains(greeting, "OK") {
			t.Errorf("greeting should contain OK, got %q", greeting)
		}

		client.Write([]byte("a001 CAPABILITY\r\n"))
		resp, _ := bufio.NewReader(client).ReadString('\n')
		if !strings.Contains(resp, "IMAP4rev1") {
			t.Errorf("CAPABILITY response should contain IMAP4rev1, got %q", resp)
		}
		resp2, _ := bufio.NewReader(client).ReadString('\n')
		if !strings.Contains(resp2, "OK") {
			t.Errorf("CAPABILITY should complete with OK, got %q", resp2)
		}

		client.Write([]byte("a002 LOGOUT\r\n"))
		bye, _ := bufio.NewReader(client).ReadString('\n')
		if !strings.Contains(bye, "BYE") {
			t.Errorf("LOGOUT should return BYE, got %q", bye)
		}
		client.Close()
	}()

	sendResponse(server, "* OK [CAPABILITY IMAP4rev1] Mailpit IMAP server")

	rawLine, _ := bufio.NewReader(server).ReadString('\n')
	rawLine = strings.TrimRight(rawLine, "\r\n")
	if !handleCommand(server, nil, rawLine, &state, &user, &mbox) {
		t.Fatal("handleCommand returned false on CAPABILITY")
	}

	rawLine2, _ := bufio.NewReader(server).ReadString('\n')
	rawLine2 = strings.TrimRight(rawLine2, "\r\n")
	if handleCommand(server, nil, rawLine2, &state, &user, &mbox) {
		t.Fatal("handleCommand should return false on LOGOUT (disconnect)")
	}
}

func TestHasFlag(t *testing.T) {
	flags := []string{"\\Seen", "\\Deleted"}

	if !hasFlag(flags, "\\Seen") {
		t.Error("hasFlag should find \\Seen")
	}

	if hasFlag(flags, "\\Answered") {
		t.Error("hasFlag should not find \\Answered")
	}
}
