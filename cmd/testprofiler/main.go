package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// --- Configuration ---

type config struct {
	server  string
	smtp    string
	imap    string
	api     string
	timeout time.Duration
}

// --- IMAP connection wrapper ---

type imapConn struct {
	conn   net.Conn
	reader *bufio.Reader
	mu     sync.Mutex // guards writes
}

func imapConnect(addr string, timeout time.Duration) (*imapConn, error) {
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	reader := bufio.NewReader(conn)
	// Read server greeting
	line, err := reader.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("read greeting: %w", err)
	}
	if !strings.Contains(line, "OK") {
		conn.Close()
		return nil, fmt.Errorf("bad greeting: %q", line)
	}
	return &imapConn{conn: conn, reader: reader}, nil
}

func (c *imapConn) send(tag, cmd string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, cmd); err != nil {
		return nil, fmt.Errorf("send %s: %w", tag, err)
	}
	return c.readUntil(tag)
}

// readUntil reads lines until a line starts with the given prefix.
func (c *imapConn) readUntil(prefix string) ([]string, error) {
	var lines []string
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return lines, fmt.Errorf("read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		lines = append(lines, line)
		if strings.HasPrefix(line, prefix) {
			return lines, nil
		}
	}
}

// readLine reads a single line.
func (c *imapConn) readLine() (string, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *imapConn) close() {
	fmt.Fprintf(c.conn, "a000 LOGOUT\r\n")
	c.conn.Close()
}

func (c *imapConn) login(user, pass string) error {
	lines, err := c.send("a001", fmt.Sprintf("LOGIN %s %s", user, pass))
	if err != nil {
		return err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "a001 ") {
			if strings.Contains(l, "OK") {
				return nil
			}
			return fmt.Errorf("login failed: %s", l)
		}
	}
	return fmt.Errorf("no tagged response for LOGIN")
}

func (c *imapConn) selectMailbox(name string) (uint32, error) {
	lines, err := c.send("a002", fmt.Sprintf("SELECT %q", name))
	if err != nil {
		return 0, err
	}
	var exists uint32
	for _, l := range lines {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "EXISTS") {
			// Parse "* N EXISTS"
			n := uint32(0)
			fmt.Sscanf(l, "* %d EXISTS", &n)
			exists = n
		}
		if strings.HasPrefix(l, "a002 ") {
			if strings.Contains(l, "OK") {
				return exists, nil
			}
			return 0, fmt.Errorf("SELECT %s failed: %s", name, l)
		}
	}
	return 0, fmt.Errorf("no tagged response for SELECT")
}

// idleStart sends IDLE and reads the "+ idling" continuation response.
// Returns a channel that delivers untagged response lines.
func (c *imapConn) idleStart(tag string, ch chan<- string, done <-chan struct{}) error {
	c.mu.Lock()
	if _, err := fmt.Fprintf(c.conn, "%s IDLE\r\n", tag); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("send IDLE: %w", err)
	}
	c.mu.Unlock()

	// Read continuation response "+ idling"
	line, err := c.readLine()
	if err != nil {
		return fmt.Errorf("read IDLE continuation: %w", err)
	}
	if !strings.HasPrefix(line, "+") {
		return fmt.Errorf("expected IDLE continuation, got: %q", line)
	}

	// Read untagged responses in a goroutine
	go func() {
		defer close(ch)
		for {
			select {
			case <-done:
				return
			default:
			}
			c.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			line, err := c.reader.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				continue
			}
			// Stop if we get the tagged response (IDLE ended by server)
			if strings.HasPrefix(line, tag+" ") {
				return
			}
			ch <- line
		}
	}()
	return nil
}

// idleDone sends DONE to end the IDLE state and reads the tagged OK.
func (c *imapConn) idleDone(tag string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := fmt.Fprintf(c.conn, "DONE\r\n"); err != nil {
		return fmt.Errorf("send DONE: %w", err)
	}
	// Read until tagged response
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read IDLE done: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, tag+" ") {
			if strings.Contains(line, "OK") {
				return nil
			}
			return fmt.Errorf("IDLE done failed: %s", line)
		}
	}
}

// fetchEnvelope fetches the envelope of message at sequence number seq.
func (c *imapConn) fetchSubject(seq uint32) (string, error) {
	lines, err := c.send("a010", fmt.Sprintf("FETCH %d (BODY[HEADER.FIELDS (SUBJECT)])", seq))
	if err != nil {
		return "", err
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "FETCH") {
			// Extract subject from the literal or parenthesized response
			return l, nil
		}
	}
	return "", nil
}

// expungeAll deletes all messages in the currently selected mailbox.
func (c *imapConn) expungeAll() error {
	// First, STORE all messages with +FLAGS (\Deleted)
	_, err := c.send("a020", "STORE 1:* +FLAGS (\\Deleted)")
	if err != nil {
		return err
	}
	_, err = c.send("a021", "EXPUNGE")
	return err
}

