package imap

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
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

// --- Helper function tests for response formatting ---

func TestParsePartialRange(t *testing.T) {
	tests := []struct {
		attr       string
		wantBase   string
		wantOff    int
		wantSize   int
		wantHas    bool
	}{
		{"BODY[]<0.393216>", "BODY[]", 0, 393216, true},
		{"BODY[]<1024.2048>", "BODY[]", 1024, 2048, true},
		{"BODY[]", "BODY[]", 0, 0, false},
		{"BODY[HEADER]", "BODY[HEADER]", 0, 0, false},
		{"BODY[]<500.>", "BODY[]", 500, -1, true},
		{"BODY[]<500>", "BODY[]", 500, -1, true},
	}

	for _, tt := range tests {
		base, off, sz, has := parsePartialRange(tt.attr)
		if base != tt.wantBase {
			t.Errorf("parsePartialRange(%q) base = %q, want %q", tt.attr, base, tt.wantBase)
		}
		if off != tt.wantOff {
			t.Errorf("parsePartialRange(%q) offset = %d, want %d", tt.attr, off, tt.wantOff)
		}
		if has != tt.wantHas {
			t.Errorf("parsePartialRange(%q) has = %v, want %v", tt.attr, has, tt.wantHas)
		}
		if has && sz != tt.wantSize {
			t.Errorf("parsePartialRange(%q) size = %d, want %d", tt.attr, sz, tt.wantSize)
		}
	}
}

func TestExtractSection(t *testing.T) {
	tests := []struct {
		attr string
		want string
	}{
		{"BODY[]", ""},
		{"BODY[HEADER]", "HEADER"},
		{"BODY[1]", "1"},
		{"BODY[1.2]", "1.2"},
		{"BODY[TEXT]", "TEXT"},
		{"BODY[MIME]", "MIME"},
		{"BODY", ""},
		{"BODY[HEADER.FIELDS (DATE FROM)]", "HEADER.FIELDS (DATE FROM)"},
	}

	for _, tt := range tests {
		got := extractSection(tt.attr)
		if got != tt.want {
			t.Errorf("extractSection(%q) = %q, want %q", tt.attr, got, tt.want)
		}
	}
}

func TestIsNumericSection(t *testing.T) {
	tests := []struct {
		section string
		want    bool
	}{
		{"1", true},
		{"2", true},
		{"1.1", true},
		{"1.2.3", true},
		{"HEADER", false},
		{"TEXT", false},
		{"MIME", false},
		{"1.HEADER", false},
		{"", false},
	}

	for _, tt := range tests {
		got := isNumericSection(tt.section)
		if got != tt.want {
			t.Errorf("isNumericSection(%q) = %v, want %v", tt.section, got, tt.want)
		}
	}
}

func TestFilterRequestedHeaders(t *testing.T) {
	headers := "Date: Mon, 1 Jan 2024 12:00:00 +0000\r\nFrom: Alice <alice@test.com>\r\nTo: Bob <bob@test.com>\r\nSubject: Hello\r\nX-Custom: value\r\n"

	tests := []struct {
		name     string
		attr     string
		contains []string
		notCont  []string
	}{
		{
			"filter DATE and FROM",
			"BODY[HEADER.FIELDS (DATE FROM)]",
			[]string{"Date:", "From:"},
			[]string{"To:", "Subject:", "X-Custom:"},
		},
		{
			"filter SUBJECT only",
			"BODY[HEADER.FIELDS (SUBJECT)]",
			[]string{"Subject:"},
			[]string{"Date:", "From:", "To:", "X-Custom:"},
		},
		{
			"HEADER.FIELDS.NOT excludes X-Custom",
			"BODY[HEADER.FIELDS.NOT (X-Custom)]",
			[]string{"Date:", "From:", "To:", "Subject:"},
			[]string{"X-Custom:"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterRequestedHeaders(headers, tt.attr)
			for _, c := range tt.contains {
				if !strings.Contains(result, c) {
					t.Errorf("result should contain %q, got:\n%s", c, result)
				}
			}
			for _, nc := range tt.notCont {
				if strings.Contains(result, nc) {
					t.Errorf("result should NOT contain %q, got:\n%s", nc, result)
				}
			}
		})
	}
}

