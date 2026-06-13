package imap

import (
	"bytes"
	"errors"
	"fmt"
	"net"
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
	attr = strings.Trim(attr, "()")

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

type fetchLiteral struct {
	tag  string // response attribute tag (e.g., "BODY[]")
	data []byte // literal data bytes; nil means tag is a plain string
}

func sendCustomFetch(conn net.Conn, seq int, msg imapMessage, attrs string) {
	attrs = strings.Trim(attrs, "()")
	attrList := splitFetchAttrs(attrs)

	uidIncluded := false
	var allParts []fetchLiteral

	for _, a := range attrList {
		a = strings.ToUpper(a)
		var lit fetchLiteral
		switch {
		case a == "FLAGS":
			lit = fetchLiteral{tag: fmt.Sprintf("FLAGS (%s)", formatFlags(msg.Flags))}
		case a == "INTERNALDATE":
			lit = fetchLiteral{tag: fmt.Sprintf("INTERNALDATE %s", formatInternalDate(msg.Created))}
		case a == "RFC822.SIZE":
			lit = fetchLiteral{tag: fmt.Sprintf("RFC822.SIZE %d", msg.Size)}
		case a == "UID":
			uidIncluded = true
			lit = fetchLiteral{tag: fmt.Sprintf("UID %d", msg.UID)}
		case a == "ENVELOPE":
			lit = fetchLiteral{tag: fmt.Sprintf("ENVELOPE %s", getEnvelope(msg.ID))}
		case a == "BODYSTRUCTURE":
			lit = fetchLiteral{tag: fmt.Sprintf("BODYSTRUCTURE %s", getBodyStructure(msg.ID))}
		case a == "BODY" || strings.HasPrefix(a, "BODY["):
			lit = getBodyPart(msg.ID, a)
		case a == "BODY.PEEK" || strings.HasPrefix(a, "BODY.PEEK["):
			peekAttr := strings.Replace(a, "BODY.PEEK", "BODY", 1)
			lit = getBodyPart(msg.ID, peekAttr)
		case strings.HasPrefix(a, "RFC822"):
			switch a {
			case "RFC822.HEADER":
				lit = getBodyPart(msg.ID, "BODY[HEADER]")
			case "RFC822.TEXT":
				lit = getBodyPart(msg.ID, "BODY[TEXT]")
			default:
				lit = getBodyPart(msg.ID, "BODY[]")
			}
		}
		if lit.tag != "" {
			allParts = append(allParts, lit)
		}
	}

	if !uidIncluded {
		allParts = append(allParts, fetchLiteral{tag: fmt.Sprintf("UID %d", msg.UID)})
	}

	if len(allParts) == 0 {
		return
	}

	// Write FETCH response directly to connection — log summary without binary data
	logger.Log().Debugf("[imap] fetch response: seq=%d uid=%d", seq, msg.UID)
	fmt.Fprintf(conn, "* %d FETCH (", seq)
	first := true
	for _, p := range allParts {
		if !first {
			fmt.Fprintf(conn, " ")
		}
		if p.data == nil {
			fmt.Fprintf(conn, "%s", p.tag)
		} else {
			fmt.Fprintf(conn, "%s {%d}\r\n", p.tag, len(p.data))
			if len(p.data) > 0 {
				if _, err := conn.Write(p.data); err != nil {
					return
				}
			}
		}
		first = false
	}
	fmt.Fprintf(conn, ")\r\n")
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
	prefix := header + ":"
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, prefix) && !strings.HasPrefix(strings.ToUpper(line), strings.ToUpper(prefix)) {
			continue
		}
		var val strings.Builder
		val.WriteString(strings.TrimSpace(line[len(prefix):]))
		for j := i + 1; j < len(lines); j++ {
			cont := strings.TrimRight(lines[j], "\r")
			if cont == "" || (cont[0] != ' ' && cont[0] != '\t') {
				break
			}
			val.WriteString(" ")
			val.WriteString(strings.TrimSpace(cont))
		}
		return val.String()
	}
	return ""
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

	ctValue := contentType
	if idx := strings.Index(contentType, ";"); idx >= 0 {
		ctValue = strings.TrimSpace(contentType[:idx])
	}

	if idx := strings.Index(ctValue, "/"); idx >= 0 {
		mimeType = strings.ToLower(strings.TrimSpace(ctValue[:idx]))
		subType = strings.ToLower(strings.TrimSpace(ctValue[idx+1:]))
	} else {
		mimeType = strings.ToLower(strings.TrimSpace(ctValue))
		subType = "plain"
	}

	// Extract Content-Type parameters (excluding boundary for multipart listing)
	typeParams := extractContentTypeParams(text)
	contentID := extractHeader(text, "Content-ID")
	contentDesc := extractHeader(text, "Content-Description")
	contentEncoding := extractHeader(text, "Content-Transfer-Encoding")
	if contentEncoding == "" {
		contentEncoding = "7bit"
	}

	if mimeType == "multipart" {
		boundary := extractBoundary(text)
		if boundary != "" {
			// Find the body (after header/body separator)
			headerEnd := strings.Index(text, "\r\n\r\n")
			if headerEnd < 0 {
				return "NIL"
			}
			body := text[headerEnd+4:]

			bodyParts := splitMultipartBody(body, boundary)
			var children []string
			for _, part := range bodyParts {
				children = append(children, buildBodyStructure([]byte(part)))
			}
			// Multipart body structure: (children subtype)
			// Per RFC 3501: (child1 child2 ... "subtype" (params) NIL NIL NIL)
			return fmt.Sprintf("(%s \"%s\" %s NIL NIL NIL)",
				strings.Join(children, " "), subType, typeParams)
		}
	}

	// Use body-only (strip headers) for octet count and line count per RFC 3501
	body := bodyFromRaw(raw)
	bodyLines := countLines(body)
	bodyOctets := len(body)

	if mimeType == "text" {
		return fmt.Sprintf("(\"%s\" \"%s\" %s NIL %s %s \"%s\" %d %d NIL NIL NIL)",
			mimeType, subType, typeParams,
			nilOrString(contentID),
			nilOrString(contentDesc),
			contentEncoding, bodyLines, bodyOctets)
	}

	return fmt.Sprintf("(\"%s\" \"%s\" %s NIL %s %s \"%s\" %d NIL NIL NIL)",
		mimeType, subType, typeParams,
		nilOrString(contentID),
		nilOrString(contentDesc),
		contentEncoding, bodyOctets)
}