// --- SMTP sender ---

func sendSMTP(addr, from, to, subject, body string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Errorf("smtp connect: %w", err)
	}
	defer conn.Close()
	reader := bufio.NewReader(conn)

	// Read greeting
	if _, err := reader.ReadString('\n'); err != nil {
		return fmt.Errorf("smtp greeting: %w", err)
	}

	// EHLO
	if _, err := fmt.Fprintf(conn, "EHLO profiler.local\r\n"); err != nil {
		return err
	}
	if err := readSMTPResponse(reader); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}

	// MAIL FROM
	if _, err := fmt.Fprintf(conn, "MAIL FROM:<%s>\r\n", from); err != nil {
		return err
	}
	if err := readSMTPResponse(reader); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}

	// RCPT TO
	if _, err := fmt.Fprintf(conn, "RCPT TO:<%s>\r\n", to); err != nil {
		return err
	}
	if err := readSMTPResponse(reader); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}

	// DATA
	if _, err := fmt.Fprintf(conn, "DATA\r\n"); err != nil {
		return err
	}
	// Read 354 response
	if _, err := reader.ReadString('\n'); err != nil {
		return fmt.Errorf("DATA response: %w", err)
	}

	// Message
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: <%s@profiler.local>\r\nContent-Type: text/plain\r\n\r\n%s",
		from, to, subject, time.Now().UTC().Format(time.RFC1123Z), uuid.New().String(), body)
	if _, err := fmt.Fprintf(conn, "%s\r\n.\r\n", msg); err != nil {
		return err
	}
	if err := readSMTPResponse(reader); err != nil {
		return fmt.Errorf("DATA body: %w", err)
	}

	// QUIT
	if _, err := fmt.Fprintf(conn, "QUIT\r\n"); err != nil {
		return err
	}
	// Don't check QUIT response — server may close connection
	return nil
}

func readSMTPResponse(reader *bufio.Reader) error {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		// SMTP multi-line: first 3 chars + '-' means more lines follow
		if len(line) >= 4 && line[3] == '-' {
			continue
		}
		// Single-line response: check for 2xx/3xx
		if len(line) >= 1 && (line[0] == '2' || line[0] == '3') {
			return nil
		}
		return fmt.Errorf("smtp error: %s", line)
	}
}

// --- API helpers ---

type apiUser struct {
	Username string `json:"username"`
}

func createUser(baseURL, username, password string) error {
	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, username, password)
	resp, err := http.Post(baseURL+"/api/v1/imap/users", "application/json", strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("create user %s: %w", username, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("create user %s: HTTP %d: %s", username, resp.StatusCode, string(b))
	}
	return nil
}

func deleteUser(baseURL, username string) error {
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/imap/users?username="+username, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete user %s: %w", username, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete user %s: HTTP %d: %s", username, resp.StatusCode, string(b))
	}
	return nil
}

func deleteAllMessages(baseURL string) error {
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/v1/messages", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete all messages: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete all messages: HTTP %d: %s", resp.StatusCode, string(b))
	}
	return nil
}

// --- Main ---

