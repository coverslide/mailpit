package imap

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
)

// ---------------------------------------------------------------------------
// test fixture
// ---------------------------------------------------------------------------

type imapTest struct {
	t           *testing.T
	client      net.Conn
	server      net.Conn
	reader      *bufio.Reader
	rootDir     string
	dbPath      string
	cfgPath     string
	tag         int
}

func newIMAPTest(t *testing.T) *imapTest {
	logger.NoLogging = true
	config.MaxMessages = 0

	root, err := os.MkdirTemp("", "mailpit-imap-e2e-*")
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
	config.IMAPConfig = config.ImapConfigStruct{Users: map[string]config.ImapUserConfig{"u": {PasswordHash: fmt.Sprintf("%x", pwh)}}}
	config.Database = filepath.Join(root, "mailpit.db")

	if err := storage.InitDB(); err != nil {
		os.RemoveAll(root)
		t.Fatalf("InitDB: %v", err)
	}

	client, server := net.Pipe()
	go handleClient(server)
	reader := bufio.NewReader(client)

	g, _ := reader.ReadString('\n')
	if !strings.Contains(g, "OK") {
		client.Close()
		server.Close()
		storage.Close()
		os.RemoveAll(root)
		t.Fatalf("bad greeting: %q", g)
	}

	return &imapTest{t: t, client: client, server: server, reader: reader, rootDir: root, dbPath: config.Database, cfgPath: cfgPath}
}

func (f *imapTest) close() {
	f.client.Close()
	f.server.Close()
	storage.Close()
	if f.rootDir != "" {
		os.RemoveAll(f.rootDir)
	}
}

func (f *imapTest) nextTag() string {
	f.tag++
	return fmt.Sprintf("a%03d", f.tag)
}

func (f *imapTest) write(s string) {
	if _, err := fmt.Fprint(f.client, s); err != nil {
		f.t.Fatal(err)
	}
}

func (f *imapTest) readLine() string {
	s, err := f.reader.ReadString('\n')
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.TrimRight(s, "\r\n")
}

// readUntil reads lines until one starts with prefix.
func (f *imapTest) readUntil(prefix string) []string {
	var lines []string
	for {
		l := f.readLine()
		lines = append(lines, l)
		if strings.HasPrefix(l, prefix) {
			return lines
		}
	}
}

// cmd sends a command with an auto-incrementing tag and returns all response lines.
func (f *imapTest) cmd(line string) []string {
	tag := f.nextTag()
	f.write(tag + " " + line + "\r\n")
	return f.readUntil(tag + " ")
}

// login as the test user.  Must be the first-issued command.
func (f *imapTest) login(user, pass string) {
	f.write("a001 LOGIN " + user + " " + pass + "\r\n")
	l := f.readLine()
	if !strings.Contains(l, "OK") {
		f.t.Fatalf("login failed: %q", l)
	}
	f.tag = 1
}

// selectInbox does SELECT INBOX, asserts success.
func (f *imapTest) selectInbox() {
	tag := f.nextTag()
	f.write(tag + " SELECT INBOX\r\n")
	for {
		l := f.readLine()
		if strings.HasPrefix(l, tag+" ") || strings.HasPrefix(l, tag+"[") {
			if !strings.Contains(l, "OK") {
				f.t.Fatalf("SELECT INBOX failed: %q", l)
			}
			return
		}
	}
}

// fetchCmd sends a FETCH command and reads the response, handling literals.
// seq and attrs are used as "FETCH <seq> (<attrs>)".
// Returns:
//   - header: first response line (e.g. "* 1 FETCH (BODY[] {N}")
//   - literal: the raw literal bytes, or nil if no literal in this fetch
//   - rest: remainder of the FETCH line after the literal (or "" if no literal)
//   - tagLine: the tagged OK/NO/BAD response (e.g. "a003 OK FETCH completed")
func (f *imapTest) fetchCmd(seq int, attrs string) (header string, literal []byte, rest string, tagLine string) {
	tag := f.nextTag()
	f.write(fmt.Sprintf("%s FETCH %d (%s)\r\n", tag, seq, attrs))
	return f.readFetchResponse(tag)
}

// uidFetchCmd sends a UID FETCH command.  Same return as fetchCmd.
func (f *imapTest) uidFetchCmd(seq int, attrs string) (header string, literal []byte, rest string, tagLine string) {
	tag := f.nextTag()
	f.write(fmt.Sprintf("%s UID FETCH %d (%s)\r\n", tag, seq, attrs))
	return f.readFetchResponse(tag)
}

