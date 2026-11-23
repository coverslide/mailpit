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
	t       *testing.T
	client  net.Conn
	server  net.Conn
	reader  *bufio.Reader
	rootDir string
	dbPath  string
	cfgPath string
	tag     int
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
		sizeStr := header[br+1 : be]
		sizeStr = strings.TrimSuffix(sizeStr, "+")
		n, err := strconv.Atoi(sizeStr)
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
	id, err := storage.Store(&raw, nil, "", "u")
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

func msgMixed() []byte {
	attachment := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 fake pdf content"))
	return []byte(fmt.Sprintf(
		"From: Alice Smith <alice@example.com>\r\n"+
			"To: Bob Ross <bob@example.com>\r\n"+
			"Subject: Mixed Test\r\n"+
			"Date: Fri, 03 Jan 2025 14:00:00 +0000\r\n"+
			"Message-ID: <mixed@t>\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: multipart/mixed; boundary=\"mix\"\r\n"+
			"\r\n"+
			"--mix\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Here is the document.\r\n"+
			"--mix\r\n"+
			"Content-Type: application/pdf; name=\"doc.pdf\"\r\n"+
			"Content-Disposition: attachment; filename=\"doc.pdf\"\r\n"+
			"Content-Transfer-Encoding: base64\r\n"+
			"\r\n"+
			"%s\r\n"+
			"--mix--\r\n", attachment))
}

func msgMixedAlt() []byte {
	attachment := base64.StdEncoding.EncodeToString([]byte("%PDF-1.4 real pdf"))
	return []byte(fmt.Sprintf(
		"From: Carol Danvers <carol@example.com>\r\n"+
			"To: Tony Stark <tony@example.com>, Steve Rogers <steve@example.com>\r\n"+
			"Cc: Bruce Banner <bruce@example.com>\r\n"+
			"Subject: Report with Attachment\r\n"+
			"Date: Sat, 04 Jan 2025 09:00:00 +0000\r\n"+
			"Message-ID: <mixedalt@t>\r\n"+
			"Reply-To: Carol D <reply@example.com>\r\n"+
			"References: <thread1@old> <thread2@older>\r\n"+
			"In-Reply-To: <thread2@older>\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: multipart/mixed; boundary=\"mab\"\r\n"+
			"\r\n"+
			"--mab\r\n"+
			"Content-Type: multipart/alternative; boundary=\"alt\"\r\n"+
			"\r\n"+
			"--alt\r\n"+
			"Content-Type: text/plain; charset=utf-8\r\n"+
			"\r\n"+
			"Please see the attached report.\r\n"+
			"--alt\r\n"+
			"Content-Type: text/html; charset=utf-8\r\n"+
			"\r\n"+
			"<p>Please see the attached <b>report</b>.</p>\r\n"+
			"--alt--\r\n"+
			"\r\n"+
			"--mab\r\n"+
			"Content-Type: application/pdf; name=\"report.pdf\"\r\n"+
			"Content-Disposition: attachment; filename=\"report.pdf\"\r\n"+
			"Content-Transfer-Encoding: base64\r\n"+
			"\r\n"+
			"%s\r\n"+
			"--mab--\r\n", attachment))
}

