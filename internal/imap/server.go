// Package imap is a simple IMAP server for Mailpit.
// References: RFC 3501 - INTERNET MESSAGE ACCESS PROTOCOL - VERSION 4rev1
package imap

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
)

// idleConns tracks connections currently in the IDLE state for diagnostics.
var idleConns sync.Map

// Diagnostic counters for debugging stalls.
var (
	inFlight  atomic.Int32
	noopCount atomic.Int64
	cmdCount  atomic.Int64
	connCount atomic.Int32
)

func init() {
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			logger.Log().Debugf("[imap] DIAGNOSTIC: in-flight=%d conn=%d cmds=%d noops=%d",
				inFlight.Load(), connCount.Load(), cmdCount.Load(), noopCount.Load())
		}
	}()
}

const (
	// IMAP capability string
	imapCapabilities = "IMAP4rev1 AUTH=PLAIN LITERAL+ NAMESPACE IDLE CHILDREN MOVE"
)

// Run will start the IMAP server if configured
func Run() {
	if config.IMAPConfigFile == "" {
		return
	}

	var listener net.Listener
	var err error

	if config.IMAPTLSCert != "" {
		cer, err2 := tls.LoadX509KeyPair(config.IMAPTLSCert, config.IMAPTLSKey)
		if err2 != nil {
			logger.Log().Errorf("[imap] %s", err2.Error())
			return
		}

		tlsConfig := &tls.Config{
			Certificates: []tls.Certificate{cer},
			MinVersion:   tls.VersionTLS12,
		}

		listener, err = tls.Listen("tcp", config.IMAPListen, tlsConfig)
	} else {
		listener, err = net.Listen("tcp", config.IMAPListen)
	}

	if err != nil {
		logger.Log().Errorf("[imap] %s", err.Error())
		return
	}

	logger.Log().Infof("[imap] starting on %s", config.IMAPListen)

	for {
		conn, err := listener.Accept()
		if err != nil {
			logger.Log().Errorf("[imap] accept error: %s", err.Error())
			continue
		}

		go handleClient(conn)
	}
}

type imapMessage struct {
	ID      string
	UID     uint64
	Size    uint64
	Flags   []string
	Created time.Time
}

type clientState int

const (
	stateNotAuthenticated clientState = iota
	stateAuthenticated
	stateSelected
)

// standardMailbox defines a virtual IMAP folder with its special-use attribute.
type standardMailbox struct {
	Name       string
	SpecialUse string // RFC 6154 attribute, e.g. \Sent
}

// standardMailboxes lists the folders advertised by Mailpit for client compatibility.
// INBOX is always real; the rest are empty stubs so that IMAP clients can discover
// and auto-assign their roles (Sent, Trash, Drafts).
var standardMailboxes = []standardMailbox{
	{Name: "INBOX"},
	{Name: "Sent", SpecialUse: "\\Sent"},
	{Name: "Trash", SpecialUse: "\\Trash"},
	{Name: "Drafts", SpecialUse: "\\Drafts"},
}

// isStandardMailbox returns true if name matches a known standard folder (case-insensitive).
func isStandardMailbox(name string) bool {
	upper := strings.ToUpper(name)
	for _, m := range standardMailboxes {
		if strings.ToUpper(m.Name) == upper {
			return true
		}
	}
	return false
}

type mailbox struct {
	Name     string
	User     string
	Messages []imapMessage
	NextUID  uint64
	Exists   int
	Recent   int
	Seen     int
	ReadOnly bool
}

func handleClient(conn net.Conn) {
	var (
		state = stateNotAuthenticated
		user  = ""
		mbox  *mailbox
	)

	defer func() {
		if err := conn.Close(); err != nil {
			logger.Log().Errorf("[imap] %s", err.Error())
		}
	}()

	reader := bufio.NewReader(conn)
	timeoutDuration := 600 * time.Second

	logger.Log().Debugf("[imap] connection opened by %s", conn.RemoteAddr().String())
	connCount.Add(1)
	defer connCount.Add(-1)

	sendResponse(conn, "* OK [CAPABILITY "+imapCapabilities+"] Mailpit IMAP server")

	for {
		if err := conn.SetReadDeadline(time.Now().Add(timeoutDuration)); err != nil {
			logger.Log().Errorf("[imap] %s", err.Error())
			return
		}

		rawLine, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				logger.Log().Debugf("[imap] client disconnected: %s", conn.RemoteAddr().String())
			} else {
				logger.Log().Errorf("[imap] read error: %s", err.Error())
			}
			return
		}

		rawLine = strings.TrimRight(rawLine, "\r\n")

		logger.Log().Debugf("[imap] received: %s (%s)", rawLine, conn.RemoteAddr().String())

		inFlight.Add(1)
		start := time.Now()
		if !handleCommand(conn, reader, rawLine, &state, &user, &mbox) {
			inFlight.Add(-1)
			return
		}
		elapsed := time.Since(start)
		inFlight.Add(-1)
		cmdCount.Add(1)

		if elapsed > 5*time.Millisecond {
			logger.Log().Debugf("[imap] SLOW COMMAND: %q from %s took %v (in-flight=%d)",
				rawLine, conn.RemoteAddr().String(), elapsed, inFlight.Load())
		}
	}
}