func main() {
	cfg := config{}
	flag.StringVar(&cfg.server, "server", "172.18.0.22", "Mailpit server address")
	flag.StringVar(&cfg.smtp, "smtp-port", "1025", "SMTP port")
	flag.StringVar(&cfg.imap, "imap-port", "1143", "IMAP port")
	flag.StringVar(&cfg.api, "api-port", "8025", "HTTP API port")
	flag.DurationVar(&cfg.timeout, "timeout", 10*time.Second, "connection timeout")
	flag.Parse()

	baseURL := fmt.Sprintf("http://%s:%s", cfg.server, cfg.api)
	smtpAddr := net.JoinHostPort(cfg.server, cfg.smtp)
	imapAddr := net.JoinHostPort(cfg.server, cfg.imap)

	// Generate unique users
	suffix := uuid.New().String()[:8]
	senderUser := fmt.Sprintf("profiler-%s-sender@mp-test.local", suffix)
	receiverUser := fmt.Sprintf("profiler-%s-receiver@mp-test.local", suffix)
	password := "profiler-test-pass"

	fmt.Println("Mailpit IDLE Profiler")
	fmt.Println("=====================")
	fmt.Printf("Sender:   %s\n", senderUser)
	fmt.Printf("Receiver: %s\n", receiverUser)
	fmt.Println()

	// Cleanup on exit
	defer func() {
		fmt.Println("\nCleanup...")
		// Delete all messages first (including Sent copies), then users
		if err := deleteAllMessages(baseURL); err != nil {
			fmt.Printf("  warn: delete messages: %v\n", err)
		}
		if err := deleteUser(baseURL, senderUser); err != nil {
			fmt.Printf("  warn: delete sender: %v\n", err)
		}
		if err := deleteUser(baseURL, receiverUser); err != nil {
			fmt.Printf("  warn: delete receiver: %v\n", err)
		}
		fmt.Println("  done")
	}()

	// Create users
	fmt.Println("Creating IMAP users...")
	if err := createUser(baseURL, senderUser, password); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}
	if err := createUser(baseURL, receiverUser, password); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: %v\n", err)
		os.Exit(1)
	}

	// Connect receiver IMAP → SELECT INBOX → IDLE
	fmt.Println("Connecting receiver IMAP (INBOX IDLE)...")
	receiverConn, err := imapConnect(imapAddr, cfg.timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: receiver connect: %v\n", err)
		os.Exit(1)
	}
	defer receiverConn.close()
	if err := receiverConn.login(receiverUser, password); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: receiver login: %v\n", err)
		os.Exit(1)
	}
	receiverCount, err := receiverConn.selectMailbox("INBOX")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: receiver SELECT: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  INBOX has %d messages\n", receiverCount)

	// Connect sender Sent IMAP → SELECT Sent → IDLE
	fmt.Println("Connecting sender IMAP (Sent IDLE)...")
	senderSentConn, err := imapConnect(imapAddr, cfg.timeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: sender Sent connect: %v\n", err)
		os.Exit(1)
	}
	defer senderSentConn.close()
	if err := senderSentConn.login(senderUser, password); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: sender Sent login: %v\n", err)
		os.Exit(1)
	}
	senderSentCount, err := senderSentConn.selectMailbox("Sent")
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: sender Sent SELECT: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  Sent has %d messages\n", senderSentCount)

	// Start IDLE on receiver INBOX
	receiverIdleCh := make(chan string, 32)
	receiverIdleDone := make(chan struct{})
	defer close(receiverIdleDone)
	if err := receiverConn.idleStart("i001", receiverIdleCh, receiverIdleDone); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: receiver IDLE start: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  Receiver IDLE started")

	// Start IDLE on sender Sent
	senderIdleCh := make(chan string, 32)
	senderIdleDone := make(chan struct{})
	defer close(senderIdleDone)
	if err := senderSentConn.idleStart("i002", senderIdleCh, senderIdleDone); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: sender Sent IDLE start: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  Sender Sent IDLE started")

	// Send email
	subject := fmt.Sprintf("Profiler test %s", suffix)
	fmt.Println("\nSending email via SMTP...")
	t0 := time.Now()
	if err := sendSMTP(smtpAddr, senderUser, receiverUser, subject, "Profiler test body"); err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: SMTP send: %v\n", err)
		os.Exit(1)
	}
	smtpSent := time.Since(t0)
	fmt.Printf("  SMTP completed in %v\n", smtpSent)

	// Wait for receiver INBOX IDLE notification
	fmt.Println("\nWaiting for IDLE notifications...")
	inboxLatency := time.Duration(0)
	sentLatency := time.Duration(0)
	inboxGot := false
	sentGot := false
	deadline := time.After(cfg.timeout)

	for !inboxGot || !sentGot {
		select {
		case line, ok := <-receiverIdleCh:
			if !ok {
				fmt.Println("  receiver IDLE channel closed")
				inboxGot = true // won't get it
				continue
			}
			if strings.Contains(line, "EXISTS") && !inboxGot {
				inboxLatency = time.Since(t0)
				inboxGot = true
				fmt.Printf("  Receiver INBOX notification: %s (latency: %v)\n", line, inboxLatency)
			}
		case line, ok := <-senderIdleCh:
			if !ok {
				fmt.Println("  sender Sent IDLE channel closed")
				sentGot = true // won't get it
				continue
			}
			if strings.Contains(line, "EXISTS") && !sentGot {
				sentLatency = time.Since(t0)
				sentGot = true
				fmt.Printf("  Sender Sent notification: %s (latency: %v)\n", line, sentLatency)
			}
		case <-deadline:
			fmt.Println("\nTimeout waiting for IDLE notifications")
			goto results
		}
	}

results:
	// End IDLE
	receiverConn.idleDone("i001")
	senderSentConn.idleDone("i002")

	fmt.Println("\n=====================")
	fmt.Println("Results")
	fmt.Println("=====================")
	if inboxGot {
		fmt.Printf("Inbox latency:  %v\n", inboxLatency)
	} else {
		fmt.Println("Inbox latency:  TIMEOUT")
	}
	if sentGot {
		fmt.Printf("Sent latency:   %v\n", sentLatency)
	} else {
		fmt.Println("Sent latency:   TIMEOUT")
	}
}