func msgRelated() []byte {
	img := base64.StdEncoding.EncodeToString([]byte("\x89PNG fake image data"))
	return []byte(fmt.Sprintf(
		"From: Dave Lee <dave@example.com>\r\n"+
			"To: Eve Park <eve@example.com>\r\n"+
			"Subject: Inline Image\r\n"+
			"Date: Sun, 05 Jan 2025 11:00:00 +0000\r\n"+
			"Message-ID: <related@t>\r\n"+
			"MIME-Version: 1.0\r\n"+
			"Content-Type: multipart/related; boundary=\"rel\"\r\n"+
			"\r\n"+
			"--rel\r\n"+
			"Content-Type: text/html; charset=utf-8\r\n"+
			"\r\n"+
			"<p>Hello <img src=\"cid:photo.png\"></p>\r\n"+
			"--rel\r\n"+
			"Content-Type: image/png; name=\"photo.png\"\r\n"+
			"Content-ID: <photo.png@cid>\r\n"+
			"Content-Disposition: inline; filename=\"photo.png\"\r\n"+
			"Content-Transfer-Encoding: base64\r\n"+
			"\r\n"+
			"%s\r\n"+
			"--rel--\r\n", img))
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

func TestE2ECustomMailboxLifecycle(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	if lines := f.cmd(`CREATE "Archive"`); !strings.Contains(lines[len(lines)-1], "OK CREATE") {
		t.Fatalf("CREATE failed: %v", lines)
	}
	if lines := f.cmd(`CREATE "Archive"`); !strings.Contains(lines[len(lines)-1], "NO") {
		t.Fatalf("duplicate CREATE should fail: %v", lines)
	}
	if lines := f.cmd(`CREATE INBOX`); !strings.Contains(lines[len(lines)-1], "NO") {
		t.Fatalf("CREATE INBOX should fail: %v", lines)
	}
	lines := f.cmd(`LIST "" "*"`)
	found := false
	for _, line := range lines {
		if strings.Contains(line, "Archive") {
			found = true
		}
	}
	if !found {
		t.Fatalf("LIST did not include custom mailbox: %v", lines)
	}
	if lines := f.cmd(`DELETE "Archive"`); !strings.Contains(lines[len(lines)-1], "OK DELETE") {
		t.Fatalf("DELETE failed: %v", lines)
	}
	if lines := f.cmd(`DELETE "Archive"`); !strings.Contains(lines[len(lines)-1], "NO") {
		t.Fatalf("deleting a missing mailbox should fail: %v", lines)
	}
	if lines := f.cmd(`DELETE INBOX`); !strings.Contains(lines[len(lines)-1], "NO") {
		t.Fatalf("DELETE INBOX should fail: %v", lines)
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

// ---------------------------------------------------------------------------
// multipart + attachment tests
// ---------------------------------------------------------------------------

func TestE2EMixedBodyStructure(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixed())
	f.selectInbox()

	hdr, _, _, _ := f.fetchCmd(1, "BODYSTRUCTURE")
	structure := hdr
	t.Logf("BODYSTRUCTURE: %s", structure)

	if !strings.Contains(structure, `"mixed"`) {
		t.Errorf("expected multipart/mixed, got %q", structure)
	}
	if !strings.Contains(structure, `"plain"`) {
		t.Errorf("expected text/plain child, got %q", structure)
	}
	if !strings.Contains(structure, `"pdf"`) {
		t.Errorf("expected application/pdf child, got %q", structure)
	}
}

func TestE2EMixedFetchParts(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixed())
	f.selectInbox()

	// Part 1: text/plain body
	_, lit1, _, _ := f.fetchCmd(1, "BODY[1]")
	part1 := string(lit1)
	if !strings.Contains(part1, "text/plain") {
		t.Errorf("part 1 should contain text/plain header, got %q", part1)
	}
	if !strings.Contains(part1, "Here is the document") {
		t.Errorf("part 1 should contain body text, got %q", part1)
	}

	// Part 2: application/pdf attachment
	_, lit2, _, _ := f.fetchCmd(1, "BODY[2]")
	part2 := string(lit2)
	if !strings.Contains(part2, "application/pdf") {
		t.Errorf("part 2 should contain application/pdf header, got %q", part2)
	}
	if !strings.Contains(part2, "Content-Transfer-Encoding: base64") {
		t.Errorf("part 2 should have base64 encoding header, got %q", part2)
	}
	if !strings.Contains(part2, "doc.pdf") {
		t.Errorf("part 2 should reference doc.pdf filename, got %q", part2)
	}

	// Part 1.HEADER: just MIME headers of part 1
	_, lit1h, _, _ := f.fetchCmd(1, "BODY[1.HEADER]")
	hdr1 := string(lit1h)
	if !strings.Contains(hdr1, "Content-Type") {
		t.Errorf("1.HEADER should have Content-Type, got %q", hdr1)
	}
	if strings.Contains(hdr1, "document") {
		t.Errorf("1.HEADER should not have body content, got %q", hdr1)
	}

	// Part 1.TEXT: just body of part 1
	_, lit1t, _, _ := f.fetchCmd(1, "BODY[1.TEXT]")
	text1 := string(lit1t)
	if !strings.Contains(text1, "Here is the document") {
		t.Errorf("1.TEXT should have body text, got %q", text1)
	}
	if strings.Contains(text1, "Content-Type") {
		t.Errorf("1.TEXT should not have headers, got %q", text1)
	}
}

func TestE2EMixedSearch(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixed())
	f.selectInbox()

	// SEARCH should find text in the body (plain text part)
	lines := f.cmd(`SEARCH BODY "document"`)
	if !strings.Contains(lines[0], "SEARCH 1") {
		t.Errorf("SEARCH BODY should find message, got %q", lines[0])
	}

	// SEARCH on subject
	lines3 := f.cmd(`SEARCH SUBJECT "Mixed Test"`)
	if !strings.Contains(lines3[0], "SEARCH 1") {
		t.Errorf("SEARCH SUBJECT should find message, got %q", lines3[0])
	}
}

