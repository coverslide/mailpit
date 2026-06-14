package imap

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
)

// ---------------------------------------------------------------------------
// Test infrastructure
// ---------------------------------------------------------------------------

type clientTest struct {
	t        *testing.T
	client   *client.Client
	rootDir  string
	listener net.Listener
}

func newClientTest(t *testing.T) *clientTest {
	t.Helper()
	logger.NoLogging = true
	config.MaxMessages = 0

	root, err := os.MkdirTemp("", "mailpit-imap-integ-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	pwh := sha256.Sum256([]byte("pass"))
	yml := fmt.Sprintf("users:\n  u:\n    password-hash: %x\n", pwh)
	cfgPath := filepath.Join(root, "imap.yaml")
	if err := os.WriteFile(cfgPath, []byte(yml), 0644); err != nil {
		os.RemoveAll(root)
		t.Fatalf("WriteFile: %v", err)
	}

	config.IMAPConfigFile = cfgPath
	config.IMAPConfig = config.ImapConfigStruct{
		Users: map[string]config.ImapUserConfig{
			"u": {PasswordHash: fmt.Sprintf("%x", pwh)},
		},
	}
	config.Database = filepath.Join(root, "mailpit.db")

	if err := storage.InitDB(); err != nil {
		os.RemoveAll(root)
		t.Fatalf("InitDB: %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		storage.Close()
		os.RemoveAll(root)
		t.Fatalf("Listen: %v", err)
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handleClient(conn)
		}
	}()

	addr := listener.Addr().String()
	c, err := client.Dial(addr)
	if err != nil {
		listener.Close()
		storage.Close()
		os.RemoveAll(root)
		t.Fatalf("client.Dial: %v", err)
	}

	return &clientTest{t: t, client: c, rootDir: root, listener: listener}
}

func seqset(num uint32) *imap.SeqSet {
	s := &imap.SeqSet{}
	s.AddNum(num)
	return s
}

func (ct *clientTest) close() {
	ct.client.Logout()
	ct.listener.Close()
	storage.Close()
	if ct.rootDir != "" {
		os.RemoveAll(ct.rootDir)
	}
}

func (ct *clientTest) login() {
	ct.t.Helper()
	if err := ct.client.Login("u", "pass"); err != nil {
		ct.t.Fatalf("login: %v", err)
	}
}

func (ct *clientTest) selectInbox() *imap.MailboxStatus {
	ct.t.Helper()
	sel, err := ct.client.Select("INBOX", false)
	if err != nil {
		ct.t.Fatalf("select INBOX: %v", err)
	}
	return sel
}

func storeTestMessage(t *testing.T, raw []byte) string {
	t.Helper()
	id, err := storage.Store(&raw, nil, "")
	if err != nil {
		t.Fatalf("storeTestMessage: %v", err)
	}
	return id
}

func testMsg(subj, body string) []byte {
	return []byte(fmt.Sprintf(
		"From: sender@example.com\r\n"+
			"To: recipient@example.com\r\n"+
			"Subject: %s\r\n"+
			"Date: Wed, 01 Jan 2025 10:00:00 +0000\r\n"+
			"Message-ID: <%d@test>\r\n"+
			"Content-Type: text/plain\r\n"+
			"\r\n"+
			"%s", subj, time.Now().UnixNano(), body))
}

// ---------------------------------------------------------------------------
// Connection & Authentication
// ---------------------------------------------------------------------------

func TestIntegrationLogin(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()
}

func TestIntegrationLoginFailure(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	err := ct.client.Login("u", "wrongpassword")
	if err == nil {
		t.Fatal("expected login failure, got nil error")
	}
}

func TestIntegrationCapability(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()

	caps, err := ct.client.Capability()
	if err != nil {
		t.Fatalf("capability: %v", err)
	}
	t.Logf("Server capabilities: %v", caps)

	for _, must := range []string{"IMAP4rev1", "AUTH=PLAIN", "LITERAL+", "NAMESPACE"} {
		if !caps[must] {
			t.Errorf("server does not advertise %s", must)
		}
	}
	for _, mustNot := range []string{"MOVE", "LOGIN-REFERRALS", "UIDPLUS"} {
		if caps[mustNot] {
			t.Errorf("server advertises %s but does not implement it", mustNot)
		}
	}
}

func TestIntegrationLogout(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()
	if err := ct.client.Logout(); err != nil {
		t.Fatalf("logout: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LIST
// ---------------------------------------------------------------------------

func TestIntegrationListAll(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.List("", "*", mailboxes)
	}()

	var names []string
	for m := range mailboxes {
		names = append(names, m.Name)
		t.Logf("Mailbox: %s attrs=%v", m.Name, m.Attributes)
	}
	if err := <-done; err != nil {
		t.Fatalf("list: %v", err)
	}

	nameSet := make(map[string]bool)
	for _, n := range names {
		nameSet[n] = true
	}
	for _, expected := range []string{"INBOX", "Sent", "Trash", "Drafts"} {
		if !nameSet[expected] {
			t.Errorf("LIST missing expected mailbox %q", expected)
		}
	}
}

func TestIntegrationListSpecialUse(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.List("", "*", mailboxes)
	}()

	specialUse := map[string]string{}
	for m := range mailboxes {
		for _, attr := range m.Attributes {
			switch attr {
			case `\Sent`, `\Trash`, `\Drafts`:
				specialUse[m.Name] = attr
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatalf("list: %v", err)
	}

	if _, ok := specialUse["Sent"]; !ok {
		t.Error("Sent folder missing \\Sent attribute")
	}
	if _, ok := specialUse["Trash"]; !ok {
		t.Error("Trash folder missing \\Trash attribute")
	}
	if _, ok := specialUse["Drafts"]; !ok {
		t.Error("Drafts folder missing \\Drafts attribute")
	}
}

func TestIntegrationListPercent(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.List("", "%", mailboxes)
	}()

	count := 0
	for range mailboxes {
		count++
	}
	if err := <-done; err != nil {
		t.Fatalf("list: %v", err)
	}

	if count < 4 {
		t.Errorf("LIST \"\" \"%%\" returned %d mailboxes, want >= 4", count)
	}
}

func TestIntegrationListSpecific(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	mailboxes := make(chan *imap.MailboxInfo, 20)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.List("", "Sent", mailboxes)
	}()

	var found []*imap.MailboxInfo
	for m := range mailboxes {
		found = append(found, m)
	}
	if err := <-done; err != nil {
		t.Fatalf("list: %v", err)
	}

	if len(found) != 1 {
		t.Fatalf("LIST \"\" \"Sent\" returned %d mailboxes, want 1", len(found))
	}
	if found[0].Name != "Sent" {
		t.Errorf("expected Sent, got %s", found[0].Name)
	}
}

// ---------------------------------------------------------------------------
// SUBSCRIBE
// ---------------------------------------------------------------------------

func TestIntegrationSubscribe(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	if err := ct.client.Subscribe("INBOX"); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := ct.client.Unsubscribe("INBOX"); err != nil {
		t.Fatalf("unsubscribe: %v", err)
	}
}

// ---------------------------------------------------------------------------
// STATUS
// ---------------------------------------------------------------------------

func TestIntegrationStatusInbox(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	status, err := ct.client.Status("INBOX", []imap.StatusItem{
		imap.StatusMessages, imap.StatusUnseen, imap.StatusUidNext, imap.StatusUidValidity,
	})
	if err != nil {
		t.Fatalf("status: %v", err)
	}

	if status.Messages != 0 {
		t.Errorf("expected 0 messages, got %d", status.Messages)
	}
}

func TestIntegrationStatusSent(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	status, err := ct.client.Status("Sent", []imap.StatusItem{imap.StatusMessages})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Messages != 0 {
		t.Errorf("expected 0 messages in Sent, got %d", status.Messages)
	}
}

func TestIntegrationStatusWithMessages(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test1", "body1"))
	storeTestMessage(t, testMsg("Test2", "body2"))

	status, err := ct.client.Status("INBOX", []imap.StatusItem{imap.StatusMessages})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if status.Messages != 2 {
		t.Errorf("expected 2 messages, got %d", status.Messages)
	}
}

// ---------------------------------------------------------------------------
// SELECT / EXAMINE
// ---------------------------------------------------------------------------

func TestIntegrationSelectEmptyInbox(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	sel, err := ct.client.Select("INBOX", false)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Messages != 0 {
		t.Errorf("expected 0 messages, got %d", sel.Messages)
	}
	if sel.ReadOnly {
		t.Error("SELECT should be read-write")
	}
}

func TestIntegrationSelectWithMessages(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))

	sel, err := ct.client.Select("INBOX", false)
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if sel.Messages != 1 {
		t.Errorf("expected 1 message, got %d", sel.Messages)
	}
}