// readFetchResponse reads a FETCH/UID FETCH response with optional literal handling.
func (f *imapTest) readFetchResponse(tag string) (header string, literal []byte, rest string, tagLine string) {
	header = f.readLine()

	br := strings.Index(header, "{")
	be := strings.Index(header, "}")
	if br >= 0 && be > br {
		n, err := strconv.Atoi(header[br+1 : be])
		if err != nil {
			f.t.Fatalf("bad literal size in %q: %v", header, err)
		}
		literal = make([]byte, n)
		if _, err := io.ReadFull(f.reader, literal); err != nil {
			f.t.Fatalf("read literal: %v", err)
		}
		rest = f.readLine()
	}

	tagLine = f.readLine()
	return
}

func storeMsg(raw []byte) string {
	id, err := storage.Store(&raw, nil)
	if err != nil {
		panic(fmt.Sprintf("Store: %v", err))
	}
	return id
}

func msg(subj, body string) []byte {
	return []byte(fmt.Sprintf("From: s@t.com\r\nTo: r@t.com\r\nSubject: %s\r\nDate: Wed, 01 Jan 2025 10:00:00 +0000\r\nMessage-ID: <%d@t>\r\nContent-Type: text/plain\r\n\r\n%s", subj, time.Now().UnixNano(), body))
}

func msgMulti() []byte {
	return []byte("From: s@t.com\r\nTo: r@t.com\r\nSubject: Multi\r\nDate: Thu, 02 Jan 2025 12:00:00 +0000\r\nMessage-ID: <m@t>\r\nMIME-Version: 1.0\r\nContent-Type: multipart/alternative; boundary=\"b\"\r\n\r\n--b\r\nContent-Type: text/plain\r\n\r\nPlain\r\n--b\r\nContent-Type: text/html\r\n\r\n<b>HTML</b>\r\n--b--\r\n")
}

// ---------------------------------------------------------------------------
// tests
// ---------------------------------------------------------------------------

func TestE2EConfigLifecycle(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()

	if _, err := os.Stat(f.cfgPath); os.IsNotExist(err) {
		t.Error("config file should exist on disk")
	}
	if _, err := os.Stat(f.dbPath); os.IsNotExist(err) {
		t.Error("database should exist on disk")
	}
}

func TestE2ELogin(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()

	f.write("a001 LOGIN u wrong\r\n")
	l := f.readLine()
	if !strings.Contains(l, "NO") && !strings.Contains(l, "BAD") {
		t.Fatalf("wrong password should fail, got %q", l)
	}

	f.write("a002 LOGIN u pass\r\n")
	l = f.readLine()
	if !strings.Contains(l, "OK") {
		t.Fatalf("correct password should succeed, got %q", l)
	}
}

func TestE2EAuthenticate(t *testing.T) {
	logger.NoLogging = true
	config.MaxMessages = 0
	root, _ := os.MkdirTemp("", "mailpit-auth-*")
	defer os.RemoveAll(root)

	pwh := sha256.Sum256([]byte("x"))
	yml := fmt.Sprintf("users:\n  u:\n    password-hash: %x\n", pwh)
	os.WriteFile(filepath.Join(root, "imap.yaml"), []byte(yml), 0644)
	config.IMAPConfigFile = filepath.Join(root, "imap.yaml")
	config.IMAPConfig = config.ImapConfigStruct{Users: map[string]config.ImapUserConfig{"u": {PasswordHash: fmt.Sprintf("%x", pwh)}}}
	config.Database = filepath.Join(root, "mailpit.db")
	storage.InitDB()
	defer storage.Close()

	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go handleClient(s)
	r := bufio.NewReader(c)
	r.ReadString('\n')

	c.Write([]byte("a AUTHENTICATE PLAIN\r\n"))
	cont, _ := r.ReadString('\n')
	if !strings.HasPrefix(cont, "+") {
		t.Fatalf("expected continuation, got %q", cont)
	}

	auth := base64.StdEncoding.EncodeToString([]byte{0, 'u', 0, 'x'})
	c.Write([]byte(auth + "\r\n"))
	resp, _ := r.ReadString('\n')
	if !strings.Contains(resp, "OK") {
		t.Fatalf("AUTHENTICATE should succeed, got %q", resp)
	}
}