func TestSplitMultipartBody(t *testing.T) {
	boundary := "boundary123"
	body := "\r\n--boundary123\r\nContent-Type: text/plain\r\n\r\nHello\r\n--boundary123\r\nContent-Type: text/html\r\n\r\n<b>Hello</b>\r\n--boundary123--\r\n"

	parts := splitMultipartBody(body, boundary)
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %d: %v", len(parts), parts)
	}
	if !strings.Contains(parts[0], "Content-Type: text/plain") {
		t.Errorf("part 0 should contain Content-Type: text/plain, got %q", parts[0])
	}
	if !strings.Contains(parts[0], "Hello") {
		t.Errorf("part 0 should contain Hello, got %q", parts[0])
	}
	if !strings.Contains(parts[1], "Content-Type: text/html") {
		t.Errorf("part 1 should contain Content-Type: text/html, got %q", parts[1])
	}
	if !strings.Contains(parts[1], "<b>Hello</b>") {
		t.Errorf("part 1 should contain <b>Hello</b>, got %q", parts[1])
	}
}

func TestGetBodyHeader(t *testing.T) {
	raw := []byte("Date: Mon, 1 Jan 2024 12:00:00 +0000\r\nFrom: Alice <alice@test.com>\r\nSubject: Test\r\n\r\nThis is the body.")
	text := string(raw)

	// Test plain HEADER
	tag, data := getBodyHeader(raw, text, "BODY[HEADER]")
	if tag != "BODY[HEADER]" {
		t.Errorf("tag = %q, want BODY[HEADER]", tag)
	}
	if !strings.Contains(data, "Date:") {
		t.Errorf("data should contain Date:")
	}
	if !strings.Contains(data, "From:") {
		t.Errorf("data should contain From:")
	}
	if !strings.Contains(data, "Subject:") {
		t.Errorf("data should contain Subject:")
	}
	if strings.Contains(data, "This is the body") {
		t.Errorf("data should NOT contain body text")
	}
	if !strings.HasSuffix(data, "\r\n") {
		t.Errorf("data should end with CRLF")
	}

	// Test HEADER.FIELDS filter
	tag2, data2 := getBodyHeader(raw, text, "BODY[HEADER.FIELDS (DATE SUBJECT)]")
	if tag2 != "BODY[HEADER.FIELDS (DATE SUBJECT)]" {
		t.Errorf("tag = %q, want BODY[HEADER.FIELDS (DATE SUBJECT)]", tag2)
	}
	if !strings.Contains(data2, "Date:") {
		t.Errorf("filtered data should contain Date:")
	}
	if !strings.Contains(data2, "Subject:") {
		t.Errorf("filtered data should contain Subject:")
	}
	if strings.Contains(data2, "From:") {
		t.Errorf("filtered data should NOT contain From:")
	}
}

func TestBuildBodyStructureMultipart(t *testing.T) {
	raw := []byte("Content-Type: multipart/alternative; boundary=\"abc123\"\r\n\r\n--abc123\r\nContent-Type: text/plain\r\n\r\nHello\r\n--abc123\r\nContent-Type: text/html\r\n\r\n<b>Hello</b>\r\n--abc123--\r\n")
	result := buildBodyStructure(raw)

	// Should be a multipart body structure: (children "alternative" params)
	if !strings.HasPrefix(result, "(") {
		t.Errorf("result should start with (, got %q", result)
	}
	if !strings.Contains(result, "\"text\" \"plain\"") {
		t.Errorf("result should contain text/plain child, got %q", result)
	}
	if !strings.Contains(result, "\"text\" \"html\"") {
		t.Errorf("result should contain text/html child, got %q", result)
	}
	if !strings.Contains(result, "alternative") {
		t.Errorf("result should contain multipart subtype, got %q", result)
	}
}

func TestBuildBodyStructurePlain(t *testing.T) {
	raw := []byte("Content-Type: text/plain; charset=\"utf-8\"\r\n\r\nHello world")
	result := buildBodyStructure(raw)

	if !strings.Contains(result, "\"text\" \"plain\"") {
		t.Errorf("result should contain text/plain, got %q", result)
	}
}