func handleCommand(conn net.Conn, reader *bufio.Reader, rawLine string, state *clientState, user *string, mbox **mailbox) bool {
	tag, cmd, args := parseImapCommand(rawLine)

	if tag == "" {
		return true
	}

	// Handle IMAP literals (e.g., {5} or {5+}) in arguments.
	// For non-LITERAL+ ({N}), the server must send a "+" continuation response
	// before reading literal data (RFC 3501 §7.5.1).
	// For LITERAL+ ({N+}), the client sends data immediately without waiting.
	for i, arg := range args {
		if isLiteralPattern(arg) {
			inner := arg[1 : len(arg)-1]
			isLiteralPlus := len(inner) > 0 && inner[len(inner)-1] == '+'
			if !isLiteralPlus {
				sendResponse(conn, "+")
			}
			args[i] = readLiteralData(arg, reader)
		}
	}

	// Merge parenthesized groups (e.g., (FLAGS UID) → single token)
	args = mergeParenGroups(args)

	switch strings.ToUpper(cmd) {
	case "CAPABILITY":
		sendResponse(conn, "* CAPABILITY "+imapCapabilities)
		sendResponse(conn, tag+" OK CAPABILITY completed")
	case "NOOP":
		noopCount.Add(1)
		// If a mailbox is selected, re-check the count and send EXISTS if changed.
		if *state == stateSelected && *mbox != nil {
			folder := ""
			upper := strings.ToUpper((*mbox).Name)
			if upper != "INBOX" {
				folder = (*mbox).Name
			}
			if total, _, _, err := storage.MailboxCounts(folder, *user); err == nil {
				if int(total) != (*mbox).Exists {
					(*mbox).Exists = int(total)
					(*mbox).NextUID = uint64(total + 1)
					sendResponse(conn, fmt.Sprintf("* %d EXISTS", total))
				}
			}
		}
		sendResponse(conn, tag+" OK NOOP completed")
	case "LOGOUT":
		sendResponse(conn, "* BYE Mailpit IMAP server logging out")
		sendResponse(conn, tag+" OK LOGOUT completed")
		return false
	case "AUTHENTICATE":
		if *state != stateNotAuthenticated {
			sendResponse(conn, tag+" BAD already authenticated")
			return true
		}
		if len(args) < 1 || strings.ToUpper(args[0]) != "PLAIN" {
			sendResponse(conn, tag+` BAD not supported`)
			return true
		}

		var authData string
		if len(args) >= 2 {
			authData = args[1]
		} else {
			sendResponse(conn, "+ ")
			rawLine, err := reader.ReadString('\n')
			if err != nil {
				return false
			}
			authData = strings.TrimRight(rawLine, "\r\n")
		}

		decoded, err := base64.StdEncoding.DecodeString(authData)
		if err != nil {
			sendResponse(conn, tag+" NO invalid authentication data")
			return true
		}

		parts := strings.SplitN(string(decoded), "\x00", 3)
		if len(parts) != 3 {
			sendResponse(conn, tag+" NO invalid authentication data")
			return true
		}

		username := parts[1]
		password := parts[2]

		if authenticateIMAP(username, password) {
			*state = stateAuthenticated
			*user = username
			_ = storage.RegisterIMAPUser(username)
			sendResponse(conn, tag+" OK AUTHENTICATE completed")
		} else {
			sendResponse(conn, tag+" NO AUTHENTICATE failed")
			logger.Log().Warnf("[imap] failed login: %s", username)
		}
	case "LOGIN":
		if *state != stateNotAuthenticated {
			sendResponse(conn, tag+" BAD already authenticated")
			return true
		}
		if len(args) < 2 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		username := args[0]
		password := args[1]

		if authenticateIMAP(username, password) {
			*state = stateAuthenticated
			*user = username
			_ = storage.RegisterIMAPUser(username)
			sendResponse(conn, tag+" OK LOGIN completed")
		} else {
			sendResponse(conn, tag+" NO LOGIN failed")
			logger.Log().Warnf("[imap] failed login: %s", username)
		}
	case "SELECT":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		if len(args) < 1 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		mailboxName := args[0]
		mb := openMailbox(mailboxName, *user, false)
		*mbox = &mb
		*state = stateSelected

		sendResponse(conn, fmt.Sprintf("* %d EXISTS", mb.Exists))
		sendResponse(conn, fmt.Sprintf("* %d RECENT", mb.Recent))
		sendResponse(conn, "* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)")
		sendResponse(conn, "* OK [PERMANENTFLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)] flags defined")
		sendResponse(conn, fmt.Sprintf("* OK [UIDNEXT %d] predicted next UID", mb.NextUID))
		sendResponse(conn, "* OK [UIDVALIDITY 1] UIDs valid")
		sendResponse(conn, tag+" OK [READ-WRITE] SELECT completed")
	case "EXAMINE":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		if len(args) < 1 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		mailboxName := args[0]
		mb := openMailbox(mailboxName, *user, true)
		*mbox = &mb
		*state = stateSelected

		sendResponse(conn, fmt.Sprintf("* %d EXISTS", mb.Exists))
		sendResponse(conn, fmt.Sprintf("* %d RECENT", mb.Recent))
		sendResponse(conn, "* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)")
		sendResponse(conn, "* OK [PERMANENTFLAGS ()] no permanent flags")
		sendResponse(conn, fmt.Sprintf("* OK [UIDNEXT %d] predicted next UID", mb.NextUID))
		sendResponse(conn, "* OK [UIDVALIDITY 1] UIDs valid")
		sendResponse(conn, tag+" OK [READ-ONLY] EXAMINE completed")
	case "CLOSE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		if !(*mbox).ReadOnly {
			deleteMarked(**mbox)
		}
		*state = stateAuthenticated
		**mbox = mailbox{}
		sendResponse(conn, tag+" OK CLOSE completed")
	case "STATUS":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		if len(args) < 1 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		mailboxName := strings.Trim(args[0], "\"")
		folder := ""
		upper := strings.ToUpper(mailboxName)
		if upper != "INBOX" && isStandardMailbox(mailboxName) {
			folder = mailboxName
		} else if upper != "INBOX" && !isStandardMailbox(mailboxName) {
			// Check if it's a custom mailbox
			if !storage.CustomMailboxExists(mailboxName, *user) {
				sendResponse(conn, tag+" NO mailbox not found")
				return true
			}
			folder = mailboxName
		}

		total, unread, nextUID, err := storage.MailboxCounts(folder, *user)
		if err != nil {
			logger.Log().Errorf("[imap] status count error: %s", err.Error())
			sendResponse(conn, tag+" NO status failed")
			return true
		}

		sendResponse(conn, fmt.Sprintf("* STATUS %s (MESSAGES %d UNSEEN %d UIDNEXT %d UIDVALIDITY 1)", mailboxName, total, unread, nextUID))
		sendResponse(conn, tag+" OK STATUS completed")
	case "LIST":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		wildcard := "*"
		if len(args) >= 1 {
			_ = args[0]
		}
		if len(args) >= 2 {
			wildcard = args[1]
		}
		wildcard = strings.Trim(wildcard, "\"")
		for _, mb := range standardMailboxes {
			if matchMailboxWildcard(wildcard, mb.Name) {
				if mb.SpecialUse != "" {
					sendResponse(conn, fmt.Sprintf(`* LIST (\HasNoChildren %s) "/" %s`, mb.SpecialUse, mb.Name))
				} else {
					sendResponse(conn, fmt.Sprintf(`* LIST (\HasNoChildren) "/" %s`, mb.Name))
				}
			}
		}
		// Also list custom mailboxes for the current user
		if *user != "" {
			customMailboxes, err := storage.GetCustomMailboxes(*user)
			if err == nil {
				for _, name := range customMailboxes {
					if matchMailboxWildcard(wildcard, name) {
						sendResponse(conn, fmt.Sprintf(`* LIST (\HasNoChildren) "/" %s`, name))
					}
				}
			}
		}
		sendResponse(conn, tag+" OK LIST completed")
	case "LSUB":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		for _, mb := range standardMailboxes {
			if mb.SpecialUse != "" {
				sendResponse(conn, fmt.Sprintf(`* LSUB (\HasNoChildren %s) "/" %s`, mb.SpecialUse, mb.Name))
			} else {
				sendResponse(conn, fmt.Sprintf(`* LSUB (\HasNoChildren) "/" %s`, mb.Name))
			}
		}
		// Also list custom mailboxes for the current user
		if *user != "" {
			customMailboxes, err := storage.GetCustomMailboxes(*user)
			if err == nil {
				for _, name := range customMailboxes {
					sendResponse(conn, fmt.Sprintf(`* LSUB (\HasNoChildren) "/" %s`, name))
				}
			}
		}
		sendResponse(conn, tag+" OK LSUB completed")
	case "FETCH":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleFetch(conn, tag, args, mbox, false)
	case "STORE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleStore(conn, tag, args, mbox, false)
	case "SEARCH":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleSearch(conn, tag, args, **mbox)
	case "UID":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleUID(conn, tag, args, mbox)
	case "COPY":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleCopy(conn, tag, args, **mbox)
	case "NAMESPACE":
		sendResponse(conn, `* NAMESPACE (("" "/")) NIL NIL`)
		sendResponse(conn, tag+" OK NAMESPACE completed")
	case "IDLE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		sendResponse(conn, "+ idling")

		addr := conn.RemoteAddr().String()
		idleConns.Store(addr, time.Now())
		logger.Log().Debugf("[imap] idle start: %s", addr)

		// Register with the idle notifier to receive push notifications.
		ic := storage.IdleNotify.RegisterIdle(conn, (*mbox).Name, *user)
		defer storage.IdleNotify.UnregisterIdle(ic)

		// Spawn a goroutine to forward notifications to the client.
		go func() {
			for {
				select {
				case <-ic.Done():
					return
				case response := <-ic.Notify():
					// net.Conn supports concurrent reads/writes, safe to write here.
					if _, err := fmt.Fprintf(conn, "%s\r\n", response); err != nil {
						logger.Log().Debugf("[imap] idle-notify write error for %s: %s", addr, err)
						return
					}
					logger.Log().Debugf("[imap] idle-notify sent to %s: %s", addr, response)
				}
			}
		}()

		sawDone := false
		lastHeartbeat := time.Now()
		for {
			if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
				logger.Log().Errorf("[imap] %s", err.Error())
				break
			}
			line, err := reader.ReadString('\n')
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					if time.Since(lastHeartbeat) >= 30*time.Second {
						logger.Log().Debugf("[imap] idle heartbeat: %s", addr)
						lastHeartbeat = time.Now()
					}
					continue
				}
				logger.Log().Debugf("[imap] idle read error for %s: %s", addr, err)
				break
			}
			line = strings.TrimRight(line, "\r\n")
			if strings.ToUpper(line) == "DONE" {
				sawDone = true
				break
			}
		}

		idleConns.Delete(addr)
		storage.IdleNotify.UnregisterIdle(ic)
		logger.Log().Debugf("[imap] idle end: %s (done=%v)", addr, sawDone)
		sendResponse(conn, tag+" OK IDLE completed")
	case "CHECK":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		sendResponse(conn, tag+" OK CHECK completed")
	case "SUBSCRIBE":
		sendResponse(conn, tag+" OK SUBSCRIBE completed")
	case "UNSUBSCRIBE":
		sendResponse(conn, tag+" OK UNSUBSCRIBE completed")
	case "APPEND":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		handleAppend(conn, tag, args, *user)
	case "EXPUNGE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		if !(*mbox).ReadOnly {
			expungeMarked(conn, mbox)
		}
		sendResponse(conn, tag+" OK EXPUNGE completed")
	case "CREATE":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		if len(args) < 1 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		mailboxName := strings.Trim(args[0], "\"")
		upper := strings.ToUpper(mailboxName)
		// Cannot create standard mailboxes or INBOX
		if isStandardMailbox(mailboxName) || upper == "INBOX" {
			sendResponse(conn, tag+" NO cannot create standard mailbox")
			return true
		}
		created, err := storage.CreateCustomMailbox(mailboxName, *user)
		if err != nil {
			logger.Log().Errorf("[imap] create mailbox error: %s", err.Error())
			sendResponse(conn, tag+" NO create failed")
			return true
		}
		if !created {
			sendResponse(conn, tag+" NO mailbox already exists")
			return true
		}
		sendResponse(conn, tag+" OK CREATE completed")
	case "DELETE":
		if *state == stateNotAuthenticated {
			sendResponse(conn, tag+" BAD not authenticated")
			return true
		}
		if len(args) < 1 {
			sendResponse(conn, tag+" BAD invalid arguments")
			return true
		}
		mailboxName := strings.Trim(args[0], "\"")
		if isStandardMailbox(mailboxName) || strings.EqualFold(mailboxName, "INBOX") {
			sendResponse(conn, tag+" NO cannot delete standard mailbox")
			return true
		}
		if !storage.CustomMailboxExists(mailboxName, *user) {
			sendResponse(conn, tag+" NO mailbox does not exist")
			return true
		}
		if err := storage.DeleteCustomMailbox(mailboxName, *user); err != nil {
			logger.Log().Errorf("[imap] delete mailbox error: %s", err.Error())
			sendResponse(conn, tag+" NO delete failed")
			return true
		}
		sendResponse(conn, tag+" OK DELETE completed")
	case "MOVE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleMove(conn, tag, args, mbox, false)
	default:
		sendResponse(conn, tag+" BAD unknown command: "+cmd)
	}

	return true
}

