package imap

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/storage"
	"github.com/axllent/mailpit/server/websockets"
)

func sendResponse(c net.Conn, m string) {
	_, _ = fmt.Fprintf(c, "%s\r\n", m)
	logger.Log().Debugf("[imap] response: %s", m)

	if strings.HasPrefix(m, "NO ") || strings.Contains(m, " BAD ") {
		sub := m
		if idx := strings.Index(m, " "); idx >= 0 {
			sub = m[idx+1:]
		}
		websockets.BroadCastClientError("error", "imap", c.RemoteAddr().String(), sub)
	}
}

func sendData(c net.Conn, m string) {
	_, _ = fmt.Fprintf(c, "%s\r\n", m)
}

func mboxReply(conn net.Conn, tag, status, msg string) {
	sendResponse(conn, fmt.Sprintf("%s %s %s", tag, status, msg))
}

func sendFetchResponse(conn net.Conn, seq int, msg imapMessage, attr string) {
	attr = strings.ToUpper(attr)

	switch attr {
	case "FULL":
		sendFullFetch(conn, seq, msg)
	case "ALL":
		sendAllFetch(conn, seq, msg)
	case "FAST":
		sendFastFetch(conn, seq, msg)
	default:
		sendCustomFetch(conn, seq, msg, attr)
	}
}

func sendFullFetch(conn net.Conn, seq int, msg imapMessage) {
	flags := formatFlags(msg.Flags)
	internalDate := formatInternalDate(msg.Created)
	envelope := getEnvelope(msg.ID)
	body := getBodyStructure(msg.ID)

	sendResponse(conn, fmt.Sprintf("* %d FETCH (FLAGS (%s) INTERNALDATE %s RFC822.SIZE %d ENVELOPE %s BODYSTRUCTURE %s UID %d)",
		seq, flags, internalDate, msg.Size, envelope, body, msg.UID))
}

func sendAllFetch(conn net.Conn, seq int, msg imapMessage) {
	flags := formatFlags(msg.Flags)
	internalDate := formatInternalDate(msg.Created)
	envelope := getEnvelope(msg.ID)

	sendResponse(conn, fmt.Sprintf("* %d FETCH (FLAGS (%s) INTERNALDATE %s RFC822.SIZE %d ENVELOPE %s UID %d)",
		seq, flags, internalDate, msg.Size, envelope, msg.UID))
}

func sendFastFetch(conn net.Conn, seq int, msg imapMessage) {
	flags := formatFlags(msg.Flags)
	internalDate := formatInternalDate(msg.Created)

	sendResponse(conn, fmt.Sprintf("* %d FETCH (FLAGS (%s) INTERNALDATE %s RFC822.SIZE %d UID %d)",
		seq, flags, internalDate, msg.Size, msg.UID))
}

func sendCustomFetch(conn net.Conn, seq int, msg imapMessage, attrs string) {
	var parts []string

	// Parse parenthesized attribute list
	attrs = strings.Trim(attrs, "()")
	attrList := splitFetchAttrs(attrs)

	uidIncluded := false
	for _, a := range attrList {
		a = strings.ToUpper(a)
		switch {
		case a == "FLAGS":
			parts = append(parts, fmt.Sprintf("FLAGS (%s)", formatFlags(msg.Flags)))
		case a == "INTERNALDATE":
			parts = append(parts, fmt.Sprintf("INTERNALDATE %s", formatInternalDate(msg.Created)))
		case a == "RFC822.SIZE":
			parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", msg.Size))
		case a == "UID":
			uidIncluded = true
			parts = append(parts, fmt.Sprintf("UID %d", msg.UID))
		case a == "ENVELOPE":
			parts = append(parts, fmt.Sprintf("ENVELOPE %s", getEnvelope(msg.ID)))
		case a == "BODYSTRUCTURE":
			parts = append(parts, fmt.Sprintf("BODYSTRUCTURE %s", getBodyStructure(msg.ID)))
		case a == "BODY" || strings.HasPrefix(a, "BODY["):
			bodyPart := getBodyPart(msg.ID, a)
			parts = append(parts, bodyPart)
		case a == "BODY.PEEK" || strings.HasPrefix(a, "BODY.PEEK["):
			peekAttr := strings.Replace(a, "BODY.PEEK", "BODY", 1)
			bodyPart := getBodyPart(msg.ID, peekAttr)
			parts = append(parts, bodyPart)
		case strings.HasPrefix(a, "RFC822"):
			bodyPart := getBodyPart(msg.ID, "BODY[]")
			if a == "RFC822.HEADER" {
				bodyPart = getBodyPart(msg.ID, "BODY[HEADER]")
			} else if a == "RFC822.TEXT" {
				bodyPart = getBodyPart(msg.ID, "BODY[TEXT]")
			}
			parts = append(parts, bodyPart)
		}
	}

	if !uidIncluded {
		parts = append(parts, fmt.Sprintf("UID %d", msg.UID))
	}

	if len(parts) > 0 {
		sendResponse(conn, fmt.Sprintf("* %d FETCH (%s)", seq, strings.Join(parts, " ")))
	}
}