func TestE2EMixedAltBodyStructure(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixedAlt())
	f.selectInbox()

	hdr, _, _, _ := f.fetchCmd(1, "BODYSTRUCTURE")
	structure := hdr
	t.Logf("BODYSTRUCTURE: %s", structure)

	if !strings.Contains(structure, `"mixed"`) {
		t.Errorf("expected multipart/mixed, got %q", structure)
	}
	// Should have nested multipart/alternative
	if !strings.Contains(structure, `"alternative"`) {
		t.Errorf("expected nested multipart/alternative, got %q", structure)
	}
	// Should have pdf attachment
	if !strings.Contains(structure, `"pdf"`) {
		t.Errorf("expected application/pdf, got %q", structure)
	}
}

func TestE2EMixedAltFetchParts(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixedAlt())
	f.selectInbox()

	// Part 1: the multipart/alternative part (contains text/plain and text/html)
	_, lit1, _, _ := f.fetchCmd(1, "BODY[1]")
	part1 := string(lit1)
	t.Logf("Part 1: %q", part1)
	if !strings.Contains(part1, "multipart/alternative") {
		t.Errorf("part 1 should be multipart/alternative, got %q", part1)
	}

	// Part 1.1: text/plain within the alternative
	_, lit11, _, _ := f.fetchCmd(1, "BODY[1.1]")
	part11 := string(lit11)
	if !strings.Contains(part11, "text/plain") {
		t.Errorf("part 1.1 should be text/plain, got %q", part11)
	}
	if !strings.Contains(part11, "Please see the attached report") {
		t.Errorf("part 1.1 should have plain text body, got %q", part11)
	}

	// Part 1.2: text/html within the alternative
	_, lit12, _, _ := f.fetchCmd(1, "BODY[1.2]")
	part12 := string(lit12)
	if !strings.Contains(part12, "text/html") {
		t.Errorf("part 1.2 should be text/html, got %q", part12)
	}
	if !strings.Contains(part12, "<b>report</b>") {
		t.Errorf("part 1.2 should have HTML body, got %q", part12)
	}

	// Part 2: application/pdf attachment
	_, lit2, _, _ := f.fetchCmd(1, "BODY[2]")
	part2 := string(lit2)
	if !strings.Contains(part2, "application/pdf") {
		t.Errorf("part 2 should be application/pdf, got %q", part2)
	}
	if !strings.Contains(part2, "Content-Transfer-Encoding: base64") {
		t.Errorf("part 2 should have base64 encoding, got %q", part2)
	}
}

func TestE2EMixedAltEnvelope(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixedAlt())
	f.selectInbox()

	hdr, _, _, _ := f.fetchCmd(1, "ENVELOPE")
	envelope := hdr
	t.Logf("ENVELOPE: %s", envelope)

	// Subject should be present
	if !strings.Contains(envelope, `"Report with Attachment"`) {
		t.Errorf("envelope subject should contain Report with Attachment, got %q", envelope)
	}
	// From address
	if !strings.Contains(envelope, `"carol" "example.com"`) {
		t.Errorf("envelope from should contain carol@example.com, got %q", envelope)
	}
	// To addresses (2 recipients)
	if !strings.Contains(envelope, `"tony" "example.com"`) {
		t.Errorf("envelope to should contain tony@example.com, got %q", envelope)
	}
	if !strings.Contains(envelope, `"steve" "example.com"`) {
		t.Errorf("envelope to should contain steve@example.com, got %q", envelope)
	}
	// CC
	if !strings.Contains(envelope, `"bruce" "example.com"`) {
		t.Errorf("envelope cc should contain bruce@example.com, got %q", envelope)
	}
}