func parseImapCommand(line string) (tag, cmd string, args []string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", nil
	}

	parts := splitImapLine(line)
	if len(parts) < 2 {
		return "", "", nil
	}

	tag = parts[0]
	cmd = parts[1]
	if len(parts) > 2 {
		args = parts[2:]
	}

	return tag, cmd, args
}

func splitImapLine(line string) []string {
	var result []string
	current := strings.Builder{}
	inQuote := false
	escape := false

	for i := 0; i < len(line); i++ {
		c := line[i]

		if escape {
			current.WriteByte(c)
			escape = false
			continue
		}

		if c == '\\' && inQuote {
			escape = true
			current.WriteByte(c)
			continue
		}

		if c == '"' {
			inQuote = !inQuote
			current.WriteByte(c)
			continue
		}

		if c == ' ' && !inQuote {
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
			continue
		}

		current.WriteByte(c)
	}

	if current.Len() > 0 {
		result = append(result, current.String())
	}

	// Strip surrounding quotes from tokens
	for i, token := range result {
		if len(token) >= 2 && token[0] == '"' && token[len(token)-1] == '"' {
			result[i] = token[1 : len(token)-1]
		}
	}

	return result
}

// mergeParenGroups merges arguments that are grouped by parentheses
// into single tokens. For example, ["FLAGS", "(\\Seen", "\\Flagged)"] becomes
// ["FLAGS", "(\\Seen \\Flagged)"].
func mergeParenGroups(args []string) []string {
	var result []string
	var parenBuf strings.Builder
	inParens := false
	depth := 0

	for _, arg := range args {
		if !inParens {
			if strings.HasPrefix(arg, "(") {
				inParens = true
				depth = strings.Count(arg, "(") - strings.Count(arg, ")")
				parenBuf.WriteString(arg)
				if depth <= 0 {
					result = append(result, parenBuf.String())
					parenBuf.Reset()
					inParens = false
				}
			} else {
				result = append(result, arg)
			}
		} else {
			if parenBuf.Len() > 0 {
				parenBuf.WriteString(" ")
			}
			parenBuf.WriteString(arg)
			depth += strings.Count(arg, "(") - strings.Count(arg, ")")
			if depth <= 0 {
				result = append(result, parenBuf.String())
				parenBuf.Reset()
				inParens = false
			}
		}
	}

	// If we still have an unclosed paren group, just add it as-is
	if inParens {
		result = append(result, parenBuf.String())
	}

	return result
}