func formatFlags(flags []string) string {
	if len(flags) == 0 {
		return ""
	}
	return strings.Join(flags, " ")
}

func formatInternalDate(t time.Time) string {
	return fmt.Sprintf("\"%s\"", t.Format("02-Jan-2006 15:04:05 -0700"))
}

func getEnvelope(id string) string {
	raw, err := storage.GetMessageRaw(id)
	if err != nil {
		return "NIL"
	}

	return buildEnvelopeFromRaw(raw)
}

func buildEnvelopeFromRaw(raw []byte) string {
	text := string(raw)

	date := extractHeader(text, "Date")
	subject := extractHeader(text, "Subject")
	from := extractAddresses(text, "From")
	sender := extractAddresses(text, "Sender")
	replyTo := extractAddresses(text, "Reply-To")
	to := extractAddresses(text, "To")
	cc := extractAddresses(text, "Cc")
	bcc := extractAddresses(text, "Bcc")
	inReplyTo := extractHeader(text, "In-Reply-To")
	messageID := extractHeader(text, "Message-ID")

	return fmt.Sprintf("(%s %s %s %s %s %s %s %s %s %s)",
		nilOrString(date),
		nilOrString(subject),
		nilOrStringList(from),
		nilOrStringList(sender),
		nilOrStringList(replyTo),
		nilOrStringList(to),
		nilOrStringList(cc),
		nilOrStringList(bcc),
		nilOrString(inReplyTo),
		nilOrString(messageID),
	)
}

func nilOrString(s string) string {
	if s == "" {
		return "NIL"
	}
	return fmt.Sprintf("\"%s\"", imapQuote(s))
}

func nilOrStringList(s string) string {
	if s == "" {
		return "NIL"
	}
	return s
}

func imapQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	return s
}

func extractHeader(text, header string) string {
	re := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(header) + ":\\s*(.*?)(?:\\r?\\n(?:\\s+.*?\\r?\\n)*|$)")
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return ""
	}
	value := strings.TrimSpace(match[1])
	// handle folded headers
	re2 := regexp.MustCompile(`\r?\n\s+`)
	value = re2.ReplaceAllString(value, " ")
	return value
}

func extractAddresses(text, header string) string {
	value := extractHeader(text, header)
	if value == "" {
		return "NIL"
	}

	addrs := parseIMAPAddresses(value)
	if len(addrs) == 0 {
		return "NIL"
	}

	return "(" + strings.Join(addrs, " ") + ")"
}

func parseIMAPAddresses(addrStr string) []string {
	var result []string

	parts := strings.Split(addrStr, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}

		// Try to parse "Name <email@domain>" or just "email@domain"
		name := ""
		email := p

		if idx := strings.Index(p, "<"); idx >= 0 {
			name = strings.TrimSpace(p[:idx])
			email = strings.Trim(p[idx:], "<>")
		}

		mailboxName := ""
		hostName := email

		if idx := strings.LastIndex(email, "@"); idx >= 0 {
			mailboxName = email[:idx]
			hostName = email[idx+1:]
		}

		addr := fmt.Sprintf("(%s NIL \"%s\" \"%s\")",
			imapQuote(name),
			imapQuote(mailboxName),
			imapQuote(hostName),
		)

		result = append(result, addr)
	}

	return result
}

func getBodyStructure(id string) string {
	raw, err := storage.GetMessageRaw(id)
	if err != nil {
		return "NIL"
	}

	return buildBodyStructure(raw)
}