func TestE2EMisc(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	lines := f.cmd("CAPABILITY")
	hasCap := false
	for _, l := range lines {
		if strings.HasPrefix(l, "* CAPABILITY") {
			hasCap = true
			if !strings.Contains(l, "IMAP4rev1") {
				t.Error("missing IMAP4rev1")
			}
			if !strings.Contains(l, "AUTH=PLAIN") {
				t.Error("missing AUTH=PLAIN")
			}
			if !strings.Contains(l, "LITERAL+") {
				t.Error("missing LITERAL+")
			}
		}
	}
	if !hasCap {
		t.Error("no * CAPABILITY line")
	}
	if !strings.HasPrefix(lines[len(lines)-1], "a002 OK") {
		t.Errorf("expected a002 OK, got %q", lines[len(lines)-1])
	}

	lines2 := f.cmd("NAMESPACE")
	if !strings.HasPrefix(lines2[0], "* NAMESPACE") {
		t.Errorf("NAMESPACE expected, got %q", lines2[0])
	}

	lines3 := f.cmd("NOOP")
	if !strings.Contains(lines3[0], "OK NOOP") {
		t.Errorf("NOOP failed, got %q", lines3[0])
	}

	lines4 := f.cmd("LOGOUT")
	if !strings.Contains(lines4[0], "* BYE") {
		t.Errorf("LOGOUT should BYE, got %q", lines4[0])
	}
	if !strings.Contains(lines4[len(lines4)-1], "OK LOGOUT") {
		t.Errorf("LOGOUT should OK, got %q", lines4[len(lines4)-1])
	}
}

func TestE2EMailbox(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Sel", "Hello"))
	f.selectInbox()

	tag := f.nextTag()
	f.write(tag + " EXAMINE INBOX\r\n")
	var examineLast string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, tag+" ") || strings.HasPrefix(l, tag+"[") {
			examineLast = l
			break
		}
	}
	if !strings.Contains(examineLast, "READ-ONLY") {
		t.Errorf("EXAMINE should be read-only, got %q", examineLast)
	}

	lines := f.cmd(`LIST "" "*"`)
	found := false
	for _, l := range lines {
		if strings.HasPrefix(l, "* LIST") && strings.Contains(l, "INBOX") {
			found = true
		}
	}
	if !found {
		t.Error("LIST should show INBOX")
	}

	lines2 := f.cmd(`LSUB "" "*"`)
	if !strings.HasPrefix(lines2[0], "* LSUB") {
		t.Errorf("LSUB expected, got %q", lines2[0])
	}

	lines3 := f.cmd("STATUS INBOX (MESSAGES UNSEEN UIDNEXT UIDVALIDITY)")
	if !strings.Contains(lines3[0], "MESSAGES") {
		t.Errorf("STATUS should have MESSAGES, got %q", lines3[0])
	}

	lines4 := f.cmd("CHECK")
	if !strings.Contains(lines4[0], "OK CHECK") {
		t.Errorf("CHECK failed, got %q", lines4[0])
	}

	lines5 := f.cmd("CLOSE")
	if !strings.Contains(lines5[0], "OK CLOSE") {
		t.Errorf("CLOSE failed, got %q", lines5[0])
	}
}

func TestE2EIdle(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	f.selectInbox()

	tag := f.nextTag()
	f.write(tag + " IDLE\r\n")
	cont := f.readLine()
	if !strings.Contains(cont, "+") {
		t.Fatalf("expected cont, got %q", cont)
	}
	f.write("DONE\r\n")
	done := f.readLine()
	if !strings.Contains(done, "OK IDLE") {
		t.Errorf("DONE should OK IDLE, got %q", done)
	}
}

func TestE2EFetchSimple(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("F1", "Body"))
	f.selectInbox()

	lines := f.cmd("FETCH 1 (FLAGS UID INTERNALDATE RFC822.SIZE)")
	fetch := lines[0]
	if !strings.HasPrefix(fetch, "* 1 FETCH") {
		t.Fatalf("bad FETCH, got %q", fetch)
	}
	for _, attr := range []string{"FLAGS", "UID", "INTERNALDATE", "RFC822.SIZE"} {
		if !strings.Contains(fetch, attr) {
			t.Errorf("missing %s in %q", attr, fetch)
		}
	}

	h, _, _, _ := f.fetchCmd(1, "FAST")
	for _, attr := range []string{"FLAGS", "INTERNALDATE", "RFC822.SIZE"} {
		if !strings.Contains(h, attr) {
			t.Errorf("FAST missing %s in %q", attr, h)
		}
	}

	h2, _, _, _ := f.fetchCmd(1, "ALL")
	if !strings.Contains(h2, "ENVELOPE") {
		t.Errorf("ALL should have ENVELOPE, got %q", h2)
	}

	h3, _, _, _ := f.fetchCmd(1, "FULL")
	if !strings.Contains(h3, "BODYSTRUCTURE") {
		t.Errorf("FULL should have BODYSTRUCTURE, got %q", h3)
	}
}