func isLiteralPattern(s string) bool {
	return len(s) > 2 && s[0] == '{' && s[len(s)-1] == '}'
}

func readLiteralData(s string, reader *bufio.Reader) string {
	// Parse {N} or {N+}
	inner := s[1 : len(s)-1]
	// Handle optional trailing '+'
	n, err := strconv.Atoi(inner)
	if err != nil {
		if len(inner) > 1 && inner[len(inner)-1] == '+' {
			n, err = strconv.Atoi(inner[:len(inner)-1])
		}
		if err != nil {
			return s
		}
	}
	if n <= 0 {
		return ""
	}
	if n > 1000000 { // sanity check
		return s
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(reader, buf); err != nil {
		return s
	}
	return string(buf)
}

func authenticateIMAP(username, password string) bool {
	if config.IMAPConfigFile == "" && len(config.IMAPConfig.Users) == 0 {
		logger.Log().Debugf("[imap] no users configured, rejecting login for %s", username)
		return false
	}

	hash := sha256.Sum256([]byte(password))
	passwordHash := hex.EncodeToString(hash[:])

	// Check the database for the user
	if storage.IMAPUserAuth(username, passwordHash) {
		return true
	}

	// Fall back to static config (for initial seed or when no DB user exists)
	if userCfg, ok := config.IMAPConfig.Users[username]; ok {
		if userCfg.PasswordHash == passwordHash {
			// Seed into DB for future logins
			_ = storage.SetIMAPUserPassword(username, passwordHash)
			return true
		}
	}

	logger.Log().Debugf("[imap] auth failed for user %s", username)
	return false
}

func openMailbox(name string, user string, readOnly bool) mailbox {
	name = strings.Trim(name, "\"")

	// Determine the folder filter for the List() query.
	// INBOX maps to Folder="" (the default), standard virtual folders map to their name.
	folder := ""
	upper := strings.ToUpper(name)
	if upper != "INBOX" {
		if !isStandardMailbox(name) {
			// Check if it's a custom mailbox
			if !storage.CustomMailboxExists(name, user) {
				return mailbox{Name: name, ReadOnly: readOnly}
			}
			folder = name
		} else {
			folder = name
		}
	}

	messages, err := storage.List(0, 0, 0, folder, user)
	if err != nil {
		logger.Log().Errorf("[imap] %s", err.Error())
		return mailbox{}
	}

	mb := mailbox{
		Name:     name,
		User:     user,
		ReadOnly: readOnly,
	}

	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		flags := []string{}
		if m.Read {
			flags = append(flags, "\\Seen")
		}
		for _, tag := range m.Tags {
			flags = append(flags, tagToIMAPFlag(tag))
		}
		seq := len(messages) - i
		mb.Messages = append(mb.Messages, imapMessage{
			ID:      m.ID,
			UID:     uint64(seq),
			Size:    m.Size,
			Flags:   flags,
			Created: m.Created,
		})
	}
	mb.NextUID = uint64(len(messages) + 1)
	mb.Exists = len(messages)
	mb.Recent = 0
	mb.Seen = countSeen(mb.Messages)

	return mb
}

func countSeen(messages []imapMessage) int {
	count := 0
	for _, m := range messages {
		for _, f := range m.Flags {
			if f == "\\Seen" {
				count++
				break
			}
		}
	}
	return count
}

func deleteMarked(mbox mailbox) {
	var toDelete []string
	for _, m := range mbox.Messages {
		for _, f := range m.Flags {
			if f == "\\Deleted" {
				toDelete = append(toDelete, m.ID)
				break
			}
		}
	}
	if len(toDelete) > 0 {
		if err := storage.DeleteMessages(toDelete); err != nil {
			logger.Log().Errorf("[imap] error deleting: %s", err.Error())
		}
	}
}

func expungeMarked(conn net.Conn, mbox **mailbox) {
	var toDelete []string
	var newMessages []imapMessage
	var deletedSeqs []int

	for seq, m := range (*mbox).Messages {
		marked := false
		for _, f := range m.Flags {
			if f == "\\Deleted" {
				marked = true
				break
			}
		}
		if marked {
			toDelete = append(toDelete, m.ID)
			deletedSeqs = append(deletedSeqs, seq+1)
		} else {
			newMessages = append(newMessages, m)
		}
	}

	if len(toDelete) > 0 {
		if err := storage.DeleteMessages(toDelete); err != nil {
			logger.Log().Errorf("[imap] error deleting: %s", err.Error())
			return
		}
		// renumber UIDs
		for i := range newMessages {
			newMessages[i].UID = uint64(i + 1)
		}
		(*mbox).Messages = newMessages
		(*mbox).Exists = len(newMessages)
		(*mbox).NextUID = uint64(len(newMessages) + 1)

		// Report EXPUNGE in descending order so sequence numbers remain valid
		for i := len(deletedSeqs) - 1; i >= 0; i-- {
			sendResponse(conn, fmt.Sprintf("* %d EXPUNGE", deletedSeqs[i]))
		}
	}
}