func buildBodyStructure(raw []byte) string {
	text := string(raw)

	contentType := extractHeader(text, "Content-Type")
	if contentType == "" {
		contentType = "text/plain"
	}

	// Split into MIME type and subtype
	mimeType := "text"
	subType := "plain"

	if idx := strings.Index(contentType, ";"); idx >= 0 {
		contentType = strings.TrimSpace(contentType[:idx])
	}

	if idx := strings.Index(contentType, "/"); idx >= 0 {
		mimeType = strings.ToLower(strings.TrimSpace(contentType[:idx]))
		subType = strings.ToLower(strings.TrimSpace(contentType[idx+1:]))
	} else {
		mimeType = strings.ToLower(strings.TrimSpace(contentType))
		subType = "plain"
	}

	// Extract Content-Type parameters
	typeParams := extractContentTypeParams(text)
	contentID := extractHeader(text, "Content-ID")
	contentDesc := extractHeader(text, "Content-Description")
	contentEncoding := extractHeader(text, "Content-Transfer-Encoding")
	if contentEncoding == "" {
		contentEncoding = "7bit"
	}

	bodyLines := countLines(raw)
	bodyOctets := len(raw)

	if mimeType == "text" {
		return fmt.Sprintf("(\"%s\" \"%s\" %s NIL %s %s \"%s\" %d %d NIL NIL NIL)",
			mimeType, subType, typeParams,
			nilOrString(contentID),
			nilOrString(contentDesc),
			contentEncoding, bodyLines, bodyOctets)
	}

	if mimeType == "multipart" {
		boundary := extractBoundary(text)
		if boundary != "" {
			bodyParts := splitByBoundary(text, boundary)
			var children []string
			for _, part := range bodyParts {
				children = append(children, buildBodyStructure([]byte(part)))
			}
			return fmt.Sprintf("(%s \"%s\" NIL NIL NIL)",
				strings.Join(children, " "), subType)
		}
	}

	return fmt.Sprintf("(\"%s\" \"%s\" %s NIL %s %s \"%s\" %d NIL NIL NIL)",
		mimeType, subType, typeParams,
		nilOrString(contentID),
		nilOrString(contentDesc),
		contentEncoding, bodyOctets)
}

func extractContentTypeParams(text string) string {
	ct := extractHeader(text, "Content-Type")
	if ct == "" {
		return "NIL"
	}

	// Find boundary/charset etc
	params := ""
	idx := strings.Index(ct, ";")
	if idx >= 0 {
		rest := strings.TrimSpace(ct[idx+1:])
		if rest != "" {
			parts := strings.Split(rest, ";")
			var kvs []string
			for _, p := range parts {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				kv := strings.SplitN(p, "=", 2)
				if len(kv) == 2 {
					k := strings.TrimSpace(strings.ToLower(kv[0]))
					v := strings.Trim(strings.TrimSpace(kv[1]), "\"")
					kvs = append(kvs, fmt.Sprintf("\"%s\" \"%s\"", k, v))
				}
			}
			if len(kvs) > 0 {
				params = "(" + strings.Join(kvs, " ") + ")"
			}
		}
	}

	if params == "" {
		return "NIL"
	}
	return params
}

func extractBoundary(text string) string {
	ct := extractHeader(text, "Content-Type")
	idx := strings.Index(ct, "boundary=")
	if idx < 0 {
		return ""
	}
	boundary := ct[idx+9:]
	if strings.HasPrefix(boundary, "\"") {
		boundary = strings.Trim(boundary, "\"")
	} else {
		idx2 := strings.IndexAny(boundary, " ;")
		if idx2 > 0 {
			boundary = boundary[:idx2]
		}
	}
	return strings.TrimSpace(boundary)
}