func TestMergeParenGroups(t *testing.T) {
	tests := []struct {
		input []string
		want  []string
	}{
		{
			[]string{"FLAGS", "(\\Seen", "\\Deleted)"},
			[]string{"FLAGS", "(\\Seen \\Deleted)"},
		},
		{
			[]string{"(FLAGS", "UID)"},
			[]string{"(FLAGS UID)"},
		},
		{
			[]string{"(BODY.PEEK[HEADER.FIELDS", "(DATE", "FROM)])"},
			[]string{"(BODY.PEEK[HEADER.FIELDS (DATE FROM)])"},
		},
		{
			[]string{"a", "b", "c"},
			[]string{"a", "b", "c"},
		},
	}

	for _, tt := range tests {
		got := mergeParenGroups(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("mergeParenGroups(%v) = %v (len=%d), want %v (len=%d)", tt.input, got, len(got), tt.want, len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("mergeParenGroups(%v) = %v, want %v", tt.input, got, tt.want)
				break
			}
		}
	}
}

func TestStripQuotes(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{`"INBOX"`, "INBOX"},
		{`INBOX`, "INBOX"},
		{`"hello world"`, "hello world"},
		{`""`, ""},
	}

	for _, tt := range tests {
		got := stripQuotes(tt.input)
		if got != tt.want {
			t.Errorf("stripQuotes(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestSendCustomFetchWireFormat(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan string)
	go func() {
		result, _ := bufio.NewReader(client).ReadString('\n')
		done <- result
	}()

	msg := imapMessage{
		ID:   "test-id",
		UID:  42,
		Size: 1234,
		Flags: []string{"\\Seen"},
		Created: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
	}

	// Test with simple non-body attributes (no storage dependency)
	sendCustomFetch(server, 1, msg, "(FLAGS UID)")

	result := <-done
	expectedPrefix := "* 1 FETCH (FLAGS (\\Seen) UID 42)\r\n"
	if result != expectedPrefix {
		t.Errorf("wire output = %q, want %q", result, expectedPrefix)
	}
}

func TestSendCustomFetchWireFormatWithInternalDate(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan string)
	go func() {
		result, _ := bufio.NewReader(client).ReadString('\n')
		done <- result
	}()

	msg := imapMessage{
		ID:   "test-id",
		UID:  1,
		Size: 500,
		Flags: []string{},
		Created: time.Date(2024, 6, 15, 14, 30, 0, 0, time.FixedZone("EST", -5*60*60)),
	}

	sendCustomFetch(server, 3, msg, "(FLAGS INTERNALDATE RFC822.SIZE)")

	result := <-done
	if !strings.HasPrefix(result, "* 3 FETCH (") {
		t.Errorf("should start with FETCH, got %q", result)
	}
	if !strings.Contains(result, "FLAGS ()") {
		t.Errorf("should contain FLAGS (), got %q", result)
	}
	if !strings.Contains(result, "INTERNALDATE") {
		t.Errorf("should contain INTERNALDATE, got %q", result)
	}
	if !strings.Contains(result, "RFC822.SIZE 500") {
		t.Errorf("should contain RFC822.SIZE 500, got %q", result)
	}
	if !strings.Contains(result, "UID 1") {
		t.Errorf("should contain UID 1, got %q", result)
	}
	if !strings.HasSuffix(result, "\r\n") {
		t.Errorf("should end with CRLF")
	}
}

func TestSendCustomFetchMultipleMessages(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	done := make(chan string, 3)
	go func() {
		reader := bufio.NewReader(client)
		for i := 0; i < 3; i++ {
			line, _ := reader.ReadString('\n')
			done <- line
		}
		close(done)
	}()

	msg1 := imapMessage{ID: "id1", UID: 10, Size: 100, Created: time.Now()}
	msg2 := imapMessage{ID: "id2", UID: 20, Size: 200, Created: time.Now()}
	msg3 := imapMessage{ID: "id3", UID: 30, Size: 300, Created: time.Now()}

	sendCustomFetch(server, 1, msg1, "FLAGS")
	sendCustomFetch(server, 2, msg2, "FLAGS")
	sendCustomFetch(server, 3, msg3, "FLAGS")

	results := []string{<-done, <-done, <-done}
	if len(results) != 3 {
		t.Fatalf("expected 3 responses, got %d", len(results))
	}

	expected := []string{
		"* 1 FETCH (FLAGS () UID 10)\r\n",
		"* 2 FETCH (FLAGS () UID 20)\r\n",
		"* 3 FETCH (FLAGS () UID 30)\r\n",
	}
	for i, r := range results {
		if r != expected[i] {
			t.Errorf("response %d = %q, want %q", i+1, r, expected[i])
		}
	}
}



func TestBuildBodyStructureMessageRFC822(t *testing.T) {
	// Test a message/rfc822 attachment structure
	raw := []byte("Content-Type: message/rfc822\r\n\r\nSubject: nested\r\n\r\nbody")
	result := buildBodyStructure(raw)
	if !strings.Contains(result, "\"message\" \"rfc822\"") {
		t.Errorf("result should contain message/rfc822, got %q", result)
	}
}

func TestGetMIMEPartNonMultipart(t *testing.T) {
	// Non-multipart message: BODY[1] should return body text
	raw := []byte("Content-Type: text/plain\r\n\r\nHello world")
	content, err := getMIMEPartContent(raw, "1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(content) != "Hello world" {
		t.Errorf("content = %q, want %q", string(content), "Hello world")
	}
}

func TestGetMIMEPartMultipart(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=\"x\"\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nPart1\r\n--x\r\nContent-Type: text/html\r\n\r\nPart2\r\n--x--\r\n")
	
	content, err := getMIMEPartContent(raw, "1")
	if err != nil {
		t.Fatalf("getMIMEPartContent(raw, 1) error: %v", err)
	}
	if !strings.Contains(string(content), "Content-Type: text/plain") {
		t.Errorf("part 1 should contain Content-Type: text/plain, got %q", string(content))
	}
	if !strings.Contains(string(content), "Part1") {
		t.Errorf("part 1 should contain Part1, got %q", string(content))
	}

	content2, err2 := getMIMEPartContent(raw, "2")
	if err2 != nil {
		t.Fatalf("getMIMEPartContent(raw, 2) error: %v", err2)
	}
	if !strings.Contains(string(content2), "Content-Type: text/html") {
		t.Errorf("part 2 should contain Content-Type: text/html, got %q", string(content2))
	}
	if !strings.Contains(string(content2), "Part2") {
		t.Errorf("part 2 should contain Part2, got %q", string(content2))
	}

	_, err3 := getMIMEPartContent(raw, "3")
	if err3 == nil {
		t.Errorf("expected error for part 3, got nil")
	}
}