func handleUID(conn net.Conn, tag string, args []string, mbox **mailbox) {
	if len(args) < 1 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	subCmd := strings.ToUpper(args[0])
	subArgs := args[1:]

	switch subCmd {
	case "FETCH":
		handleFetch(conn, tag, subArgs, mbox, true)
	case "STORE":
		handleStore(conn, tag, subArgs, mbox, true)
	case "SEARCH":
		handleSearch(conn, tag, subArgs, **mbox)
	case "COPY":
		handleCopy(conn, tag, subArgs, **mbox, true)
	case "MOVE":
		handleMove(conn, tag, subArgs, mbox, true)
	default:
		sendResponse(conn, tag+" BAD unknown UID command")
	}
}

func handleFetch(conn net.Conn, tag string, args []string, mbox **mailbox, uidMode bool) {
	if len(args) < 2 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	msgSet := args[0]
	attr := args[1]

	indices := parseMessageSet(msgSet, (*mbox).Exists, uidMode, (*mbox).Messages)

	for _, idx := range indices {
		if idx < 1 || idx > len((*mbox).Messages) {
			continue
		}
		msg := &(*mbox).Messages[idx-1]
		sendFetchResponse(conn, idx, *msg, attr)

		// RFC 3501: non-PEEK body access sets \Seen
		normAttr := strings.Trim(strings.ToUpper(attr), "()")
		if !isPeekAttr(normAttr) && isBodyAccess(normAttr) {
			if !hasFlag(msg.Flags, "\\Seen") {
				msg.Flags = addFlags(msg.Flags, []string{"\\Seen"})
				syncReadStatus(msg)
			}
		}
	}

	sendResponse(conn, tag+" OK FETCH completed")
}

func handleStore(conn net.Conn, tag string, args []string, mbox **mailbox, uidMode bool) {
	if len(args) < 3 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	if (*mbox).ReadOnly {
		mboxReply(conn, tag, "NO", "mailbox is read-only")
		return
	}

	msgSet := args[0]
	action := strings.ToUpper(args[1])
	actionBase := strings.TrimSuffix(action, ".SILENT")
	flagsRaw := args[2]

	flagsRaw = strings.Trim(flagsRaw, "()")
	flags := strings.Split(flagsRaw, " ")

	var cleanFlags []string
	for _, f := range flags {
		f = strings.TrimSpace(f)
		if f != "" {
			cleanFlags = append(cleanFlags, f)
		}
	}

	indices := parseMessageSet(msgSet, (*mbox).Exists, uidMode, (*mbox).Messages)

	for _, idx := range indices {
		if idx < 1 || idx > len((*mbox).Messages) {
			continue
		}
		msg := &(*mbox).Messages[idx-1]

		switch actionBase {
		case "FLAGS":
			msg.Flags = cleanFlags
		case "+FLAGS":
			msg.Flags = addFlags(msg.Flags, cleanFlags)
		case "-FLAGS":
			msg.Flags = removeFlags(msg.Flags, cleanFlags)
		}

		// Sync flags to database
		syncFlagsToStorage(msg)

		if action == actionBase {
			sendResponse(conn, fmt.Sprintf("* %d FETCH (FLAGS (%s))", idx, strings.Join(msg.Flags, " ")))
		}

		// Notify other IDLEing clients on the same mailbox about the flag change.
		storage.IdleNotify.NotifyFlagChangeUser((*mbox).User, (*mbox).Name, idx, msg.Flags)
	}

	sendResponse(conn, tag+" OK STORE completed")
}

func syncReadStatus(msg *imapMessage) {
	hasSeen := false
	for _, f := range msg.Flags {
		if f == "\\Seen" {
			hasSeen = true
			break
		}
	}

	if hasSeen {
		if err := storage.MarkRead([]string{msg.ID}); err != nil {
			logger.Log().Errorf("[imap] error marking read: %s", err.Error())
		}
	} else {
		if err := storage.MarkUnread([]string{msg.ID}); err != nil {
			logger.Log().Errorf("[imap] error marking unread: %s", err.Error())
		}
	}
}

// syncFlagsToStorage persists the in-memory IMAP flags for a message to the
// database: \Seen maps to the Read column, all other flags map to tags.
func syncFlagsToStorage(msg *imapMessage) {
	syncReadStatus(msg)

	var tags []string
	for _, f := range msg.Flags {
		if f == "\\Seen" {
			continue
		}
		// Store IMAP system flags (e.g. \Draft) as lowercase tag names.
		tag := strings.TrimPrefix(f, "\\")
		tag = strings.ToLower(tag)
		if tag != "" {
			tags = append(tags, tag)
		}
	}

	if len(tags) > 0 || msg.hasNonSeenFlag() {
		if _, err := storage.SetMessageTags(msg.ID, tags); err != nil {
			logger.Log().Errorf("[imap] error setting tags: %s", err.Error())
		}
	}
}

func (msg *imapMessage) hasNonSeenFlag() bool {
	for _, f := range msg.Flags {
		if f != "\\Seen" {
			return true
		}
	}
	return false
}

// tagToIMAPFlag converts a stored tag name back to an IMAP flag.
// Known system flag tags are returned with the leading backslash.
func tagToIMAPFlag(tag string) string {
	switch strings.ToLower(tag) {
	case "seen":
		return "\\Seen"
	case "answered":
		return "\\Answered"
	case "flagged":
		return "\\Flagged"
	case "deleted":
		return "\\Deleted"
	case "draft":
		return "\\Draft"
	default:
		return tag
	}
}

func addFlags(existing, new []string) []string {
	flagSet := make(map[string]bool)
	for _, f := range existing {
		flagSet[f] = true
	}
	for _, f := range new {
		flagSet[f] = true
	}
	var result []string
	for f := range flagSet {
		result = append(result, f)
	}
	return result
}

func removeFlags(existing, remove []string) []string {
	flagSet := make(map[string]bool)
	for _, f := range existing {
		flagSet[f] = true
	}
	for _, f := range remove {
		delete(flagSet, f)
	}
	var result []string
	for f := range flagSet {
		result = append(result, f)
	}
	return result
}

func isPeekAttr(attr string) bool {
	return strings.Contains(strings.ToUpper(attr), "BODY.PEEK")
}

