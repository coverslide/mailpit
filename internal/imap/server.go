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
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
)

const (
	// IMAP capability string
	imapCapabilities = "IMAP4rev1 LOGIN-REFERRALS AUTH=PLAIN UIDPLUS LITERAL+ NAMESPACE IDLE MOVE CHILDREN"
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
	ID        string
	UID       uint64
	Size      uint64
	Flags     []string
	Created   time.Time
}

type clientState int

const (
	stateNotAuthenticated clientState = iota
	stateAuthenticated
	stateSelected
)

type mailbox struct {
	Name       string
	Messages   []imapMessage
	NextUID    uint64
	Exists     int
	Recent     int
	Seen       int
	ReadOnly   bool
}

func handleClient(conn net.Conn) {
	var (
		state   = stateNotAuthenticated
		user    = ""
		mbox    *mailbox
	)

	defer func() {
		if err := conn.Close(); err != nil {
			logger.Log().Errorf("[imap] %s", err.Error())
		}
	}()

	reader := bufio.NewReader(conn)
	timeoutDuration := 600 * time.Second

	logger.Log().Debugf("[imap] connection opened by %s", conn.RemoteAddr().String())

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

		if !handleCommand(conn, reader, rawLine, &state, &user, &mbox) {
			return
		}
	}
}

func handleCommand(conn net.Conn, reader *bufio.Reader, rawLine string, state *clientState, user *string, mbox **mailbox) bool {
	tag, cmd, args := parseImapCommand(rawLine)

	if tag == "" {
		return true
	}

	// Handle IMAP literals (e.g., {5+}) in arguments
	for i, arg := range args {
		if isLiteralPattern(arg) {
			args[i] = readLiteralData(arg, reader)
		}
	}

	switch strings.ToUpper(cmd) {
	case "CAPABILITY":
		sendResponse(conn, "* CAPABILITY "+imapCapabilities)
		sendResponse(conn, tag+" OK CAPABILITY completed")
	case "NOOP":
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
		sendResponse(conn, fmt.Sprintf("* FLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)"))
		sendResponse(conn, fmt.Sprintf("* OK [PERMANENTFLAGS (\\Seen \\Answered \\Flagged \\Deleted \\Draft)]"))
		sendResponse(conn, fmt.Sprintf("* OK [UIDNEXT %d]", mb.NextUID))
		sendResponse(conn, fmt.Sprintf("* OK [UIDVALIDITY 1]"))
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
		sendResponse(conn, "* OK [PERMANENTFLAGS ()]")
		sendResponse(conn, fmt.Sprintf("* OK [UIDNEXT %d]", mb.NextUID))
		sendResponse(conn, "* OK [UIDVALIDITY 1]")
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
		mailboxName := args[0]
		mb := openMailbox(mailboxName, *user, true)
		sendResponse(conn, fmt.Sprintf("* STATUS %s (MESSAGES %d UNSEEN %d UIDNEXT %d UIDVALIDITY 1)", mailboxName, mb.Exists, mb.Exists-mb.Seen, mb.NextUID))
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
		if wildcard == "*" || wildcard == "%" || wildcard == "INBOX" || wildcard == "INBOX*" || wildcard == "INBOX%" {
			sendResponse(conn, `* LIST (\HasNoChildren) "/" INBOX`)
		}
		sendResponse(conn, tag+" OK LIST completed")
	case "LSUB":
		sendResponse(conn, `* LSUB (\HasNoChildren) "/" INBOX`)
		sendResponse(conn, tag+" OK LSUB completed")
	case "FETCH":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleFetch(conn, tag, args, **mbox, false)
	case "STORE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		handleStore(conn, tag, args, mbox)
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
	case "NAMESPACE":
		sendResponse(conn, `* NAMESPACE (("" "/")) NIL NIL`)
		sendResponse(conn, tag+" OK NAMESPACE completed")
	case "IDLE":
		sendResponse(conn, "+ idling")
	doneLoop:
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			line = strings.TrimRight(line, "\r\n")
			switch strings.ToUpper(line) {
			case "DONE":
				break doneLoop
			}
		}
		sendResponse(conn, tag+" OK IDLE completed")
	case "CHECK":
		sendResponse(conn, tag+" OK CHECK completed")
	case "EXPUNGE":
		if *state != stateSelected {
			sendResponse(conn, tag+" BAD no mailbox selected")
			return true
		}
		if !(*mbox).ReadOnly {
			expungeMarked(conn, mbox)
		}
		sendResponse(conn, tag+" OK EXPUNGE completed")
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

	userCfg, ok := config.IMAPConfig.Users[username]
	if !ok {
		logger.Log().Debugf("[imap] unknown user %s", username)
		return false
	}

	hash := sha256.Sum256([]byte(password))
	passwordHash := hex.EncodeToString(hash[:])

	logger.Log().Debugf("[imap] password hash for %s: %s (expected: %s)", username, passwordHash, userCfg.PasswordHash)

	if userCfg.PasswordHash != passwordHash {
		logger.Log().Debugf("[imap] invalid password for user %s", username)
		return false
	}

	return true
}