func TestE2EFetchBodyHeader(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("FH", "Body here"))
	f.selectInbox()

	hdr, lit, rest, _ := f.fetchCmd(1, "BODY[HEADER]")
	if !strings.Contains(hdr, "BODY[HEADER]") {
		t.Errorf("bad header, got %q", hdr)
	}
	if !strings.Contains(string(lit), "Subject: FH") {
		t.Errorf("header missing subject, got %q", string(lit))
	}
	if !strings.Contains(rest, "UID") {
		t.Errorf("rest missing UID, got %q", rest)
	}

	_, lit2, _, _ := f.fetchCmd(1, "BODY[]")
	if !strings.Contains(string(lit2), "Body here") {
		t.Errorf("body missing text, got %q", string(lit2))
	}

	lines := f.cmd("FETCH 1 (BODYSTRUCTURE)")
	if !strings.Contains(lines[0], "BODYSTRUCTURE") {
		t.Errorf("BODYSTRUCTURE missing, got %q", lines[0])
	}

	lines2 := f.cmd("FETCH 1 (ENVELOPE)")
	if !strings.Contains(lines2[0], "ENVELOPE") {
		t.Errorf("ENVELOPE missing, got %q", lines2[0])
	}
}

func TestE2EFetchMIMEPart(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMulti())
	f.selectInbox()

	_, lit, _, _ := f.fetchCmd(1, "BODY[1]")
	if !strings.Contains(string(lit), "text/plain") {
		t.Errorf("part 1 should have text/plain, got %q", string(lit))
	}
	if !strings.Contains(string(lit), "Plain") {
		t.Errorf("part 1 should have body, got %q", string(lit))
	}

	_, lit2, _, _ := f.fetchCmd(1, "BODY[2]")
	if !strings.Contains(string(lit2), "text/html") {
		t.Errorf("part 2 should have text/html, got %q", string(lit2))
	}

	_, lit3, _, _ := f.fetchCmd(1, "BODY[1.HEADER]")
	if !strings.Contains(string(lit3), "Content-Type") {
		t.Errorf("1.HEADER should have Content-Type, got %q", string(lit3))
	}
	if strings.Contains(string(lit3), "Plain") {
		t.Errorf("1.HEADER should not have body, got %q", string(lit3))
	}

	_, lit4, _, _ := f.fetchCmd(1, "BODY[1.TEXT]")
	if !strings.Contains(string(lit4), "Plain") {
		t.Errorf("1.TEXT should have body, got %q", string(lit4))
	}
	if strings.Contains(string(lit4), "Content-Type") {
		t.Errorf("1.TEXT should not have headers, got %q", string(lit4))
	}
}

func TestE2EFetchPartial(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Part", "ABCDEFGHIJKLMNOPQRSTUVWXYZ"))
	f.selectInbox()

	hdr, lit, _, _ := f.fetchCmd(1, "BODY[]<0.5>")
	if !strings.Contains(hdr, "BODY[]<0.5>") {
		t.Errorf("expected BODY[]<0.5> in header, got %q", hdr)
	}
	if string(lit) != "From:" {
		t.Errorf("expected 'From:', got %q", string(lit))
	}
}

func TestE2EFetchRFC822(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("RFC", "rfcbody"))
	f.selectInbox()

	_, lit, _, _ := f.fetchCmd(1, "RFC822.HEADER")
	if !strings.Contains(string(lit), "Subject: RFC") {
		t.Errorf("header missing subject, got %q", string(lit))
	}

	_, lit2, _, _ := f.fetchCmd(1, "RFC822.TEXT")
	if !strings.Contains(string(lit2), "rfcbody") {
		t.Errorf("text missing body, got %q", string(lit2))
	}
	if strings.Contains(string(lit2), "Subject") {
		t.Errorf("text should not have headers, got %q", string(lit2))
	}

	_, lit3, _, _ := f.fetchCmd(1, "RFC822")
	if !strings.Contains(string(lit3), "Subject: RFC") {
		t.Errorf("full missing subject, got %q", string(lit3))
	}
	if !strings.Contains(string(lit3), "rfcbody") {
		t.Errorf("full missing body, got %q", string(lit3))
	}
}