func TestE2EMixedAltSearch(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgMixedAlt())
	f.selectInbox()

	// SEARCH BODY should find text in plain text part
	lines := f.cmd(`SEARCH BODY "attached report"`)
	if !strings.Contains(lines[0], "SEARCH 1") {
		t.Errorf("SEARCH BODY should find plain text content, got %q", lines[0])
	}

	// SEARCH should find text in HTML part too
	lines2 := f.cmd(`SEARCH BODY "report"`)
	if !strings.Contains(lines2[0], "SEARCH 1") {
		t.Errorf("SEARCH BODY should find HTML content, got %q", lines2[0])
	}

	// SEARCH FROM
	lines3 := f.cmd(`SEARCH FROM "carol@example.com"`)
	if !strings.Contains(lines3[0], "SEARCH 1") {
		t.Errorf("SEARCH FROM should find sender, got %q", lines3[0])
	}

	// SEARCH TO
	lines4 := f.cmd(`SEARCH TO "tony@example.com"`)
	if !strings.Contains(lines4[0], "SEARCH 1") {
		t.Errorf("SEARCH TO should find recipient, got %q", lines4[0])
	}

	// SEARCH CC
	lines5 := f.cmd(`SEARCH CC "bruce@example.com"`)
	if !strings.Contains(lines5[0], "SEARCH 1") {
		t.Errorf("SEARCH CC should find CC recipient, got %q", lines5[0])
	}
}

func TestE2ERelatedBodyStructure(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgRelated())
	f.selectInbox()

	hdr, _, _, _ := f.fetchCmd(1, "BODYSTRUCTURE")
	structure := hdr
	t.Logf("BODYSTRUCTURE: %s", structure)

	if !strings.Contains(structure, `"related"`) {
		t.Errorf("expected multipart/related, got %q", structure)
	}
	if !strings.Contains(structure, `"html"`) {
		t.Errorf("expected text/html child, got %q", structure)
	}
	if !strings.Contains(structure, `"png"`) {
		t.Errorf("expected image/png child, got %q", structure)
	}
}

func TestE2ERelatedFetchParts(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msgRelated())
	f.selectInbox()

	// Part 1: text/html
	_, lit1, _, _ := f.fetchCmd(1, "BODY[1]")
	part1 := string(lit1)
	if !strings.Contains(part1, "text/html") {
		t.Errorf("part 1 should be text/html, got %q", part1)
	}
	if !strings.Contains(part1, "<img src=\"cid:photo.png\">") {
		t.Errorf("part 1 should contain HTML with CID reference, got %q", part1)
	}

	// Part 2: image/png inline
	_, lit2, _, _ := f.fetchCmd(1, "BODY[2]")
	part2 := string(lit2)
	if !strings.Contains(part2, "image/png") {
		t.Errorf("part 2 should be image/png, got %q", part2)
	}
	if !strings.Contains(part2, "Content-ID: <photo.png@cid>") {
		t.Errorf("part 2 should have Content-ID header, got %q", part2)
	}
	if !strings.Contains(part2, "inline") {
		t.Errorf("part 2 should have Content-Disposition: inline, got %q", part2)
	}
}

func TestE2EEmptyMailbox(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	f.selectInbox()

	// SEARCH on empty mailbox should return empty
	lines := f.cmd("SEARCH ALL")
	if !strings.Contains(lines[0], "SEARCH") {
		t.Errorf("SEARCH ALL on empty mailbox should return SEARCH, got %q", lines[0])
	}
	// Should not contain any numbers after "SEARCH"
	parts := strings.SplitN(lines[0], "SEARCH", 2)
	if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
		t.Errorf("SEARCH ALL on empty mailbox should have no results, got %q", lines[0])
	}
}

func TestE2EAppend(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	raw := msg("AppendRaw", "raw appended body")
	tag := f.nextTag()
	// LITERAL+ syntax: {N+} followed immediately by the data
	f.write(fmt.Sprintf("%s APPEND \"INBOX\" {%d+}\r\n%s\r\n", tag, len(raw), raw))
	resp := f.readLine()
	if !strings.HasPrefix(resp, tag+" OK APPEND") {
		t.Fatalf("APPEND failed, got %q", resp)
	}

	f.selectInbox()
	lines := f.cmd("FETCH 1 (FLAGS UID)")
	if !strings.HasPrefix(lines[0], "* 1 FETCH") {
		t.Fatalf("expected FETCH response, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "UID 1") {
		t.Errorf("FETCH should contain UID 1, got %q", lines[0])
	}
}

func TestE2EAppendWithFlagsAndDate(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	raw := msg("AppendFull", "full append")
	tag := f.nextTag()
	// APPEND with flags and date-time, using LITERAL+
	f.write(fmt.Sprintf("%s APPEND \"INBOX\" (\\Seen \\Draft) \"13-Jun-2026 00:00:00 -0500\" {%d+}\r\n%s\r\n",
		tag, len(raw), raw))
	resp := f.readLine()
	if !strings.HasPrefix(resp, tag+" OK APPEND") {
		t.Fatalf("APPEND with flags/date failed, got %q", resp)
	}

	f.selectInbox()
	lines := f.cmd("FETCH 1 (FLAGS)")
	if !strings.Contains(lines[0], "\\Seen") {
		t.Errorf("FETCH should contain \\Seen, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "\\Draft") {
		t.Errorf("FETCH should contain \\Draft, got %q", lines[0])
	}
}

func TestE2EAppendToSent(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	raw := msg("AppendSent", "sent folder")
	tag := f.nextTag()
	f.write(fmt.Sprintf("%s APPEND \"Sent\" {%d+}\r\n%s\r\n", tag, len(raw), raw))
	resp := f.readLine()
	if !strings.HasPrefix(resp, tag+" OK APPEND") {
		t.Fatalf("APPEND to Sent failed, got %q", resp)
	}

	tag2 := f.nextTag()
	f.write(tag2 + " SELECT Sent\r\n")
	var selectLast string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, tag2+" ") || strings.HasPrefix(l, tag2+"[") {
			selectLast = l
			break
		}
	}
	if !strings.Contains(selectLast, "OK") {
		t.Fatalf("SELECT Sent failed, got %q", selectLast)
	}

	lines := f.cmd("FETCH 1 (FLAGS)")
	if !strings.HasPrefix(lines[0], "* 1 FETCH") {
		t.Fatalf("expected message in Sent, got %q", lines[0])
	}
}