func TestIntegrationExamine(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))

	sel, err := ct.client.Select("INBOX", true)
	if err != nil {
		t.Fatalf("examine: %v", err)
	}
	if !sel.ReadOnly {
		t.Error("EXAMINE should be read-only")
	}
	if sel.Messages != 1 {
		t.Errorf("expected 1 message, got %d", sel.Messages)
	}
}

func TestIntegrationSelectSent(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	sel, err := ct.client.Select("Sent", false)
	if err != nil {
		t.Fatalf("select Sent: %v", err)
	}
	if sel.Messages != 0 {
		t.Errorf("expected 0 messages in Sent, got %d", sel.Messages)
	}
}

// ---------------------------------------------------------------------------
// FETCH
// ---------------------------------------------------------------------------

func TestIntegrationFetchEnvelope(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Hello World", "body"))
	ct.selectInbox()

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchEnvelope},
			messages,
		)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if msg == nil {
		t.Fatal("no message returned")
	}
	if msg.Envelope == nil {
		t.Fatal("envelope is nil")
	}
	if msg.Envelope.Subject != "Hello World" {
		t.Errorf("subject: got %q, want %q", msg.Envelope.Subject, "Hello World")
	}
	if len(msg.Envelope.From) == 0 {
		t.Error("from is empty")
	}
	if len(msg.Envelope.To) == 0 {
		t.Error("to is empty")
	}
}

func TestIntegrationFetchFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))
	ct.selectInbox()

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags, imap.FetchUid, imap.FetchRFC822Size},
			messages,
		)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if msg == nil {
		t.Fatal("no message returned")
	}
	if msg.Uid == 0 {
		t.Error("UID is 0")
	}
	if msg.Size == 0 {
		t.Error("RFC822.SIZE is 0")
	}
}

func TestIntegrationFetchBody(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("FetchBody", "Hello body content"))
	ct.selectInbox()

	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{section.FetchItem()},
			messages,
		)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if msg == nil {
		t.Fatal("no message returned")
	}

	body := msg.GetBody(section)
	if body == nil {
		t.Fatal("body is nil")
	}

	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	content := string(data)
	if content == "" {
		t.Error("body is empty")
	}
	t.Logf("Body (%d bytes): %.200s", len(content), content)
}

func TestIntegrationFetchBodyPeek(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("PeekTest", "peek body"))
	ct.selectInbox()

	// PEEK should not set \Seen
	section := &imap.BodySectionName{Peek: true}
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{section.FetchItem()},
			messages,
		)
	}()
	for range messages {
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Check flags
	flagsMsgs := make(chan *imap.Message, 10)
	done2 := make(chan error, 1)
	go func() {
		done2 <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			flagsMsgs,
		)
	}()
	var fmsg *imap.Message
	for m := range flagsMsgs {
		fmsg = m
	}
	if err := <-done2; err != nil {
		t.Fatalf("fetch flags: %v", err)
	}

	for _, f := range fmsg.Flags {
		if f == imap.SeenFlag {
			t.Error("BODY.PEEK[] should not set \\Seen flag")
		}
	}
}