func splitByBoundary(text, boundary string) []string {
	delim := "--" + boundary
	parts := strings.Split(text, delim)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "--" || p == "-" {
			continue
		}
		if idx := strings.Index(p, "\n"); idx >= 0 {
			p = p[idx+1:]
		}
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func countLines(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	count := 0
	for _, b := range raw {
		if b == '\n' {
			count++
		}
	}
	return count
}

// parsePartialRange extracts the <offset.size> suffix from a BODY attribute.
// Returns the base attribute (without the partial specifier) and the offset and size.
func parsePartialRange(attr string) (baseAttr string, offset, size int, hasPartial bool) {
	startIdx := strings.LastIndex(attr, "<")
	endIdx := strings.LastIndex(attr, ">")
	if startIdx < 0 || endIdx <= startIdx {
		return attr, 0, 0, false
	}
	rangeStr := attr[startIdx+1 : endIdx]
	baseAttr = attr[:startIdx]

	dotIdx := strings.Index(rangeStr, ".")
	if dotIdx < 0 {
		// <offset> only — no dot, size means rest
		off, err := strconv.Atoi(rangeStr)
		if err != nil {
			return attr, 0, 0, false
		}
		return baseAttr, off, -1, true
	}

	offStr := rangeStr[:dotIdx]
	sizeStr := rangeStr[dotIdx+1:]

	off, err := strconv.Atoi(offStr)
	if err != nil {
		return attr, 0, 0, false
	}

	if sizeStr == "" {
		// <offset.> — read to end
		return baseAttr, off, -1, true
	}

	sz, err := strconv.Atoi(sizeStr)
	if err != nil {
		return attr, 0, 0, false
	}

	return baseAttr, off, sz, true
}

func getBodyPart(id, attr string) string {
	raw, err := storage.GetMessageRaw(id)
	if err != nil {
		return "NIL"
	}

	text := string(raw)
	origAttr := attr
	attr = strings.ToUpper(attr)

	// Check for partial range <offset.size>
	baseAttr, offset, size, isPartial := parsePartialRange(attr)

	switch {
	case baseAttr == "BODY[]" || baseAttr == "BODY":
		if isPartial {
			if offset < 0 {
				offset = 0
			}
			if offset >= len(raw) {
				return fmt.Sprintf("%s {%d}\r\n", origAttr, 0)
			}
			if size < 0 || offset+size > len(raw) {
				size = len(raw) - offset
			}
			sliced := raw[offset : offset+size]
			return fmt.Sprintf("%s {%d}\r\n%s", origAttr, len(sliced), string(sliced))
		}
		return fmt.Sprintf("BODY[] {%d}\r\n%s", len(raw), text)
	case baseAttr == "BODY[HEADER]" || strings.HasPrefix(baseAttr, "BODY[HEADER.FIELDS"):
		if isPartial {
			return fmt.Sprintf("%s {%d}\r\n%s", origAttr, 0, "")
		}
		return getBodyHeader(raw, text, attr)
	case baseAttr == "BODY[TEXT]":
		parts := strings.SplitN(text, "\r\n\r\n", 2)
		if len(parts) < 2 {
			return fmt.Sprintf("BODY[TEXT] {%d}\r\n", 0)
		}
		bodyText := parts[1]
		if isPartial {
			if offset < 0 {
				offset = 0
			}
			if offset >= len(bodyText) {
				return fmt.Sprintf("%s {%d}\r\n", origAttr, 0)
			}
			if size < 0 || offset+size > len(bodyText) {
				size = len(bodyText) - offset
			}
			sliced := bodyText[offset : offset+size]
			return fmt.Sprintf("%s {%d}\r\n%s", origAttr, len(sliced), sliced)
		}
		return fmt.Sprintf("BODY[TEXT] {%d}\r\n%s", len(bodyText), bodyText)
	default:
		return fmt.Sprintf("%s NIL", origAttr)
	}
}

func getBodyHeader(raw []byte, text, attr string) string {
	parts := strings.SplitN(text, "\r\n\r\n", 2)
	headers := parts[0] + "\r\n"

	return fmt.Sprintf("%s {%d}\r\n%s", attr, len(headers), headers)
}

func splitFetchAttrs(attrs string) []string {
	var result []string
	current := strings.Builder{}
	depth := 0
	inQuote := false

	for i := 0; i < len(attrs); i++ {
		c := attrs[i]
		switch {
		case c == '"':
			inQuote = !inQuote
			current.WriteByte(c)
		case c == '(' && !inQuote:
			depth++
			current.WriteByte(c)
		case c == ')' && !inQuote:
			depth--
			current.WriteByte(c)
		case c == ' ' && !inQuote && depth == 0:
			if current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
			}
		default:
			current.WriteByte(c)
		}
	}
	if current.Len() > 0 {
		result = append(result, current.String())
	}

	return result
}

func parseMessageSet(msgSet string, exists int, uidMode bool, messages []imapMessage) []int {
	var result []int
	msgSet = strings.TrimSpace(msgSet)

	if msgSet == "*" {
		for i := 1; i <= exists; i++ {
			result = append(result, i)
		}
		return result
	}

	// Handle comma-separated ranges
	ranges := strings.Split(msgSet, ",")
	for _, r := range ranges {
		r = strings.TrimSpace(r)
		if strings.Contains(r, ":") {
			parts := strings.SplitN(r, ":", 2)
			start, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
			end, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
			if err1 != nil || err2 != nil {
				continue
			}
			if start > end {
				start, end = end, start
			}
			for i := start; i <= end; i++ {
				if i <= exists {
					result = append(result, i)
				}
			}
		} else {
			n, err := strconv.Atoi(r)
			if err != nil {
				continue
			}
			if n >= 1 && n <= exists {
				result = append(result, n)
			}
		}
	}

	return result
}