func isBodyAccess(attr string) bool {
	upper := strings.ToUpper(attr)
	return strings.HasPrefix(upper, "BODY[") || upper == "BODY" ||
		strings.HasPrefix(upper, "RFC822")
}

func handleSearch(conn net.Conn, tag string, args []string, mbox mailbox) {
	if len(args) == 0 {
		var result []string
		for i := range mbox.Messages {
			result = append(result, strconv.Itoa(i+1))
		}
		sendResponse(conn, "* SEARCH "+strings.Join(result, " "))
		sendResponse(conn, tag+" OK SEARCH completed")
		return
	}

	pos := 0
	matchingIDs, err := evalSearchKeys(args, &pos, mbox)
	if err != nil {
		sendResponse(conn, tag+" BAD search error: "+err.Error())
		return
	}

	var result []string
	for i, m := range mbox.Messages {
		if matchingIDs[m.ID] {
			result = append(result, strconv.Itoa(i+1))
		}
	}

	sendResponse(conn, "* SEARCH "+strings.Join(result, " "))
	sendResponse(conn, tag+" OK SEARCH completed")
}

func evalSearchKeys(args []string, pos *int, mbox mailbox) (map[string]bool, error) {
	// Start with all message IDs (AND logic - each key narrows down)
	result := make(map[string]bool)
	for _, m := range mbox.Messages {
		result[m.ID] = true
	}

	for *pos < len(args) {
		keyResult, err := evalSearchKey(args, pos, mbox)
		if err != nil {
			return nil, err
		}
		result = intersectMaps(result, keyResult)
	}

	return result, nil
}

func evalSearchKey(args []string, pos *int, mbox mailbox) (map[string]bool, error) {
	if *pos >= len(args) {
		return nil, errors.New("unexpected end of search criteria")
	}

	orig := args[*pos]
	cmd := strings.ToUpper(orig)
	*pos++

	// Handle parenthesized search key groups: (key1 key2 ...)
	if strings.HasPrefix(cmd, "(") {
		if !strings.HasSuffix(cmd, ")") {
			return nil, errors.New("unclosed parenthesized search key")
		}
		inner := orig[1 : len(orig)-1]
		innerArgs := splitImapLine(inner)
		innerArgs = mergeParenGroups(innerArgs)
		innerPos := 0
		return evalSearchKeys(innerArgs, &innerPos, mbox)
	}

	switch cmd {
	case "ALL":
		return allMessageIDs(mbox), nil

	case "CHARSET":
		// Consume charset argument (e.g., "UTF-8").
		// We only support UTF-8 implicitly by normalizing to lowercase,
		// so just skip the charset name and return all messages.
		if *pos < len(args) {
			*pos++
		}
		return allMessageIDs(mbox), nil

	case "UNSEEN", "NEW":
		return flagMatch(mbox, "\\Seen", false), nil

	case "SEEN":
		return flagMatch(mbox, "\\Seen", true), nil

	case "ANSWERED":
		return flagMatch(mbox, "\\Answered", true), nil

	case "UNANSWERED":
		return flagMatch(mbox, "\\Answered", false), nil

	case "DELETED":
		return flagMatch(mbox, "\\Deleted", true), nil

	case "UNDELETED":
		return flagMatch(mbox, "\\Deleted", false), nil

	case "FLAGGED":
		return flagMatch(mbox, "\\Flagged", true), nil

	case "UNFLAGGED":
		return flagMatch(mbox, "\\Flagged", false), nil

	case "DRAFT":
		return flagMatch(mbox, "\\Draft", true), nil

	case "UNDRAFT":
		return flagMatch(mbox, "\\Draft", false), nil

	case "RECENT":
		return make(map[string]bool), nil

	case "OLD":
		return allMessageIDs(mbox), nil

	case "NOT":
		inner, err := evalSearchKey(args, pos, mbox)
		if err != nil {
			return nil, err
		}
		return complementMap(inner, mbox), nil

	case "OR":
		left, err := evalSearchKey(args, pos, mbox)
		if err != nil {
			return nil, err
		}
		right, err := evalSearchKey(args, pos, mbox)
		if err != nil {
			return nil, err
		}
		return unionMaps(left, right), nil

	case "SUBJECT":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for SUBJECT")
		}
		term := args[*pos]
		*pos++
		return searchBySubject(mbox, term)

	case "FROM", "SENDER":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for " + cmd)
		}
		term := args[*pos]
		*pos++
		return searchByMetadata(mbox, "From", term)

	case "TO":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for TO")
		}
		term := args[*pos]
		*pos++
		return searchByMetadata(mbox, "To", term)

	case "CC":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for CC")
		}
		term := args[*pos]
		*pos++
		return searchByMetadata(mbox, "Cc", term)

	case "BCC":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for BCC")
		}
		term := args[*pos]
		*pos++
		return searchByMetadata(mbox, "Bcc", term)

	case "BODY", "TEXT":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for " + cmd)
		}
		term := args[*pos]
		*pos++
		return searchBySearchText(mbox, term)

	case "SINCE", "SENTSINCE":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for " + cmd)
		}
		dateStr := args[*pos]
		*pos++
		return searchSince(mbox, dateStr)

	case "BEFORE", "SENTBEFORE":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for " + cmd)
		}
		dateStr := args[*pos]
		*pos++
		return searchBefore(mbox, dateStr)

	case "ON", "SENTON":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for " + cmd)
		}
		dateStr := args[*pos]
		*pos++
		return searchOn(mbox, dateStr)

	case "SMALLER":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for SMALLER")
		}
		sizeStr := args[*pos]
		*pos++
		return searchSmaller(mbox, sizeStr)

	case "LARGER":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for LARGER")
		}
		sizeStr := args[*pos]
		*pos++
		return searchLarger(mbox, sizeStr)

	case "KEYWORD":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for KEYWORD")
		}
		flag := args[*pos]
		*pos++
		return keywordFlagMatch(mbox, flag, true), nil

	case "UNKEYWORD":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for UNKEYWORD")
		}
		flag := args[*pos]
		*pos++
		return keywordFlagMatch(mbox, flag, false), nil

	case "HEADER":
		if *pos+1 >= len(args) {
			return nil, errors.New("missing argument for HEADER")
		}
		fieldName := args[*pos]
		fieldValue := args[*pos+1]
		*pos += 2
		return searchHeader(mbox, fieldName, fieldValue)

	case "UID":
		if *pos >= len(args) {
			return nil, errors.New("missing argument for UID")
		}
		uidStr := args[*pos]
		*pos++
		return searchUID(mbox, uidStr)

	default:
		// Treat as sequence set (single number or range)
		result := make(map[string]bool)
		if strings.Contains(cmd, ":") {
			parts := strings.SplitN(cmd, ":", 2)
			startStr := parts[0]
			endStr := parts[1]

			start := 1
			end := len(mbox.Messages)

			if startStr != "*" {
				if n, err := strconv.Atoi(startStr); err == nil && n >= 1 {
					start = n
				} else {
					return result, nil
				}
			}
			if endStr != "*" {
				if n, err := strconv.Atoi(endStr); err == nil && n >= 1 {
					end = n
				} else {
					return result, nil
				}
			}
			if start > end {
				start, end = end, start
			}
			if start < 1 {
				start = 1
			}
			if end > len(mbox.Messages) {
				end = len(mbox.Messages)
			}
			for i := start; i <= end; i++ {
				result[mbox.Messages[i-1].ID] = true
			}
		} else if cmd == "*" {
			for _, m := range mbox.Messages {
				result[m.ID] = true
			}
		} else {
			if id, err := strconv.Atoi(cmd); err == nil && id >= 1 && id <= len(mbox.Messages) {
				result[mbox.Messages[id-1].ID] = true
			}
		}
		return result, nil
	}
}