func TestIntegrationFetchBodySetsSeen(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("SeenTest", "seen body"))
	ct.selectInbox()

	// Plain BODY[] should set \Seen
	section := &imap.BodySectionName{}
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{section.FetchItem()},
			messages,
		)
	}()
	for range messages {
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Check flags
	flagsMsgs := make(chan *imap.Message, 10)
	done2 := make(chan error, 1)
	go func() {
		done2 <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			flagsMsgs,
		)
	}()
	var fmsg *imap.Message
	for m := range flagsMsgs {
		fmsg = m
	}
	if err := <-done2; err != nil {
		t.Fatalf("fetch flags: %v", err)
	}

	found := false
	for _, f := range fmsg.Flags {
		if f == imap.SeenFlag {
			found = true
		}
	}
	if !found {
		t.Error("BODY[] should set \\Seen flag")
	}
}

func TestIntegrationFetchBodyStructure(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("StructTest", "body"))
	ct.selectInbox()

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchBodyStructure},
			messages,
		)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if msg == nil || msg.BodyStructure == nil {
		t.Fatal("body structure is nil")
	}
	t.Logf("BodyStructure: %#v", msg.BodyStructure)
}

// ---------------------------------------------------------------------------
// STORE
// ---------------------------------------------------------------------------

func TestIntegrationStoreAddFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Store", "body"))
	ct.selectInbox()

	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.AddFlags, false),
		[]interface{}{imap.SeenFlag},
		nil,
	); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Verify
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			messages,
		)
	}()
	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	found := false
	for _, f := range msg.Flags {
		if f == imap.SeenFlag {
			found = true
		}
	}
	if !found {
		t.Error("\\Seen flag not found after +FLAGS")
	}
}

func TestIntegrationStoreRemoveFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Store", "body"))
	ct.selectInbox()

	// Add \Seen
	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.AddFlags, false),
		[]interface{}{imap.SeenFlag},
		nil,
	); err != nil {
		t.Fatalf("store add: %v", err)
	}

	// Remove \Seen
	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.RemoveFlags, false),
		[]interface{}{imap.SeenFlag},
		nil,
	); err != nil {
		t.Fatalf("store remove: %v", err)
	}

	// Verify
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			messages,
		)
	}()
	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	for _, f := range msg.Flags {
		if f == imap.SeenFlag {
			t.Error("\\Seen flag still present after -FLAGS")
		}
	}
}

func TestIntegrationStoreReplaceFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Store", "body"))
	ct.selectInbox()

	// Replace all flags with just \Flagged
	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.SetFlags, false),
		[]interface{}{imap.FlaggedFlag},
		nil,
	); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Verify
	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			messages,
		)
	}()
	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}

	hasFlagged := false
	hasSeen := false
	for _, f := range msg.Flags {
		if f == imap.FlaggedFlag {
			hasFlagged = true
		}
		if f == imap.SeenFlag {
			hasSeen = true
		}
	}
	if !hasFlagged {
		t.Error("\\Flagged not found after FLAGS replacement")
	}
	if hasSeen {
		t.Error("\\Seen found after FLAGS replacement (should have been cleared)")
	}
}

// ---------------------------------------------------------------------------
// SEARCH
// ---------------------------------------------------------------------------

func TestIntegrationSearchAll(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("T1", "b1"))
	storeTestMessage(t, testMsg("T2", "b2"))
	storeTestMessage(t, testMsg("T3", "b3"))

	ct.selectInbox()

	results, err := ct.client.Search(imap.NewSearchCriteria())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 3 {
		t.Errorf("expected 3 results, got %d", len(results))
	}
}

func TestIntegrationSearchSubject(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Alpha", "body"))
	storeTestMessage(t, testMsg("Beta", "body"))
	storeTestMessage(t, testMsg("Gamma", "body"))

	ct.selectInbox()

	criteria := imap.NewSearchCriteria()
	criteria.Header.Set("Subject", "Beta")
	results, err := ct.client.Search(criteria)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0] != 2 {
		t.Errorf("expected [2], got %v", results)
	}
}