func TestE2ECopy(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")
	storeMsg(msg("CopyE2E", "body"))
	f.selectInbox()

	lines := f.cmd("COPY 1 Sent")
	last := lines[len(lines)-1]
	if !strings.Contains(last, "OK COPY") {
		t.Fatalf("COPY failed, got %q", last)
	}

	tag := f.nextTag()
	f.write(tag + " SELECT Sent\r\n")
	var selectLast string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, tag+" ") || strings.HasPrefix(l, tag+"[") {
			selectLast = l
			break
		}
	}
	if !strings.Contains(selectLast, "OK") {
		t.Fatalf("SELECT Sent failed, got %q", selectLast)
	}

	lines2 := f.cmd("FETCH 1 (FLAGS)")
	if !strings.HasPrefix(lines2[0], "* 1 FETCH") {
		t.Fatalf("expected copied message in Sent, got %q", lines2[0])
	}
}

// TestE2EImaptestCommands exercises the exact command patterns that imaptest
// uses, to ensure they all receive a proper tagged response.
func TestE2EImaptestCommands(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	// Seed several messages so sequence ranges are valid.
	for i := 0; i < 5; i++ {
		storeMsg(msg(fmt.Sprintf("M%d", i), fmt.Sprintf("body%d", i)))
	}

	// STATUS (this previously loaded the whole mailbox and could hang).
	lines := f.cmd("STATUS INBOX (MESSAGES UNSEEN RECENT)")
	if !strings.Contains(lines[len(lines)-1], "OK STATUS") {
		t.Fatalf("STATUS failed, got %q", lines[len(lines)-1])
	}
	if !strings.Contains(lines[0], "MESSAGES") {
		t.Errorf("STATUS response missing MESSAGES, got %q", lines[0])
	}

	// SELECT and various FETCH forms used by imaptest.
	f.selectInbox()

	fetchCmds := []string{
		"FETCH 1:3,5 (RFC822.SIZE UID BODY.PEEK[HEADER.FIELDS (Subject From)] BODY.PEEK[HEADER.FIELDS (Cc References From)])",
		"FETCH 1:3,5 (RFC822.SIZE BODYSTRUCTURE BODY.PEEK[HEADER.FIELDS (Cc References From)])",
		"FETCH 1:3,5 (BODY.PEEK[HEADER.FIELDS (In-Reply-To Subject Message-ID)] ENVELOPE)",
		"FETCH 1:3,5 (INTERNALDATE INTERNALDATE BODY.PEEK[HEADER.FIELDS (From)] BODY)",
		"FETCH 1 (UID RFC822.HEADER)",
		"FETCH 1:3,5 (INTERNALDATE BODY.PEEK[HEADER.FIELDS (References Cc Message-ID)] BODY.PEEK[HEADER.FIELDS (In-Reply-To In-Reply-To Cc In-Reply-To)])",
	}
	for _, cmd := range fetchCmds {
		tag := f.nextTag()
		f.write(tag + " " + cmd + "\r\n")
		var last string
		for {
			l := f.readLine()
			if strings.HasPrefix(l, tag+" ") || strings.HasPrefix(l, tag+"[") {
				last = l
				break
			}
		}
		if !strings.Contains(last, "OK FETCH") {
			t.Fatalf("%s failed, got %q", cmd, last)
		}
	}

	// STORE +FLAGS and FLAGS.SILENT.
	lines2 := f.cmd("STORE 1:3 +FLAGS (\\Deleted)")
	if !strings.Contains(lines2[len(lines2)-1], "OK STORE") {
		t.Fatalf("STORE +FLAGS failed, got %q", lines2[len(lines2)-1])
	}
	lines3 := f.cmd("STORE 1:2 +FLAGS.SILENT (\\Answered \\Draft)")
	if !strings.Contains(lines3[len(lines3)-1], "OK STORE") {
		t.Fatalf("STORE +FLAGS.SILENT failed, got %q", lines3[len(lines3)-1])
	}

	// EXPUNGE.
	lines4 := f.cmd("EXPUNGE")
	if !strings.Contains(lines4[len(lines4)-1], "OK EXPUNGE") {
		t.Fatalf("EXPUNGE failed, got %q", lines4[len(lines4)-1])
	}
}