func allMessageIDs(mbox mailbox) map[string]bool {
	result := make(map[string]bool, len(mbox.Messages))
	for _, m := range mbox.Messages {
		result[m.ID] = true
	}
	return result
}

func flagMatch(mbox mailbox, flag string, present bool) map[string]bool {
	result := make(map[string]bool)
	for _, m := range mbox.Messages {
		if hasFlag(m.Flags, flag) == present {
			result[m.ID] = true
		}
	}
	return result
}

func keywordFlagMatch(mbox mailbox, flag string, present bool) map[string]bool {
	result := make(map[string]bool)
	for _, m := range mbox.Messages {
		if hasFlag(m.Flags, flag) == present {
			result[m.ID] = true
		} else if !strings.HasPrefix(flag, "\\") && hasFlag(m.Flags, "\\"+flag) == present {
			result[m.ID] = true
		} else if strings.HasPrefix(flag, "\\") && hasFlag(m.Flags, flag[1:]) == present {
			result[m.ID] = true
		}
	}
	return result
}

func intersectMaps(a, b map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k := range a {
		if b[k] {
			result[k] = true
		}
	}
	return result
}

func unionMaps(a, b map[string]bool) map[string]bool {
	result := make(map[string]bool)
	for k := range a {
		result[k] = true
	}
	for k := range b {
		result[k] = true
	}
	return result
}

func complementMap(inner map[string]bool, mbox mailbox) map[string]bool {
	result := make(map[string]bool)
	for _, m := range mbox.Messages {
		if !inner[m.ID] {
			result[m.ID] = true
		}
	}
	return result
}