func TestIntegrationSearchBody(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("S1", "unique search term here"))
	storeTestMessage(t, testMsg("S2", "other body"))

	ct.selectInbox()

	results, err := ct.client.Search(&imap.SearchCriteria{
		Body: []string{"unique search term"},
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 || results[0] != 1 {
		t.Errorf("expected [1], got %v", results)
	}
}

func TestIntegrationSearchFrom(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))
	ct.selectInbox()

	criteria := imap.NewSearchCriteria()
	criteria.Header = make(map[string][]string)
	criteria.Header["From"] = []string{"sender@example.com"}

	results, err := ct.client.Search(criteria)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 1 {
		t.Errorf("expected 1 result, got %d", len(results))
	}
}

func TestIntegrationSearchEmpty(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	ct.selectInbox()

	results, err := ct.client.Search(imap.NewSearchCriteria())
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

// ---------------------------------------------------------------------------
// EXPUNGE
// ---------------------------------------------------------------------------

func TestIntegrationExpunge(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test1", "body1"))
	storeTestMessage(t, testMsg("Test2", "body2"))

	sel := ct.selectInbox()
	if sel.Messages != 2 {
		t.Fatalf("expected 2 messages, got %d", sel.Messages)
	}

	// Mark first as deleted
	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.AddFlags, false),
		[]interface{}{imap.DeletedFlag},
		nil,
	); err != nil {
		t.Fatalf("store: %v", err)
	}

	// Expunge
	expunged := make(chan uint32, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Expunge(expunged)
	}()

	var seqs []uint32
	for seq := range expunged {
		seqs = append(seqs, seq)
	}
	if err := <-done; err != nil {
		t.Fatalf("expunge: %v", err)
	}

	if len(seqs) != 1 {
		t.Fatalf("expected 1 expunge, got %d", len(seqs))
	}

	// Verify 1 message remains
	sel2, err := ct.client.Select("INBOX", false)
	if err != nil {
		t.Fatalf("re-select: %v", err)
	}
	if sel2.Messages != 1 {
		t.Errorf("expected 1 message after expunge, got %d", sel2.Messages)
	}
}

func TestIntegrationExpungeNone(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))
	ct.selectInbox()

	expunged := make(chan uint32, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Expunge(expunged)
	}()

	var seqs []uint32
	for seq := range expunged {
		seqs = append(seqs, seq)
	}
	if err := <-done; err != nil {
		t.Fatalf("expunge: %v", err)
	}
	if len(seqs) != 0 {
		t.Errorf("expected 0 expunges, got %d", len(seqs))
	}
}

// ---------------------------------------------------------------------------
// UID operations
// ---------------------------------------------------------------------------

func TestIntegrationUidSearch(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("T1", "b1"))
	storeTestMessage(t, testMsg("T2", "b2"))

	ct.selectInbox()

	results, err := ct.client.UidSearch(imap.NewSearchCriteria())
	if err != nil {
		t.Fatalf("uid search: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 UIDs, got %d", len(results))
	}
	for _, uid := range results {
		if uid == 0 {
			t.Error("UID is 0")
		}
	}
}

func TestIntegrationUidFetch(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("Test", "body"))

	sel := ct.selectInbox()

	uidSet := new(imap.SeqSet)
	uidSet.AddRange(1, sel.UidNext-1)

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.UidFetch(
			uidSet,
			[]imap.FetchItem{imap.FetchFlags, imap.FetchUid},
			messages,
		)
	}()

	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("uid fetch: %v", err)
	}

	if msg == nil {
		t.Fatal("no message returned")
	}
	if msg.Uid == 0 {
		t.Error("UID is 0")
	}
}

// ---------------------------------------------------------------------------
// APPEND
// ---------------------------------------------------------------------------