func TestE2EAppendPreservesInternalDate(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	raw := msg("DateTest", "internal date test")
	tag := f.nextTag()
	// APPEND with a specific date-time
	f.write(fmt.Sprintf("%s APPEND \"INBOX\" (\\Seen) \"15-Jun-2026 10:30:00 +0000\" {%d+}\r\n%s\r\n",
		tag, len(raw), raw))
	resp := f.readLine()
	if !strings.HasPrefix(resp, tag+" OK APPEND") {
		t.Fatalf("APPEND failed, got %q", resp)
	}

	f.selectInbox()
	lines := f.cmd("FETCH 1 (FLAGS UID INTERNALDATE)")
	fetch := lines[0]
	if !strings.HasPrefix(fetch, "* 1 FETCH") {
		t.Fatalf("expected FETCH response, got %q", fetch)
	}
	// Verify the date portion (day, month, year) matches, ignoring timezone
	// since the server formats INTERNALDATE in its local timezone.
	if !strings.Contains(fetch, "INTERNALDATE \"15-Jun-2026") {
		t.Errorf("INTERNALDATE should be 15-Jun-2026, got %q", fetch)
	}
}

// TestE2EMove tests MOVE from INBOX to Trash and back.
func TestE2EMove(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	// Store a message in INBOX
	storeMsg(msg("MoveMe", "move body"))
	f.selectInbox()

	// Verify 1 message
	lines := f.cmd("FETCH 1 (FLAGS)")
	if !strings.HasPrefix(lines[0], "* 1 FETCH") {
		t.Fatalf("expected 1 message in INBOX, got %q", lines[0])
	}

	// MOVE to Trash
	moveLines := f.cmd("MOVE 1 Trash")
	last := moveLines[len(moveLines)-1]
	if !strings.Contains(last, "OK MOVE") {
		t.Fatalf("MOVE failed, got %q", last)
	}

	// Verify INBOX is now empty
	tag := f.nextTag()
	f.write(tag + " SELECT INBOX\r\n")
	var existsCount string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			existsCount = l
		}
		if strings.HasPrefix(l, tag+" ") {
			break
		}
	}
	if !strings.Contains(existsCount, "* 0 EXISTS") {
		t.Errorf("expected 0 EXISTS in INBOX after MOVE, got %q", existsCount)
	}

	// SELECT Trash and verify 1 message
	tag2 := f.nextTag()
	f.write(tag2 + " SELECT Trash\r\n")
	var trashExists string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			trashExists = l
		}
		if strings.HasPrefix(l, tag2+" ") {
			break
		}
	}
	if !strings.Contains(trashExists, "* 1 EXISTS") {
		t.Errorf("expected 1 EXISTS in Trash after MOVE, got %q", trashExists)
	}

	// MOVE back to INBOX
	moveLines2 := f.cmd("MOVE 1 INBOX")
	last2 := moveLines2[len(moveLines2)-1]
	if !strings.Contains(last2, "OK MOVE") {
		t.Fatalf("MOVE back failed, got %q", last2)
	}

	// Verify INBOX has 1 message again
	tag3 := f.nextTag()
	f.write(tag3 + " SELECT INBOX\r\n")
	var backExists string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			backExists = l
		}
		if strings.HasPrefix(l, tag3+" ") {
			break
		}
	}
	if !strings.Contains(backExists, "* 1 EXISTS") {
		t.Errorf("expected 1 EXISTS in INBOX after MOVE back, got %q", backExists)
	}
}