func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func searchBySubject(mbox mailbox, term string) (map[string]bool, error) {
	term = stripQuotes(term)
	ids, err := storage.SearchIDsBySubject(term, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchByMetadata(mbox mailbox, field, term string) (map[string]bool, error) {
	term = stripQuotes(term)
	ids, err := storage.SearchIDsByMetadata(field, term, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchBySearchText(mbox mailbox, term string) (map[string]bool, error) {
	term = stripQuotes(term)
	ids, err := storage.SearchIDsBySearchText(term, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchSince(mbox mailbox, dateStr string) (map[string]bool, error) {
	dateStr = stripQuotes(dateStr)
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	ts := t.UnixMilli()
	ids, err := storage.SearchIDsByCreated(">=", ts, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchBefore(mbox mailbox, dateStr string) (map[string]bool, error) {
	dateStr = stripQuotes(dateStr)
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	ts := t.UnixMilli()
	ids, err := storage.SearchIDsByCreated("<", ts, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchOn(mbox mailbox, dateStr string) (map[string]bool, error) {
	dateStr = stripQuotes(dateStr)
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	startOfDay := t.UnixMilli()
	endOfDay := t.Add(24*time.Hour).UnixMilli() - 1
	ids, err := storage.SearchIDsByCreatedBetween(startOfDay, endOfDay, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchSmaller(mbox mailbox, sizeStr string) (map[string]bool, error) {
	size, err := strconv.ParseUint(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid size: %s", sizeStr)
	}
	ids, err := storage.SearchIDsBySize("<", size, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchLarger(mbox mailbox, sizeStr string) (map[string]bool, error) {
	size, err := strconv.ParseUint(sizeStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid size: %s", sizeStr)
	}
	ids, err := storage.SearchIDsBySize(">", size, mbox.User)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result, nil
}

func searchHeader(mbox mailbox, fieldName, fieldValue string) (map[string]bool, error) {
	// Search in SearchText which contains most headers
	return searchBySearchText(mbox, fieldValue)
}

func searchUID(mbox mailbox, uidStr string) (map[string]bool, error) {
	result := make(map[string]bool)
	maxUID := len(mbox.Messages)

	if strings.Contains(uidStr, ":") {
		parts := strings.SplitN(uidStr, ":", 2)
		startStr := strings.TrimSpace(parts[0])
		endStr := strings.TrimSpace(parts[1])

		start := 1
		end := maxUID

		if startStr != "*" {
			if n, err := strconv.Atoi(startStr); err == nil && n >= 1 {
				start = n
			} else {
				return result, nil
			}
		}
		if endStr != "*" {
			if n, err := strconv.Atoi(endStr); err == nil && n >= 1 {
				end = n
			} else {
				return result, nil
			}
		}
		if start > end {
			start, end = end, start
		}
		if start < 1 {
			start = 1
		}
		if end > maxUID {
			end = maxUID
		}
		for i := start; i <= end; i++ {
			result[mbox.Messages[i-1].ID] = true
		}
	} else if uidStr == "*" {
		for _, m := range mbox.Messages {
			result[m.ID] = true
		}
	} else {
		if id, err := strconv.Atoi(uidStr); err == nil && id >= 1 && id <= maxUID {
			result[mbox.Messages[id-1].ID] = true
		}
	}
	return result, nil
}

func hasFlag(flags []string, flag string) bool {
	for _, f := range flags {
		if strings.EqualFold(f, flag) {
			return true
		}
	}
	return false
}

// matchMailboxWildcard matches a LIST reference name against a mailbox name.
// "*" matches everything, "%" matches within a hierarchy level.
func matchMailboxWildcard(wildcard, name string) bool {
	if wildcard == "*" || wildcard == "%" {
		return true
	}
	upper := strings.ToUpper(wildcard)
	nameUpper := strings.ToUpper(name)
	if strings.HasPrefix(upper, strings.ToUpper(name)) {
		return true
	}
	if strings.Contains(upper, "*") || strings.Contains(upper, "%") {
		// Simple glob: * matches any suffix, % matches within level
		base := strings.TrimRight(upper, "*%")
		return strings.HasPrefix(nameUpper, base)
	}
	return upper == nameUpper
}

// handleAppend processes the APPEND command: APPEND mailbox [flags] [date-time] literal
func handleAppend(conn net.Conn, tag string, args []string, user string) {
	if len(args) < 2 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	mailboxName := strings.Trim(args[0], "\"")

	// Determine the folder from the mailbox name
	folder := ""
	upper := strings.ToUpper(mailboxName)
	if upper != "INBOX" && isStandardMailbox(mailboxName) {
		folder = mailboxName
	} else if upper != "INBOX" && !isStandardMailbox(mailboxName) {
		// Check if it's a custom mailbox
		if storage.CustomMailboxExists(mailboxName, user) {
			folder = mailboxName
		} else {
			sendResponse(conn, tag+" NO mailbox not found")
			return
		}
	}

	// Parse optional flags and date-time from middle args
	// Last arg is always the literal data (message body)
	var flags []string
	var messageData string
	var appendDate *time.Time

	// The literal data is always the last arg (already read by handleCommand)
	messageData = args[len(args)-1]

	// Middle args may contain flags (parenthesized) and/or date-time (quoted)
	for _, arg := range args[1 : len(args)-1] {
		if strings.HasPrefix(arg, "(") {
			// Flags: (\Seen \Draft) — strip parens and split
			flagsRaw := strings.Trim(arg, "()")
			for _, f := range strings.Fields(flagsRaw) {
				flags = append(flags, f)
			}
		} else if t, err := time.Parse("02-Jan-2006 15:04:05 -0700", arg); err == nil {
			// Date-time: dd-Mon-yyyy hh:mm:ss +hhmm
			appendDate = &t
		}
	}

	body := []byte(messageData)
	id, err := storage.Store(&body, nil, folder, user)
	if err != nil {
		sendResponse(conn, tag+" NO store failed: "+err.Error())
		return
	}
	if id == "" {
		sendResponse(conn, tag+" NO APPEND failed: invalid message data")
		return
	}

	// Override Created timestamp with the client-provided date-time.
	if appendDate != nil {
		if err := storage.UpdateCreated(id, *appendDate); err != nil {
			logger.Log().Errorf("[imap] failed to update created date: %s", err.Error())
		}
	}

	// Apply flags by creating a temporary imapMessage and syncing to storage.
	appendedMsg := imapMessage{ID: id, Flags: flags}
	syncFlagsToStorage(&appendedMsg)

	sendResponse(conn, tag+" OK APPEND completed")
}

// handleCopy processes the COPY command: COPY sequence-set mailbox
func handleCopy(conn net.Conn, tag string, args []string, mbox mailbox, uidMode ...bool) {
	if len(args) < 2 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	um := false
	if len(uidMode) > 0 {
		um = uidMode[0]
	}

	msgSet := args[0]
	destName := strings.Trim(args[1], "\"")

	// Determine destination folder
	destFolder := ""
	upper := strings.ToUpper(destName)
	if upper != "INBOX" && isStandardMailbox(destName) {
		destFolder = destName
	} else if upper != "INBOX" && !isStandardMailbox(destName) {
		// Check if it's a custom mailbox
		if storage.CustomMailboxExists(destName, mbox.User) {
			destFolder = destName
		} else {
			sendResponse(conn, tag+" NO destination mailbox not found")
			return
		}
	}

	indices := parseMessageSet(msgSet, mbox.Exists, um, mbox.Messages)

	for _, idx := range indices {
		if idx < 1 || idx > len(mbox.Messages) {
			continue
		}
		msg := mbox.Messages[idx-1]
		raw, err := storage.GetMessageRaw(msg.ID)
		if err != nil {
			continue
		}
		newID, err := storage.Store(&raw, nil, destFolder, mbox.User)
		if err != nil || newID == "" {
			continue
		}
		// Preserve source flags on the copied message.
		copied := imapMessage{ID: newID, Flags: msg.Flags}
		syncFlagsToStorage(&copied)
	}

	sendResponse(conn, tag+" OK COPY completed")
}

// handleMove processes the MOVE command: MOVE sequence-set mailbox
// This moves messages from the current mailbox to the destination mailbox
func handleMove(conn net.Conn, tag string, args []string, mbox **mailbox, uidMode bool) {
	if len(args) < 2 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	if (*mbox).ReadOnly {
		mboxReply(conn, tag, "NO", "mailbox is read-only")
		return
	}

	msgSet := args[0]
	destName := strings.Trim(args[1], "\"")

	// Determine destination folder
	destFolder := ""
	upper := strings.ToUpper(destName)
	if upper != "INBOX" && isStandardMailbox(destName) {
		destFolder = destName
	} else if upper == "INBOX" {
		destFolder = ""
	} else {
		// Check if it's a custom mailbox
		if storage.CustomMailboxExists(destName, (*mbox).User) {
			destFolder = destName
		} else {
			sendResponse(conn, tag+" NO destination mailbox not found")
			return
		}
	}

	indices := parseMessageSet(msgSet, (*mbox).Exists, uidMode, (*mbox).Messages)

	var movedIDs []string
	var newMessages []imapMessage

	for _, idx := range indices {
		if idx < 1 || idx > len((*mbox).Messages) {
			continue
		}
		msg := (*mbox).Messages[idx-1]
		// Move the message by updating its Folder column
		if err := storage.MoveMessage(msg.ID, destFolder); err != nil {
			logger.Log().Errorf("[imap] error moving message: %s", err.Error())
			continue
		}
		movedIDs = append(movedIDs, msg.ID)
	}

	// Remove moved messages from the in-memory mailbox
	if len(movedIDs) > 0 {
		for _, m := range (*mbox).Messages {
			deleted := false
			for _, id := range movedIDs {
				if m.ID == id {
					deleted = true
					break
				}
			}
			if !deleted {
				newMessages = append(newMessages, m)
			}
		}
		// Renumber UIDs
		for i := range newMessages {
			newMessages[i].UID = uint64(i + 1)
		}
		(*mbox).Messages = newMessages
		(*mbox).Exists = len(newMessages)
		(*mbox).NextUID = uint64(len(newMessages) + 1)
	}

	sendResponse(conn, tag+" OK MOVE completed")
}