// bodyFromRaw returns the body portion (after headers) of a raw message or MIME part.
func bodyFromRaw(raw []byte) []byte {
	idx := bytes.Index(raw, []byte("\r\n\r\n"))
	if idx < 0 {
		return raw
	}
	return raw[idx+4:]
}

// extractBodyFromText returns the body portion of a text message.
func extractBodyFromText(text string) string {
	if idx := strings.Index(text, "\r\n\r\n"); idx >= 0 {
		return text[idx+4:]
	}
	return text
}

// isMIMEHeader returns true if the field name is a MIME header field.
func isMIMEHeader(field string) bool {
	switch strings.ToLower(field) {
	case "content-type", "content-transfer-encoding", "content-id",
		"content-description", "content-disposition", "mime-version":
		return true
	}
	return false
}

// filterMIMEHeaders returns only the MIME header fields from the given raw headers.
func filterMIMEHeaders(headers string) string {
	var result []string
	lines := strings.Split(headers, "\r\n")
	var inContinuation bool
	for _, line := range lines {
		if line == "" {
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			if inContinuation {
				result = append(result, line)
			}
			continue
		}
		fieldName := strings.TrimSpace(line[:colonIdx])
		if isMIMEHeader(fieldName) {
			result = append(result, line)
			inContinuation = true
		} else {
			inContinuation = false
		}
	}
	return strings.Join(result, "\r\n")
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
				kvs = append(kvs, fmt.Sprintf("\"%s\" %s", k, nilOrString(v)))
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

func getBodyPart(id, attr string) fetchLiteral {
	raw, err := storage.GetMessageRaw(id)
	if err != nil {
		return fetchLiteral{tag: "NIL"}
	}

	text := string(raw)
	origAttr := attr
	attr = strings.ToUpper(attr)

	// Check for partial range <offset.size>
	baseAttr, offset, size, isPartial := parsePartialRange(attr)

	// Extract section from baseAttr (e.g., "BODY[1]" → "1")
	section := extractSection(baseAttr)

	switch {
	case section == "" && (baseAttr == "BODY[]" || baseAttr == "BODY"):
		// BODY[] or BODY — entire message
		if isPartial {
			if offset < 0 {
				offset = 0
			}
			if offset >= len(raw) {
				return fetchLiteral{tag: fmt.Sprintf("%s {%d}", origAttr, 0), data: []byte{}}
			}
			if size < 0 || offset+size > len(raw) {
				size = len(raw) - offset
			}
			sliced := raw[offset : offset+size]
			return fetchLiteral{tag: origAttr, data: sliced}
		}
		return fetchLiteral{tag: "BODY[]", data: raw}

	case section == "HEADER" || strings.HasPrefix(section, "HEADER.FIELDS"):
		// BODY[HEADER] or BODY[HEADER.FIELDS (...)]
		tag, data := getBodyHeader(raw, text, attr)
		if isPartial {
			return fetchLiteral{tag: fmt.Sprintf("%s {%d}", origAttr, 0), data: []byte{}}
		}
		return fetchLiteral{tag: tag, data: []byte(data)}

	case section == "MIME":
		// BODY[MIME] — MIME headers of top-level message
		parts := strings.SplitN(text, "\r\n\r\n", 2)
		mimeHdrs := filterMIMEHeaders(parts[0])
		return fetchLiteral{tag: "BODY[MIME]", data: []byte(mimeHdrs + "\r\n")}

	case section == "TEXT":
		// BODY[TEXT] — body text
		parts := strings.SplitN(text, "\r\n\r\n", 2)
		if len(parts) < 2 {
			return fetchLiteral{tag: fmt.Sprintf("BODY[TEXT] {%d}", 0), data: []byte{}}
		}
		bodyText := parts[1]
		if isPartial {
			if offset < 0 {
				offset = 0
			}
			if offset >= len(bodyText) {
				return fetchLiteral{tag: fmt.Sprintf("%s {%d}", origAttr, 0), data: []byte{}}
			}
			if size < 0 || offset+size > len(bodyText) {
				size = len(bodyText) - offset
			}
			sliced := bodyText[offset : offset+size]
			return fetchLiteral{tag: origAttr, data: []byte(sliced)}
		}
		return fetchLiteral{tag: "BODY[TEXT]", data: []byte(bodyText)}

	case isNumericSection(section) || hasNumericPrefix(section):
		// BODY[1], BODY[2], BODY[1.1], BODY[1.HEADER], BODY[1.TEXT], etc.
		content, err := getMIMEPartContent(raw, section)
		if err != nil {
			return fetchLiteral{tag: fmt.Sprintf("%s NIL", origAttr)}
		}
		if isPartial {
			if offset < 0 {
				offset = 0
			}
			if offset >= len(content) {
				return fetchLiteral{tag: fmt.Sprintf("%s {%d}", origAttr, 0), data: []byte{}}
			}
			if size < 0 || offset+size > len(content) {
				size = len(content) - offset
			}
			sliced := content[offset : offset+size]
			return fetchLiteral{tag: origAttr, data: sliced}
		}
		return fetchLiteral{tag: origAttr, data: content}

	default:
		return fetchLiteral{tag: fmt.Sprintf("%s NIL", origAttr)}
	}
}

func getBodyHeader(raw []byte, text, attr string) (string, string) {
	parts := strings.SplitN(text, "\r\n\r\n", 2)
	headers := parts[0]

	// Filter headers if HEADER.FIELDS or HEADER.FIELDS.NOT
	if strings.Contains(attr, "HEADER.FIELDS") {
		headers = filterRequestedHeaders(headers, attr)
	}

	headers += "\r\n"

	return attr, headers
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
		case (c == '(' || c == '[') && !inQuote:
			depth++
			current.WriteByte(c)
		case (c == ')' || c == ']') && !inQuote:
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

// extractSection returns the section specifier inside brackets.
// "BODY[HEADER]" → "HEADER", "BODY[1]" → "1", "BODY[]" → ""
func extractSection(attr string) string {
	start := strings.Index(attr, "[")
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(attr, "]")
	if end <= start {
		return ""
	}
	return attr[start+1 : end]
}

// isNumericSection returns true if the section is a numeric path like "1", "1.2", etc.
// excluding sub-section keywords HEADER, TEXT, MIME.
func isNumericSection(section string) bool {
	if section == "" {
		return false
	}
	parts := strings.Split(section, ".")
	for _, p := range parts {
		if p == "HEADER" || p == "TEXT" || p == "MIME" {
			return false
		}
		if _, err := strconv.Atoi(p); err != nil {
			return false
		}
	}
	return true
}

// hasNumericPrefix returns true if the section starts with a number
// (e.g. "1.HEADER", "1.TEXT", "2.1.MIME").
func hasNumericPrefix(section string) bool {
	if section == "" {
		return false
	}
	first := strings.SplitN(section, ".", 2)[0]
	_, err := strconv.Atoi(first)
	return err == nil
}

// getMIMEPartContent extracts content for a MIME part section like "1", "1.2", "1.HEADER", etc.
func getMIMEPartContent(raw []byte, section string) ([]byte, error) {
	if section == "" {
		return raw, nil
	}

	path := strings.Split(section, ".")
	current := raw

	for i, step := range path {
		// Sub-section keywords: HEADER, TEXT, MIME
		if step == "HEADER" || step == "MIME" {
			headerEnd := findHeaderEnd(current)
			if headerEnd < 0 {
				return current, nil
			}
			hdrs := current[:headerEnd]
			if step == "MIME" {
				return []byte(filterMIMEHeaders(string(hdrs)) + "\r\n"), nil
			}
			return hdrs, nil
		}
		if step == "TEXT" {
			headerEnd := findHeaderEnd(current)
			if headerEnd < 0 {
				return nil, errors.New("no body found")
			}
			current = current[headerEnd:]
			continue
		}

		partNum, err := strconv.Atoi(step)
		if err != nil {
			return nil, fmt.Errorf("invalid section number: %s", step)
		}

		boundary := findBoundary(string(current))
		if boundary == "" {
			// Not multipart — only part 1 is valid
			if partNum != 1 {
				return nil, errors.New("part not found in non-multipart message")
			}
			// Skip to body
			headerEnd := findHeaderEnd(current)
			if headerEnd < 0 {
				return nil, errors.New("no body found")
			}
			current = current[headerEnd:]
			if isLastStep(path, i) {
				// If no sub-section keyword follows, this is the final section — return the current content as-is
				return current, nil
			}
			continue
		}

		// Multipart — find the body and split by boundary
		body := current
		if hdrEnd := findHeaderEnd(body); hdrEnd >= 0 {
			body = body[hdrEnd:]
		}

		parts := splitMultipartBody(string(body), boundary)
		if partNum < 1 || partNum > len(parts) {
			return nil, fmt.Errorf("part %d not found", partNum)
		}

		current = []byte(parts[partNum-1])

		if isLastStep(path, i) {
			// If the next token in the original section (not path) is HEADER/TEXT/MIME,
			// it would have been handled above. Since we got here, return as-is.
			return current, nil
		}
	}

	return current, nil
}

func isLastStep(path []string, i int) bool {
	return i == len(path)-1
}

func findHeaderEnd(data []byte) int {
	idx := bytes.Index(data, []byte("\r\n\r\n"))
	if idx < 0 {
		return -1
	}
	return idx + 4
}

// findBoundary extracts the boundary parameter from Content-Type.
func findBoundary(text string) string {
	ct := extractHeader(text, "Content-Type")
	if ct == "" {
		return ""
	}
	return extractBoundaryValue(ct)
}

func extractBoundaryValue(ct string) string {
	idx := strings.Index(ct, "boundary=")
	if idx < 0 {
		return ""
	}
	b := ct[idx+9:]
	if strings.HasPrefix(b, "\"") {
		b = strings.Trim(b, "\"")
	} else {
		if idx2 := strings.IndexAny(b, " ;"); idx2 > 0 {
			b = b[:idx2]
		}
	}
	return strings.TrimSpace(b)
}

// splitMultipartBody splits a multipart body into individual MIME parts,
// returning each part with its headers.
func splitMultipartBody(body, boundary string) []string {
	delim := "--" + boundary
	parts := strings.Split(body, delim)
	var result []string
	for i, part := range parts {
		// Skip preamble/prologue (first element before first boundary per RFC 2046)
		if i == 0 {
			continue
		}
		part = strings.TrimLeft(part, "\r\n")

		// Strip trailing -- for closing boundary
		if strings.HasSuffix(part, "--") {
			part = strings.TrimSuffix(part, "--")
		}
		if strings.HasSuffix(part, "--\r\n") {
			part = strings.TrimSuffix(part, "--\r\n")
		}

		part = strings.TrimRight(part, "\r\n")
		if part != "" && part != "--" && part != "-" {
			result = append(result, part)
		}
	}
	return result
}

// filterRequestedHeaders filters headers to return only the requested fields.
// attr contains the full attribute like BODY[HEADER.FIELDS (DATE FROM)].
func filterRequestedHeaders(headers, attr string) string {
	parenStart := strings.Index(attr, "(")
	parenEnd := strings.LastIndex(attr, ")")
	if parenStart < 0 || parenEnd <= parenStart {
		return headers
	}

	fieldList := attr[parenStart+1 : parenEnd]
	fields := strings.Fields(fieldList)
	if len(fields) == 0 {
		return headers
	}

	isNot := strings.Contains(attr, "HEADER.FIELDS.NOT")

	var result []string
	lines := strings.Split(headers, "\r\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		if line == "" {
			i++
			continue
		}
		colonIdx := strings.Index(line, ":")
		if colonIdx < 0 {
			// Continuation line (folded header) — include if we're collecting
			if len(result) > 0 {
				result = append(result, line)
			}
			i++
			continue
		}

		fieldName := strings.TrimSpace(line[:colonIdx])
		matched := false
		for _, f := range fields {
			if strings.EqualFold(fieldName, f) {
				matched = true
				break
			}
		}

		if isNot {
			if !matched {
				result = append(result, line)
				// Include continuation lines
				i++
				for i < len(lines) && lines[i] != "" && (lines[i][0] == ' ' || lines[i][0] == '\t') {
					result = append(result, lines[i])
					i++
				}
				continue
			}
		} else {
			if matched {
				result = append(result, line)
				// Include continuation lines
				i++
				for i < len(lines) && lines[i] != "" && (lines[i][0] == ' ' || lines[i][0] == '\t') {
					result = append(result, lines[i])
					i++
				}
				continue
			}
		}
		i++
	}

	return strings.Join(result, "\r\n")
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