// TestE2EMoveMulti tests moving multiple messages at once.
func TestE2EMoveMulti(t *testing.T) {
	f := newIMAPTest(t)
	defer f.close()
	f.login("u", "pass")

	// Store 3 messages
	for i := 0; i < 3; i++ {
		storeMsg(msg(fmt.Sprintf("M%d", i), fmt.Sprintf("body%d", i)))
	}
	f.selectInbox()

	// Verify 3 messages
	lines := f.cmd("FETCH 1:3 (FLAGS)")
	if !strings.HasPrefix(lines[0], "* 1 FETCH") {
		t.Fatalf("expected 3 messages in INBOX, got %q", lines[0])
	}

	// MOVE all to Sent
	moveLines := f.cmd("MOVE 1:3 Sent")
	last := moveLines[len(moveLines)-1]
	if !strings.Contains(last, "OK MOVE") {
		t.Fatalf("MOVE multi failed, got %q", last)
	}

	// Verify INBOX is empty
	tag := f.nextTag()
	f.write(tag + " SELECT INBOX\r\n")
	var existsCount string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			existsCount = l
		}
		if strings.HasPrefix(l, tag+" ") {
			break
		}
	}
	if !strings.Contains(existsCount, "* 0 EXISTS") {
		t.Errorf("expected 0 EXISTS in INBOX after multi MOVE, got %q", existsCount)
	}

	// Verify Sent has 3
	tag2 := f.nextTag()
	f.write(tag2 + " SELECT Sent\r\n")
	var sentExists string
	for {
		l := f.readLine()
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			sentExists = l
		}
		if strings.HasPrefix(l, tag2+" ") {
			break
		}
	}
	if !strings.Contains(sentExists, "* 3 EXISTS") {
		t.Errorf("expected 3 EXISTS in Sent after multi MOVE, got %q", sentExists)
	}
}

// TestE2ECrossUserSearchIsolation verifies that IMAP SEARCH returns only
// the current user's messages, not messages belonging to other users.
func TestE2ECrossUserSearchIsolation(t *testing.T) {
	f := newIMAPTestMulti(t, []string{"u", "v"})
	defer f.close()

	// Store messages for user "u"
	storeMsg(msg("u-subj", "u body"))
	storeMsg(msg("v-subj", "v body"))

	// Connect as user "u", SELECT INBOX
	f.writeCmd("a001", "LOGIN u pass")
	f.readUntil("a001 ")
	f.writeCmd("a002", "SELECT INBOX")
	f.readUntil("a002 ")

	// SEARCH SUBJECT should find only u's message
	lines := f.cmd(`SEARCH SUBJECT "u-subj"`)
	last := lines[len(lines)-1]
	if !strings.Contains(last, "OK SEARCH") {
		t.Fatalf("expected OK SEARCH, got: %s", last)
	}
	for _, l := range lines {
		if strings.Contains(l, "v-subj") {
			t.Errorf("user u should NOT see user v's messages in SEARCH, got: %s", l)
		}
	}

	// Connect as user "v"
	f.writeCmdV("b001", "LOGIN v pass")
	f.readUntilV("b001 ")
	f.writeCmdV("b002", "SELECT INBOX")
	f.readUntilV("b002 ")

	// SEARCH SUBJECT should find only v's message
	lines2 := f.cmdV(`SEARCH SUBJECT "v-subj"`)
	last2 := lines2[len(lines2)-1]
	if !strings.Contains(last2, "OK SEARCH") {
		t.Fatalf("expected OK SEARCH, got: %s", last2)
	}
	for _, l := range lines2 {
		if strings.Contains(l, "u-subj") {
			t.Errorf("user v should NOT see user u's messages in SEARCH, got: %s", l)
		}
	}
}

// TestE2ESharedVsIMAPUserIsolation verifies that shared messages (UserID="")
// are not visible to IMAP users, and IMAP users only see their own messages.
func TestE2ESharedVsIMAPUserIsolation(t *testing.T) {
	f := newIMAPTestMulti(t, []string{"u", "v"})
	defer f.close()

	// Store messages for user "u", user "v", and shared (no user)
	storeMsg(msg("u-msg", "u body"))
	storeMsg(msg("v-msg", "v body"))
	storeMsg(msg("shared-msg", "shared body"))

	// Connect as user "u"
	f.writeCmd("a001", "LOGIN u pass")
	f.readUntil("a001 ")
	f.writeCmd("a002", "SELECT INBOX")
	f.readUntil("a002 ")

	// user "u" should see only 1 message (own), not 2 (not user v's or shared)
	lines := f.cmd("FETCH 1 (BODY[HEADER.FIELDS (SUBJECT)])")
	if lines[0] != "" && strings.Contains(lines[0], "v-msg") {
		t.Errorf("user u should NOT see user v's messages")
	}
	if lines[0] != "" && strings.Contains(lines[0], "shared-msg") {
		t.Errorf("user u should NOT see shared messages")
	}

	// Connect as user "v"
	f.writeCmdV("b001", "LOGIN v pass")
	f.readUntilV("b001 ")
	f.writeCmdV("b002", "SELECT INBOX")
	f.readUntilV("b002 ")

	// user "v" should see only 1 message (own)
	lines2 := f.cmdV("FETCH 1 (BODY[HEADER.FIELDS (SUBJECT)])")
	if lines2[0] != "" && strings.Contains(lines2[0], "u-msg") {
		t.Errorf("user v should NOT see user u's messages")
	}
	if lines2[0] != "" && strings.Contains(lines2[0], "shared-msg") {
		t.Errorf("user v should NOT see shared messages")
	}
}