func TestE2EFetchBodyPeek(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Peek", "peekbody"))
	f.selectInbox()

	_, lit, _, _ := f.fetchCmd(1, "BODY.PEEK[]")
	if !strings.Contains(string(lit), "peekbody") {
		t.Errorf("BODY.PEEK should return body, got %q", string(lit))
	}
	if !strings.Contains(string(lit), "Subject: Peek") {
		t.Errorf("should have subject, got %q", string(lit))
	}
}

func TestE2EHeaderFields(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("HF", "body"))
	f.selectInbox()

	_, lit, _, _ := f.fetchCmd(1, `BODY[HEADER.FIELDS (SUBJECT FROM)]`)
	s := string(lit)
	if !strings.Contains(s, "Subject: HF") {
		t.Errorf("missing subject, got %q", s)
	}
	if !strings.Contains(s, "From:") {
		t.Errorf("missing From, got %q", s)
	}
	if strings.Contains(s, "To:") {
		t.Errorf("should not have To, got %q", s)
	}
}

func TestE2EFetchOrder(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Order", "body"))
	f.selectInbox()

	lines := f.cmd("FETCH 1 (UID FLAGS RFC822.SIZE)")
	fetch := lines[0]
	ui := strings.Index(fetch, "UID")
	fi := strings.Index(fetch, "FLAGS")
	si := strings.Index(fetch, "RFC822.SIZE")
	if ui < 0 || fi < 0 || si < 0 {
		t.Fatalf("missing attrs in %q", fetch)
	}
	if !(ui < fi && fi < si) {
		t.Errorf("expected UID < FLAGS < RFC822.SIZE, got %q", fetch)
	}
}

func TestE2EStore(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Store", "body"))
	f.selectInbox()

	lines := f.cmd(`STORE 1 +FLAGS (\Seen \Flagged)`)
	if !strings.Contains(lines[0], "\\Seen") || !strings.Contains(lines[0], "\\Flagged") {
		t.Errorf("STORE should add flags, got %q", lines[0])
	}

	lines2 := f.cmd("FETCH 1 (FLAGS)")
	if !strings.Contains(lines2[0], "\\Seen") || !strings.Contains(lines2[0], "\\Flagged") {
		t.Errorf("FLAGS should have both, got %q", lines2[0])
	}

	f.cmd(`STORE 1 -FLAGS (\Flagged)`)
	lines3 := f.cmd("FETCH 1 (FLAGS)")
	if strings.Contains(lines3[0], "\\Flagged") {
		t.Errorf("\\Flagged should be removed, got %q", lines3[0])
	}
	if !strings.Contains(lines3[0], "\\Seen") {
		t.Errorf("\\Seen should remain, got %q", lines3[0])
	}
}

func TestE2ESearch(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Alpha", "first body"))
	storeMsg(msg("Beta", "second body"))
	storeMsg(msg("Gamma", "third body"))
	f.selectInbox()

	check := func(desc, cmdline string, want []int) {
		t.Helper()
		lines := f.cmd(cmdline)
		for _, w := range want {
			if !strings.Contains(lines[0], fmt.Sprintf(" %d", w)) {
				t.Errorf("%s: missing seq %d in %q", desc, w, lines[0])
			}
		}
	}

	check("ALL", "SEARCH ALL", []int{1, 2, 3})
	check("SUBJECT Alpha", `SEARCH SUBJECT "Alpha"`, []int{1})
	check("TEXT first", `SEARCH TEXT "first"`, []int{1})
	check("BODY second", `SEARCH BODY "second"`, []int{2})
	check("FROM", `SEARCH FROM "s@t.com"`, []int{1, 2, 3})
	check("UNSEEN", "SEARCH UNSEEN", []int{1, 2, 3})

	// SEEN (should be empty)
	lines := f.cmd("SEARCH SEEN")
	if lines[0] != "* SEARCH" && lines[0] != "* SEARCH " {
		t.Errorf("SEEN should be empty, got %q", lines[0])
	}

	check("SMALLER", "SEARCH SMALLER 50000", []int{1, 2, 3})
	check("LARGER", "SEARCH LARGER 1", []int{1, 2, 3})
	check("SENTSINCE", `SEARCH SENTSINCE "01-Jan-2025"`, []int{1, 2, 3})
}