func TestGetMIMEPartMultipartHeader(t *testing.T) {
	raw := []byte("Content-Type: multipart/mixed; boundary=\"x\"\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nBody text\r\n--x\r\nContent-Type: text/html\r\n\r\n<b>Body</b>\r\n--x--\r\n")
	
	// BODY[1.HEADER] — headers of first part
	content, err := getMIMEPartContent(raw, "1.HEADER")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if !strings.Contains(string(content), "Content-Type: text/plain") {
		t.Errorf("should contain Content-Type: text/plain, got %q", string(content))
	}
	if strings.Contains(string(content), "Body text") {
		t.Errorf("should NOT contain body text, got %q", string(content))
	}

	// BODY[1.TEXT] — body of first part
	content2, err2 := getMIMEPartContent(raw, "1.TEXT")
	if err2 != nil {
		t.Fatalf("error: %v", err2)
	}
	if strings.Contains(string(content2), "Content-Type:") {
		t.Errorf("should NOT contain Content-Type header, got %q", string(content2))
	}
	if !strings.Contains(string(content2), "Body text") {
		t.Errorf("should contain 'Body text', got %q", string(content2))
	}
}

func TestFullIMAPConversation(t *testing.T) {
	passwordHash := sha256.Sum256([]byte("pass"))
	config.IMAPConfig = config.ImapConfigStruct{
		Users: map[string]config.ImapUserConfig{
			"test": {PasswordHash: hex.EncodeToString(passwordHash[:])},
		},
	}
	config.IMAPConfigFile = "test"

	logger.NoLogging = true
	config.MaxMessages = 0
	config.Database = ""
	if err := storage.InitDB(); err != nil {
		t.Fatal(err)
	}
	defer storage.Close()

	msgData, err := os.ReadFile("../storage/testdata/plain-text.eml")
	if err != nil {
		t.Fatal(err)
	}
	_, err = storage.Store(&msgData, nil)
	if err != nil {
		t.Fatal(err)
	}

	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	go handleClient(server)

	reader := bufio.NewReader(client)

	// Read greeting
	greeting, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading greeting: %v", err)
	}
	if !strings.Contains(greeting, "OK") {
		t.Fatalf("greeting should contain OK, got %q", greeting)
	}

	// LOGIN
	client.Write([]byte("a001 LOGIN test pass\r\n"))
	loginResp, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading login response: %v", err)
	}
	if !strings.Contains(loginResp, "OK") {
		t.Fatalf("login should succeed, got %q", loginResp)
	}

	// SELECT INBOX
	client.Write([]byte("a002 SELECT INBOX\r\n"))
	var selectLast string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SELECT response: %v", err)
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "a002 ") || line == "a002" {
			selectLast = line
			break
		}
	}
	if !strings.HasPrefix(selectLast, "a002 OK") {
		t.Fatalf("SELECT should complete with OK, got %q", selectLast)
	}

	// FETCH 1 (FLAGS UID) — simple attributes, no storage dependency
	client.Write([]byte("a003 FETCH 1 (FLAGS UID)\r\n"))
	// Read FETCH data line
	fetchLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading FETCH response: %v", err)
	}
	fetchLine = strings.TrimSpace(fetchLine)
	if !strings.Contains(fetchLine, "* 1 FETCH") {
		t.Fatalf("FETCH response should start with * 1 FETCH, got %q", fetchLine)
	}
	if !strings.Contains(fetchLine, "FLAGS ()") {
		t.Errorf("FETCH response should contain FLAGS (), got %q", fetchLine)
	}
	if !strings.Contains(fetchLine, "UID 1") {
		t.Errorf("FETCH response should contain UID 1, got %q", fetchLine)
	}
	// Read "a003 OK FETCH completed" line
	fetchTagLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading FETCH tag response: %v", err)
	}
	if !strings.HasPrefix(fetchTagLine, "a003 OK") {
		t.Fatalf("expected a003 OK, got %q", fetchTagLine)
	}

	// FETCH 1 (BODY[]) — full body literal
	client.Write([]byte("a004 FETCH 1 (BODY[])\r\n"))

	fetchLine2, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading BODY[] response header: %v", err)
	}
	fetchLine2 = strings.TrimSpace(fetchLine2)
	if !strings.Contains(fetchLine2, "BODY[] {") {
		t.Fatalf("expected BODY[] {N}, got %q", fetchLine2)
	}

	braceStart := strings.Index(fetchLine2, "{")
	braceEnd := strings.Index(fetchLine2, "}")
	if braceStart < 0 || braceEnd <= braceStart {
		t.Fatalf("cannot parse literal size from %q", fetchLine2)
	}
	literalSize, err := strconv.Atoi(fetchLine2[braceStart+1 : braceEnd])
	if err != nil {
		t.Fatalf("invalid literal size: %v", err)
	}

	literalData := make([]byte, literalSize)
	_, err = io.ReadFull(reader, literalData)
	if err != nil {
		t.Fatalf("reading literal data: %v", err)
	}
	literalStr := string(literalData)
	if !strings.Contains(literalStr, "Subject: Plain text message") {
		t.Errorf("literal data should contain Subject header, got:\n%s", literalStr)
	}

	restLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading FETCH rest: %v", err)
	}
	restLine = strings.TrimSpace(restLine)
	if !strings.Contains(restLine, "UID 1") {
		t.Errorf("FETCH rest should contain UID 1, got %q", restLine)
	}

	fetchDone, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading FETCH completion: %v", err)
	}
	fetchDone = strings.TrimSpace(fetchDone)
	if !strings.HasPrefix(fetchDone, "a004 OK") {
		t.Fatalf("expected FETCH OK, got %q", fetchDone)
	}

	// LOGOUT
	client.Write([]byte("a005 LOGOUT\r\n"))
	byeLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading BYE: %v", err)
	}
	if !strings.Contains(byeLine, "BYE") {
		t.Errorf("LOGOUT should return BYE, got %q", byeLine)
	}
	logoutLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading LOGOUT completion: %v", err)
	}
	if !strings.Contains(logoutLine, "a005 OK LOGOUT completed") {
		t.Errorf("LOGOUT should complete, got %q", logoutLine)
	}
}

func TestStripQuotesFromMailbox(t *testing.T) {
	// Test that stripQuotes works for mailbox names like "INBOX"
	if got := stripQuotes(`"INBOX"`); got != "INBOX" {
		t.Errorf("stripQuotes(\"INBOX\") = %q, want INBOX", got)
	}
	if got := stripQuotes(`"Sent Items"`); got != "Sent Items" {
		t.Errorf("stripQuotes(\"Sent Items\") = %q, want Sent Items", got)
	}
}