// ---------------------------------------------------------------------------
// multi-user test fixture
// ---------------------------------------------------------------------------

type imapTestMulti struct {
	*imapTest
	userB *imapTest
}

func newIMAPTestMulti(t *testing.T, users []string) *imapTestMulti {
	logger.NoLogging = true
	config.MaxMessages = 0

	root, err := os.MkdirTemp("", "mailpit-imap-e2e-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	pwh := sha256.Sum256([]byte("pass"))
	userMap := map[string]config.ImapUserConfig{}
	yml := "users:\n"
	for _, u := range users {
		yml += fmt.Sprintf("  %s:\n    password-hash: %x\n", u, pwh)
		userMap[u] = config.ImapUserConfig{PasswordHash: fmt.Sprintf("%x", pwh)}
	}

	cfgPath := filepath.Join(root, "imap.yaml")
	if err := os.WriteFile(cfgPath, []byte(yml), 0644); err != nil {
		os.RemoveAll(root)
		t.Fatalf("WriteFile: %v", err)
	}

	config.IMAPConfigFile = cfgPath
	config.IMAPConfig = config.ImapConfigStruct{Users: userMap}
	config.Database = filepath.Join(root, "mailpit.db")

	if err := storage.InitDB(); err != nil {
		os.RemoveAll(root)
		t.Fatalf("InitDB: %v", err)
	}

	// First connection (user A)
	clientA, serverA := net.Pipe()
	go handleClient(serverA)
	readerA := bufio.NewReader(clientA)
	g, _ := readerA.ReadString('\n')
	if !strings.Contains(g, "OK") {
		clientA.Close()
		serverA.Close()
		storage.Close()
		os.RemoveAll(root)
		t.Fatalf("bad greeting: %q", g)
	}

	// Second connection (user B)
	clientB, serverB := net.Pipe()
	go handleClient(serverB)
	readerB := bufio.NewReader(clientB)
	g2, _ := readerB.ReadString('\n')
	if !strings.Contains(g2, "OK") {
		clientA.Close()
		serverA.Close()
		clientB.Close()
		serverB.Close()
		storage.Close()
		os.RemoveAll(root)
		t.Fatalf("bad greeting: %q", g2)
	}

	a := &imapTest{t: t, client: clientA, server: serverA, reader: readerA, rootDir: root, dbPath: config.Database, cfgPath: cfgPath}
	b := &imapTest{t: t, client: clientB, server: serverB, reader: readerB}

	return &imapTestMulti{imapTest: a, userB: b}
}

func (f *imapTestMulti) close() {
	f.userB.client.Close()
	f.userB.server.Close()
	f.imapTest.close()
}

// writeCmd sends a tagged command on user A's connection.
func (f *imapTest) writeCmd(tag, cmd string) {
	f.write(tag + " " + cmd + "\r\n")
}

// writeCmdV sends a tagged command on user B's connection.
func (f *imapTestMulti) writeCmdV(tag, cmd string) {
	f.userB.write(tag + " " + cmd + "\r\n")
}

// readUntilV reads lines until one starts with prefix on user B's connection.
func (f *imapTestMulti) readUntilV(prefix string) []string {
	var lines []string
	for {
		l := f.userB.readLine()
		lines = append(lines, l)
		if strings.HasPrefix(l, prefix) {
			return lines
		}
	}
}

// cmdV sends a command with auto-incrementing tag on user B's connection.
func (f *imapTestMulti) cmdV(line string) []string {
	tag := f.userB.nextTag()
	f.userB.write(tag + " " + line + "\r\n")
	return f.readUntilV(tag + " ")
}