func TestE2ESearchORNOT(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Alpha", "b"))
	storeMsg(msg("Beta", "b"))
	storeMsg(msg("Gamma", "b"))
	f.selectInbox()

	lines := f.cmd(`SEARCH OR SUBJECT "Alpha" SUBJECT "Beta"`)
	if !strings.Contains(lines[0], "1") || !strings.Contains(lines[0], "2") {
		t.Errorf("OR should match 1 and 2, got %q", lines[0])
	}
	if strings.Contains(lines[0], "3") {
		t.Errorf("OR should not match 3, got %q", lines[0])
	}

	lines2 := f.cmd(`SEARCH NOT SUBJECT "Alpha"`)
	if strings.Contains(lines2[0], "1") {
		t.Errorf("NOT Alpha should not match 1, got %q", lines2[0])
	}
	if !strings.Contains(lines2[0], "2") || !strings.Contains(lines2[0], "3") {
		t.Errorf("NOT Alpha should match 2 and 3, got %q", lines2[0])
	}
}

func TestE2ESearchCharset(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("CS", "body"))
	f.selectInbox()

	lines := f.cmd(`SEARCH CHARSET UTF-8 SUBJECT "CS"`)
	if !strings.Contains(lines[0], "SEARCH 1") {
		t.Errorf("CHARSET search should find 1, got %q", lines[0])
	}
}

func TestE2EUID(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("U1", "b1"))
	storeMsg(msg("U2", "b2"))
	storeMsg(msg("U3", "b3"))
	f.selectInbox()

	h, _, _, _ := f.uidFetchCmd(2, "FLAGS")
	if !strings.Contains(h, "FETCH") {
		t.Errorf("UID FETCH expected, got %q", h)
	}
	if !strings.Contains(h, "UID 2") {
		t.Errorf("UID FETCH should show UID 2, got %q", h)
	}

	lines2 := f.cmd("UID SEARCH ALL")
	if !strings.Contains(lines2[0], "SEARCH 1 2 3") {
		t.Errorf("UID SEARCH should find all, got %q", lines2[0])
	}

	lines3 := f.cmd("UID STORE 2 +FLAGS (\\Deleted)")
	if !strings.Contains(lines3[0], "FLAGS") {
		t.Errorf("UID STORE should return FLAGS, got %q", lines3[0])
	}
}

func TestE2EExpunge(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Keep", "b"))
	storeMsg(msg("Delete", "b"))
	f.selectInbox()

	f.cmd("STORE 2 +FLAGS (\\Deleted)")
	lines := f.cmd("EXPUNGE")
	foundExp := false
	for _, l := range lines[:len(lines)-1] {
		if strings.Contains(l, "EXPUNGE") {
			foundExp = true
			break
		}
	}
	if !foundExp {
		t.Errorf("EXPUNGE should have untagged EXPUNGE, got %v", lines)
	}
	if !strings.Contains(lines[len(lines)-1], "OK EXPUNGE") {
		t.Errorf("EXPUNGE should OK, got %q", lines[len(lines)-1])
	}
}

func TestE2EMultiMessageFetch(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("M1", "b"))
	storeMsg(msg("M2", "b"))
	storeMsg(msg("M3", "b"))
	f.selectInbox()

	lines := f.cmd("FETCH 1:3 (FLAGS)")
	n := 0
	for _, l := range lines[:len(lines)-1] {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, " FETCH ") {
			n++
		}
	}
	if n != 3 {
		t.Errorf("expected 3 FETCH responses, got %d", n)
	}

	lines2 := f.cmd("FETCH 1,3 (FLAGS)")
	n2 := 0
	for _, l := range lines2[:len(lines2)-1] {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, " FETCH ") {
			n2++
		}
	}
	if n2 != 2 {
		t.Errorf("expected 2 FETCH responses, got %d", n2)
	}

	lines3 := f.cmd("UID FETCH * (FLAGS)")
	n3 := 0
	for _, l := range lines3[:len(lines3)-1] {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, " FETCH ") {
			n3++
		}
	}
	if n3 != 3 {
		t.Errorf("expected 3 UID FETCH responses, got %d", n3)
	}
}

func TestE2ETags(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("Tag", "b"))
	f.selectInbox()

	f.cmd(`STORE 1 +FLAGS (urgent)`)
	lines := f.cmd("FETCH 1 (FLAGS)")
	if !strings.Contains(lines[0], "urgent") {
		t.Errorf("tag should appear as flag, got %q", lines[0])
	}

	f.cmd(`STORE 1 -FLAGS (urgent)`)
	lines2 := f.cmd("FETCH 1 (FLAGS)")
	if strings.Contains(lines2[0], "urgent") {
		t.Errorf("tag should be removed, got %q", lines2[0])
	}
}