func TestIntegrationAppend(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	raw := testMsg("AppendTest", "appended body")
	if err := ct.client.Append("INBOX", []string{}, time.Time{}, bytes.NewReader(raw)); err != nil {
		t.Fatalf("append: %v", err)
	}

	sel := ct.selectInbox()
	if sel.Messages != 1 {
		t.Errorf("expected 1 message after append, got %d", sel.Messages)
	}
}

func TestIntegrationAppendWithFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	raw := testMsg("AppendFlags", "body")
	if err := ct.client.Append("INBOX", []string{imap.SeenFlag, imap.DraftFlag}, time.Time{}, bytes.NewReader(raw)); err != nil {
		t.Fatalf("append: %v", err)
	}

	ct.selectInbox()

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			messages,
		)
	}()
	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if msg == nil {
		t.Fatal("no message returned")
	}

	hasSeen := false
	hasDraft := false
	for _, f := range msg.Flags {
		if f == imap.SeenFlag {
			hasSeen = true
		}
		if f == imap.DraftFlag {
			hasDraft = true
		}
	}
	if !hasSeen {
		t.Error("\\Seen flag not found after APPEND")
	}
	if !hasDraft {
		t.Error("\\Draft flag not found after APPEND")
	}
}

func TestIntegrationAppendToSent(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	raw := testMsg("AppendSent", "body")
	if err := ct.client.Append("Sent", []string{}, time.Time{}, bytes.NewReader(raw)); err != nil {
		t.Fatalf("append to Sent: %v", err)
	}

	sel, err := ct.client.Select("Sent", false)
	if err != nil {
		t.Fatalf("select Sent: %v", err)
	}
	if sel.Messages != 1 {
		t.Errorf("expected 1 message in Sent, got %d", sel.Messages)
	}
}

// ---------------------------------------------------------------------------
// COPY
// ---------------------------------------------------------------------------

func TestIntegrationCopy(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("CopyMe", "body"))
	ct.selectInbox()

	if err := ct.client.Copy(seqset(1), "Sent"); err != nil {
		t.Fatalf("copy: %v", err)
	}

	sel, err := ct.client.Select("Sent", false)
	if err != nil {
		t.Fatalf("select Sent: %v", err)
	}
	if sel.Messages != 1 {
		t.Errorf("expected 1 message in Sent after copy, got %d", sel.Messages)
	}
}

func TestIntegrationCopyPreservesFlags(t *testing.T) {
	ct := newClientTest(t)
	defer ct.close()
	ct.login()

	storeTestMessage(t, testMsg("CopyWithFlags", "body"))
	ct.selectInbox()

	// Add \Seen and \Draft to the source message
	if err := ct.client.Store(
		seqset(1),
		imap.FormatFlagsOp(imap.AddFlags, false),
		[]interface{}{imap.SeenFlag, imap.DraftFlag},
		nil,
	); err != nil {
		t.Fatalf("store flags: %v", err)
	}

	if err := ct.client.Copy(seqset(1), "Sent"); err != nil {
		t.Fatalf("copy: %v", err)
	}

	sel, err := ct.client.Select("Sent", false)
	if err != nil {
		t.Fatalf("select Sent: %v", err)
	}
	if sel.Messages != 1 {
		t.Fatalf("expected 1 message in Sent after copy, got %d", sel.Messages)
	}

	messages := make(chan *imap.Message, 10)
	done := make(chan error, 1)
	go func() {
		done <- ct.client.Fetch(
			seqset(1),
			[]imap.FetchItem{imap.FetchFlags},
			messages,
		)
	}()
	var msg *imap.Message
	for m := range messages {
		msg = m
	}
	if err := <-done; err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if msg == nil {
		t.Fatal("no message returned")
	}

	hasSeen := false
	hasDraft := false
	for _, f := range msg.Flags {
		if f == imap.SeenFlag {
			hasSeen = true
		}
		if f == imap.DraftFlag {
			hasDraft = true
		}
	}
	if !hasSeen {
		t.Error("\\Seen flag not preserved by COPY")
	}
	if !hasDraft {
		t.Error("\\Draft flag not preserved by COPY")
	}
}