func openMailbox(name string, user string, readOnly bool) mailbox {
	if strings.ToUpper(name) != "INBOX" {
		return mailbox{}
	}

	messages, err := storage.List(0, 0, 0)
	if err != nil {
		logger.Log().Errorf("[imap] %s", err.Error())
		return mailbox{}
	}

	mb := mailbox{
		Name:       name,
		ReadOnly:   readOnly,
	}

	nextUID := uint64(1)
	for i, m := range messages {
		flags := []string{}
		if m.Read {
			flags = append(flags, "\\Seen")
		}
		flags = append(flags, m.Tags...)
		mb.Messages = append(mb.Messages, imapMessage{
			ID:    m.ID,
			UID:   uint64(i + 1),
			Size:  m.Size,
			Flags: flags,
			Created: m.Created,
		})
		nextUID = uint64(i + 2)
	}
	mb.NextUID = nextUID
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
	removed := 0

	for _, m := range (*mbox).Messages {
		marked := false
		for _, f := range m.Flags {
			if f == "\\Deleted" {
				marked = true
				break
			}
		}
		if marked {
			toDelete = append(toDelete, m.ID)
			removed++
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

		for i := 0; i < removed; i++ {
			sendResponse(conn, fmt.Sprintf("* %d EXPUNGE", len(newMessages)+i+1))
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
		handleFetch(conn, tag, subArgs, **mbox, true)
	case "STORE":
		handleStore(conn, tag, subArgs, mbox)
	case "SEARCH":
		handleSearch(conn, tag, subArgs, **mbox)
	default:
		sendResponse(conn, tag+" BAD unknown UID command")
	}
}

func handleFetch(conn net.Conn, tag string, args []string, mbox mailbox, uidMode bool) {
	if len(args) < 2 {
		sendResponse(conn, tag+" BAD invalid arguments")
		return
	}

	msgSet := args[0]
	attr := args[1]

	indices := parseMessageSet(msgSet, mbox.Exists, uidMode, mbox.Messages)

	for _, idx := range indices {
		msg := mbox.Messages[idx-1]
		sendFetchResponse(conn, idx, msg, attr)
	}

	sendResponse(conn, tag+" OK FETCH completed")
}

func handleStore(conn net.Conn, tag string, args []string, mbox **mailbox) {
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

	indices := parseMessageSet(msgSet, (*mbox).Exists, false, (*mbox).Messages)

	for _, idx := range indices {
		if idx < 1 || idx > len((*mbox).Messages) {
			continue
		}
		msg := &(*mbox).Messages[idx-1]

		switch action {
		case "FLAGS":
			msg.Flags = cleanFlags
		case "+FLAGS":
			msg.Flags = addFlags(msg.Flags, cleanFlags)
		case "-FLAGS":
			msg.Flags = removeFlags(msg.Flags, cleanFlags)
		}

		// Sync read status to database
		syncReadStatus(msg)

		sendResponse(conn, fmt.Sprintf("* %d FETCH (FLAGS (%s))", idx, strings.Join(msg.Flags, " ")))
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

	cmd := strings.ToUpper(args[*pos])
	*pos++

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
			start, err1 := strconv.Atoi(parts[0])
			end, err2 := strconv.Atoi(parts[1])
			if err1 == nil && err2 == nil {
				if start < 1 {
					start = 1
				}
				if end > len(mbox.Messages) {
					end = len(mbox.Messages)
				}
				for i := start; i <= end; i++ {
					result[mbox.Messages[i-1].ID] = true
				}
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
	ids, err := storage.SearchIDsBySubject(term)
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
	ids, err := storage.SearchIDsByMetadata(field, term)
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
	ids, err := storage.SearchIDsBySearchText(term)
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
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	ts := t.UnixMilli()
	ids, err := storage.SearchIDsByCreated(">=", ts)
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
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	ts := t.UnixMilli()
	ids, err := storage.SearchIDsByCreated("<", ts)
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
	t, err := time.Parse("02-Jan-2006", dateStr)
	if err != nil {
		return nil, fmt.Errorf("invalid date: %s", dateStr)
	}
	startOfDay := t.UnixMilli()
	endOfDay := t.Add(24 * time.Hour).UnixMilli() - 1
	ids, err := storage.SearchIDsByCreatedBetween(startOfDay, endOfDay)
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
	ids, err := storage.SearchIDsBySize("<", size)
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
	ids, err := storage.SearchIDsBySize(">", size)
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
	if strings.Contains(uidStr, ":") {
		parts := strings.SplitN(uidStr, ":", 2)
		start, err1 := strconv.Atoi(parts[0])
		end, err2 := strconv.Atoi(parts[1])
		if err1 == nil && err2 == nil {
			if start < 1 {
				start = 1
			}
			if end > len(mbox.Messages) {
				end = len(mbox.Messages)
			}
			for i := start; i <= end; i++ {
				result[mbox.Messages[i-1].ID] = true
			}
		}
	} else {
		if id, err := strconv.Atoi(uidStr); err == nil && id >= 1 && id <= len(mbox.Messages) {
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
