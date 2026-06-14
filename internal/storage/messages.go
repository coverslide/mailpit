package storage

import (
	"bytes"
	"context"
	"crypto/md5"  // #nosec
	"crypto/sha1" // #nosec
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"sort"
	"strings"
	"time"

	"github.com/axllent/mailpit/config"
	"github.com/axllent/mailpit/internal/logger"
	"github.com/axllent/mailpit/internal/shortuuid"
	"github.com/axllent/mailpit/internal/tools"
	"github.com/axllent/mailpit/server/webhook"
	"github.com/axllent/mailpit/server/websockets"
	"github.com/jhillyerd/enmime/v2"
	"github.com/leporo/sqlf"
)

// Store will save an email to the database tables.
// The username is the authentication username of either the SMTP or HTTP client (blank for none).
// The folder is the IMAP folder to store the message in (empty string for INBOX, "Sent", "Trash", "Drafts").
// Returns the database ID of the saved message.
func Store(body *[]byte, username *string, placement ...string) (string, error) {
	folder := ""
	if len(placement) > 0 {
		folder = placement[0]
	}
	var userID []string
	if len(placement) > 1 {
		userID = placement[1:]
	}
	// Parse message body with enmime
	env, err := storeParser.ReadEnvelope(bytes.NewReader(*body))
	if err != nil {
		logger.Log().Warnf("[message] %s", err.Error())
		// the message can never be parsed, so reject it permanently rather than have the client retry
		return "", errors.New("554 5.6.0 Unable to parse message")
	}

	from := &mail.Address{}
	fromJSON := addressToSlice(env, "From")
	if len(fromJSON) > 0 {
		from = fromJSON[0]
	} else if env.GetHeader("From") != "" {
		from = &mail.Address{Name: env.GetHeader("From")}
	}

	obj := Metadata{
		From:    from,
		To:      addressToSlice(env, "To"),
		Cc:      addressToSlice(env, "Cc"),
		Bcc:     addressToSlice(env, "Bcc"),
		ReplyTo: addressToSlice(env, "Reply-To"),
	}
	if username != nil {
		obj.Username = *username
	}

	messageID := strings.Trim(env.GetHeader("Message-ID"), "<>")
	created := time.Now()

	// use message date instead of created date
	if config.UseMessageDates {
		if mDate, err := env.Date(); err == nil {
			created = mDate
		}
	}

	// generate the search text & snippet
	searchText, snippet := searchTextAndSnippet(env)

	// generate unique ID
	id := shortuuid.New()

	summaryJSON, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}

	subject := env.GetHeader("Subject")

	metaSearchText, err := createMetaSearchText(obj, subject)
	if err != nil {
		return "", err
	}

	// begin a transaction to ensure both the message
	// and data are stored successfully
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}

	// roll back if it fails
	defer func() { _ = tx.Rollback() }()

	size := uint64(len(*body))
	inline := len(env.Inlines)
	attachments := len(env.Attachments)

	sql := fmt.Sprintf(`INSERT INTO %s
	(Created, ID, MessageID, Subject, Metadata, Size, Inline, Attachments, SearchText, MetaSearchText, Read, Snippet, Folder, UserID)
	    VALUES(?,?,?,?,?,?,?,?,?,?,0,?,?,?)`,
		tenant("mailbox"),
	) // #nosec

	// insert mail summary data
	uid := ""
	if len(userID) > 0 {
		uid = userID[0]
	}
	_, err = tx.Exec(sql, created.UnixMilli(), id, messageID, subject, string(summaryJSON), size, inline, attachments, searchText, metaSearchText, snippet, folder, uid)
	if err != nil {
		return "", err
	}

	if config.Compression > 0 {
		// insert compressed raw message
		compressed := dbEncoder.EncodeAll(*body, make([]byte, 0, size))

		if sqlDriver == "rqlite" {
			// rqlite does not support binary data in query, so we need to encode the compressed message into hexadecimal
			// string and then generate the SQL query, which is more memory intensive, especially with large messages
			hexStr := hex.EncodeToString(compressed)
			_, err = tx.Exec(fmt.Sprintf(`INSERT INTO %s (ID, Email, Compressed) VALUES(?, x'%s', 1)`, tenant("mailbox_data"), hexStr), id) // #nosec
		} else {
			_, err = tx.Exec(fmt.Sprintf(`INSERT INTO %s (ID, Email, Compressed) VALUES(?, ?, 1)`, tenant("mailbox_data")), id, compressed) // #nosec
		}
	} else {
		// insert uncompressed raw message
		_, err = tx.Exec(fmt.Sprintf(`INSERT INTO %s (ID, Email, Compressed) VALUES(?, ?, 0)`, tenant("mailbox_data")), id, string(*body)) // #nosec
	}

	if err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", err
	}

	// The tags are collected in order of priority: the operator's own tag filters first, then the
	// authenticated username, then plus-addresses and X-Tags last, as the content of a message is
	// the least trusted source. A message can only have MaxTagsPerMessage tags, so the first
	// ones are kept and anything a message adds beyond that cannot displace the tags above it.

	// extract tags using pre-set tag filters, empty slice if not set
	tags := findTagsInRawMessage(body)

	// extract tags from search matches
	tags = append(tags, tagFilterMatches(id)...)

	// auto-tag by username if enabled
	if config.TagsUsername && username != nil && *username != "" {
		tags = append(tags, *username)
	}

	if !config.TagsDisablePlus {
		// get tags from plus-addresses
		tags = append(tags, obj.tagsFromPlusAddresses()...)
	}

	if !config.TagsDisableXTags {
		xTagsHdr := env.GetHeader("X-Tags")
		if xTagsHdr != "" {
			tags = append(tags, tools.SetTagCasing(xTagsFromHeader(xTagsHdr))...)
		}
	}

	// remove invalid and duplicate tags, then keep the highest priority ones and sort them
	tags = uniqueTags(tags)
	if len(tags) > MaxTagsPerMessage {
		logger.Log().Warnf("[tags] message %s has %d tags, only the first %d are kept", id, len(tags), MaxTagsPerMessage)
		tags = tags[:MaxTagsPerMessage]
	}
	sort.Strings(tags)

	setTags := []string{}
	if len(tags) > 0 {
		setTags, err = SetMessageTags(id, tags)
		if err != nil {
			return "", err
		}
	}

	// Build the summary directly from obj rather than round-tripping through JSON.
	// We do not want to broadcast null values for metadata else this does not align
	// with the message summary documented in the API docs, so we set them to empty slices.
	c := &MessageSummary{
		Created:     created,
		ID:          id,
		MessageID:   messageID,
		From:        obj.From,
		To:          obj.To,
		Cc:          obj.Cc,
		Bcc:         obj.Bcc,
		ReplyTo:     obj.ReplyTo,
		Username:    obj.Username,
		Subject:     subject,
		Attachments: attachments,
		Size:        size,
		Tags:        setTags,
		Snippet:     snippet,
	}
	if c.From == nil {
		c.From = &mail.Address{}
	}
	if c.To == nil {
		c.To = []*mail.Address{}
	}
	if c.Cc == nil {
		c.Cc = []*mail.Address{}
	}
	if c.Bcc == nil {
		c.Bcc = []*mail.Address{}
	}
	if c.ReplyTo == nil {
		c.ReplyTo = []*mail.Address{}
	}

	websockets.Broadcast("new", c)
	webhook.Send(c)

	dbLastAction.touch()

	BroadcastMailboxStats()

	// Notify IDLEing IMAP clients about the new message in the target folder.
	uid = ""
	if len(userID) > 0 {
		uid = userID[0]
	}
	if uid != "" {
		// Per-user notification: only notify connections for this specific user.
		if total, _, _, err := MailboxCounts(folder, uid); err == nil {
			IdleNotify.NotifyFolderChangeUser(uid, folder, int(total))
		}
	} else {
		// Shared message: notify all IDLEing connections.
		if total, _, _, err := MailboxCounts(folder); err == nil {
			IdleNotify.NotifyFolderChange(folder, int(total))
		}
	}

	logger.Log().Debugf("[db] saved message %s (%d bytes)", id, size)

	return id, nil
}

// UpdateCreated overrides the Created timestamp for a stored message.
// Used by IMAP APPEND to preserve the client-provided date-time.
func UpdateCreated(id string, t time.Time) error {
	q := fmt.Sprintf("UPDATE %s SET Created = ? WHERE ID = ?", tenant("mailbox"))
	_, err := db.Exec(q, t.UnixMilli(), id)
	return err
}

// List returns a subset of messages from the mailbox,
// sorted latest to oldest.
// Pass folder="" for INBOX (default), "Sent", "Trash", or "Drafts".
func List(start int, beforeTS int64, limit int, folder string, userID ...string) ([]MessageSummary, error) {
	results := []MessageSummary{}
	tsStart := time.Now()

	q := sqlf.From(tenant("mailbox") + " m").
		Select(`m.Created, m.ID, m.MessageID, m.Subject, m.Metadata, m.Size, m.Attachments, m.Read, m.Snippet`).
		OrderBy("m.Created DESC, m.rowid DESC")

	if folder != "" {
		q = q.Where("m.Folder = ?", folder)
	} else {
		q = q.Where("m.Folder = ''")
	}

	// Per-user inbox filtering: show only messages owned by this user.
	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("m.UserID = ?", userID[0])
	}

	if limit > 0 {
		q = q.Limit(limit).Offset(start)
	}

	if beforeTS > 0 {
		q = q.Where("Created < ?", beforeTS)
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var created float64 // use float64 for rqlite compatibility
		var id string
		var messageID string
		var subject string
		var metadataJSON string
		var size float64 // use float64 for rqlite compatibility
		var attachments int
		var read int
		var snippet string
		em := MessageSummary{}
		var meta Metadata

		err := row.Scan(&created, &id, &messageID, &subject, &metadataJSON, &size, &attachments, &read, &snippet)
		if err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}

		if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
			logger.Log().Errorf("[json] %s", err.Error())
			return
		}

		em.From = meta.From
		em.To = meta.To
		em.Cc = meta.Cc
		em.Bcc = meta.Bcc
		em.ReplyTo = meta.ReplyTo
		em.Username = meta.Username

		em.Created = time.UnixMilli(int64(created))
		em.ID = id
		em.MessageID = messageID
		em.Subject = subject
		em.Size = uint64(size)
		em.Attachments = attachments
		em.Read = read == 1
		em.Snippet = snippet
		// artificially generate ReplyTo if legacy data is missing Reply-To field
		if em.ReplyTo == nil {
			em.ReplyTo = []*mail.Address{}
		}

		results = append(results, em)
	}); err != nil {
		return results, err
	}

	// set tags for listed messages only
	if len(results) > 0 {
		ids := make([]string, len(results))
		for i, m := range results {
			ids[i] = m.ID
		}
		tagMap := getTagsForIDs(ids)
		for i, m := range results {
			if tags, ok := tagMap[m.ID]; ok {
				results[i].Tags = tags
			} else {
				results[i].Tags = []string{}
			}
		}
	}

	dbLastAction.touch()

	elapsed := time.Since(tsStart)

	logger.Log().Debugf("[db] list INBOX in %s", elapsed)

	return results, nil
}

// MailboxCounts returns the total, unread, and next UID for a folder.
// Pass folder="" for INBOX.
func MailboxCounts(folder string, userID ...string) (total, unread, nextUID int64, err error) {
	var totalF, unreadF float64

	q := sqlf.From(tenant("mailbox")).Select("COUNT(*)").To(&totalF)
	if folder != "" {
		q = q.Where("Folder = ?", folder)
	} else {
		q = q.Where("Folder = ''")
	}
	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}
	if err = q.QueryRowAndClose(context.TODO(), db); err != nil {
		return 0, 0, 1, err
	}

	q2 := sqlf.From(tenant("mailbox")).Select("COUNT(*)").To(&unreadF).
		Where("Read = ?", 0)
	if folder != "" {
		q2 = q2.Where("Folder = ?", folder)
	} else {
		q2 = q2.Where("Folder = ''")
	}
	if len(userID) > 0 && userID[0] != "" {
		q2 = q2.Where("UserID = ?", userID[0])
	}
	if err = q2.QueryRowAndClose(context.TODO(), db); err != nil {
		return 0, 0, 1, err
	}

	return int64(totalF), int64(unreadF), int64(totalF) + 1, nil
}

// GetMessage returns a Message generated from the mailbox_data collection.
// If the message lacks a date header, then the received datetime is used.
func GetMessage(id string) (*Message, error) {
	env, rawSize, err := getCachedEnvelope(id)
	if err != nil {
		return nil, err
	}

	// Load metadata from DB
	meta, err := GetMetadata(id)
	if err != nil {
		meta = Metadata{}
	}

	from := meta.From
	if from == nil {
		fromData := addressToSlice(env, "From")
		if len(fromData) > 0 {
			from = fromData[0]
		} else if env.GetHeader("From") != "" {
			from = &mail.Address{Name: env.GetHeader("From")}
		}
	}

	messageID := strings.Trim(env.GetHeader("Message-ID"), "<>")

	returnPath := strings.Trim(env.GetHeader("Return-Path"), "<>")
	if returnPath == "" && from != nil {
		returnPath = from.Address
	}

	date, err := env.Date()
	if err != nil {
		// return received datetime when message does not contain a date header
		q := sqlf.From(tenant("mailbox")).
			Select(`Created`).
			Where(`ID = ?`, id)

		if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
			var created float64 // use float64 for rqlite compatibility

			if err := row.Scan(&created); err != nil {
				logger.Log().Errorf("[db] %s", err.Error())
				return
			}

			logger.Log().Debugf("[db] %s does not contain a date header, using received datetime", id)
			date = time.UnixMilli(int64(created))
		}); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
		}
	}

	obj := Message{
		ID:         id,
		MessageID:  messageID,
		From:       from,
		Date:       date,
		To:         addressToSlice(env, "To"),
		Cc:         addressToSlice(env, "Cc"),
		Bcc:        addressToSlice(env, "Bcc"),
		ReplyTo:    addressToSlice(env, "Reply-To"),
		ReturnPath: returnPath,
		Subject:    env.GetHeader("Subject"),
		Tags:       getMessageTags(id),
		Size:       rawSize,
		Text:       env.Text,
		Username:   meta.Username,
	}
	obj.HTML = env.HTML
	obj.Inline = []Attachment{}
	obj.Attachments = []Attachment{}

	for _, i := range env.Inlines {
		if i.FileName != "" || i.ContentID != "" {
			obj.Inline = append(obj.Inline, AttachmentSummary(i))
		}
	}

	for _, i := range env.OtherParts {
		if i.FileName != "" || i.ContentID != "" {
			obj.Inline = append(obj.Inline, AttachmentSummary(i))
		}
	}

	for _, a := range env.Attachments {
		if a.FileName != "" || a.ContentID != "" {
			obj.Attachments = append(obj.Attachments, AttachmentSummary(a))
		}
	}

	// get List-Unsubscribe links if set
	obj.ListUnsubscribe = ListUnsubscribe{}
	obj.ListUnsubscribe.Links = []string{}
	if env.GetHeader("List-Unsubscribe") != "" {
		l := env.GetHeader("List-Unsubscribe")
		links, err := tools.ListUnsubscribeParser(l)
		obj.ListUnsubscribe.Header = l
		obj.ListUnsubscribe.Links = links
		if err != nil {
			obj.ListUnsubscribe.Errors = err.Error()
		}
		obj.ListUnsubscribe.HeaderPost = env.GetHeader("List-Unsubscribe-Post")
	}

	// mark message as read
	if err := MarkRead([]string{id}); err != nil {
		return &obj, err
	}

	dbLastAction.touch()

	return &obj, nil
}

// GetMessageRaw returns an []byte of the full message
func GetMessageRaw(id string) ([]byte, error) {
	var i, msg string
	var compressed int
	q := sqlf.From(tenant("mailbox_data")).
		Select(`ID`).To(&i).
		Select(`Email`).To(&msg).
		Select(`Compressed`).To(&compressed).
		Where(`ID = ?`, id)
	err := q.QueryRowAndClose(context.Background(), db)
	if err != nil {
		return nil, err
	}

	if i == "" {
		return nil, errors.New("message not found")
	}

	var data []byte
	if sqlDriver == "rqlite" && compressed == 1 {
		data, err = base64.StdEncoding.DecodeString(msg)
		if err != nil {
			return nil, fmt.Errorf("error decoding base64 message: %w", err)
		}
	} else {
		data = []byte(msg)
	}

	dbLastAction.touch()

	if compressed == 1 {
		raw, err := dbDecoder.DecodeAll(data, nil)
		if err != nil {
			return nil, fmt.Errorf("error decompressing message: %s", err.Error())
		}

		return raw, err
	}

	return data, nil
}

// GetAttachmentPart returns an *enmime.Part (attachment or inline) from a message
func GetAttachmentPart(id, partID string) (*enmime.Part, error) {
	env, _, err := getCachedEnvelope(id)
	if err != nil {
		return nil, err
	}

	for _, a := range env.Inlines {
		if a.PartID == partID {
			return a, nil
		}
	}

	for _, a := range env.OtherParts {
		if a.PartID == partID {
			return a, nil
		}
	}

	for _, a := range env.Attachments {
		if a.PartID == partID {
			return a, nil
		}
	}

	dbLastAction.touch()

	return nil, errors.New("attachment not found")
}

// AttachmentSummary returns a summary of the attachment without any binary data
func AttachmentSummary(a *enmime.Part) Attachment {
	o := Attachment{}
	o.PartID = a.PartID
	o.FileName = a.FileName
	if o.FileName == "" {
		o.FileName = a.ContentID
	}
	o.ContentType = a.ContentType
	o.ContentID = a.ContentID
	o.Size = uint64(len(a.Content))

	md5Hash := md5.Sum(a.Content)   // #nosec
	sha1Hash := sha1.Sum(a.Content) // #nosec
	sha256Hash := sha256.Sum256(a.Content)

	o.Checksums.MD5 = hex.EncodeToString(md5Hash[:])
	o.Checksums.SHA1 = hex.EncodeToString(sha1Hash[:])
	o.Checksums.SHA256 = hex.EncodeToString(sha256Hash[:])

	return o
}

// LatestID returns the latest message ID
//
// If a query argument is set in the request the function will return the
// latest message matching the search
func LatestID(r *http.Request) (string, error) {
	var messages []MessageSummary
	var err error

	search := strings.TrimSpace(r.URL.Query().Get("query"))
	if search != "" {
		messages, _, err = Search(search, r.URL.Query().Get("tz"), 0, 0, 1)
		if err != nil {
			return "", err
		}
	} else {
		messages, err = List(0, 0, 1, "")
		if err != nil {
			return "", err
		}
	}
	if len(messages) == 0 {
		return "", errors.New("Message not found")
	}

	return messages[0].ID, nil
}

// maxSQLVariables is how many placeholders a statement with a list of IDs may have. SQLite refuses more than
// 32,766 ("too many SQL variables"), and a message list can be as long as the mailbox, so longer lists
// are split. It is a variable for the tests.
var maxSQLVariables = 1000

// inPlaceholders returns the placeholders for an IN clause of n values, eg: "(?,?,?)". n must be at least 1.
func inPlaceholders(n int) string {
	return "(?" + strings.Repeat(",?", n-1) + ")"
}

// idArgs returns the IDs as query arguments.
func idArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}

	return args
}

// MarkRead will mark messages as read
func MarkRead(ids []string) error {
	return setReadStatus(ids, true)
}

// MarkUnread will mark messages as unread
func MarkUnread(ids []string) error {
	return setReadStatus(ids, false)
}

// setReadStatus marks messages as read or unread, however many there are, and tells the web UI about the
// ones which changed.
func setReadStatus(ids []string, read bool) error {
	if len(ids) == 0 {
		return nil
	}

	from, to := 1, 0 // the status the messages have to be in to change, and the one they are set to
	if read {
		from, to = 0, 1
	}

	changed := []string{}

	for _, chunk := range chunkBy(ids, maxSQLVariables) {
		// Find which messages are actually in the other state (will change)
		toUpdate := []string{}
		rows, err := db.Query(fmt.Sprintf(`SELECT ID FROM %s WHERE Read = %d AND ID IN %s`, tenant("mailbox"), from, inPlaceholders(len(chunk))), idArgs(chunk)...) // #nosec
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			toUpdate = append(toUpdate, id)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}

		if len(toUpdate) == 0 {
			continue
		}

		if _, err := db.Exec(fmt.Sprintf(`UPDATE %s SET Read = %d WHERE ID IN %s`, tenant("mailbox"), to, inPlaceholders(len(toUpdate))), idArgs(toUpdate)...); err != nil { // #nosec
			return err
		}

		changed = append(changed, toUpdate...)
	}

	if len(changed) == 0 {
		return nil
	}

	dbLastAction.touch()

	status := "unread"
	if read {
		status = "read"
	}

	for _, id := range changed {
		logger.Log().Debugf("[db] marked message %s as %s", id, status)
		websockets.Broadcast("update", struct {
			ID   string
			Read bool
		}{ID: id, Read: read})
	}

	BroadcastMailboxStats()

	return nil
}

// MarkAllRead will mark all messages as read
func MarkAllRead() error {
	var (
		start = time.Now()
		total = CountUnread()
	)

	_, err := sqlf.Update(tenant("mailbox")).
		Set("Read", 1).
		Where("Read = ?", 0).
		ExecAndClose(context.Background(), db)
	if err != nil {
		return err
	}

	elapsed := time.Since(start)
	logger.Log().Debugf("[db] marked %v messages as read in %s", total, elapsed)

	BroadcastMailboxStats()

	dbLastAction.touch()

	return nil
}

// MarkAllUnread will mark all messages as unread
func MarkAllUnread() error {
	var (
		start = time.Now()
		total = CountRead()
	)

	_, err := sqlf.Update(tenant("mailbox")).
		Set("Read", 0).
		Where("Read = ?", 1).
		ExecAndClose(context.Background(), db)
	if err != nil {
		return err
	}

	elapsed := time.Since(start)
	logger.Log().Debugf("[db] marked %v messages as unread in %s", total, elapsed)

	BroadcastMailboxStats()

	dbLastAction.touch()

	return nil
}

// DeleteMessages deletes one or more messages in bulk
func DeleteMessages(ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	start := time.Now()

	toDelete := []string{}
	var totalSize uint64
	// Track affected folders per user for IDLE notifications.
	type folderUser struct {
		folder string
		user   string
	}
	foldersAffected := make(map[folderUser]bool)

	// only the messages which exist are counted, and a long list is looked up in parts
	for _, chunk := range chunkBy(ids, maxSQLVariables) {
		sql := fmt.Sprintf(`SELECT ID, Size, Folder, UserID FROM %s WHERE ID IN %s`, tenant("mailbox"), inPlaceholders(len(chunk))) // #nosec
		rows, err := db.Query(sql, idArgs(chunk)...)
		if err != nil {
			return err
		}

		for rows.Next() {
			var id string
			var size float64 // use float64 for rqlite compatibility
			var folder string
			var userID string

			if err := rows.Scan(&id, &size, &folder, &userID); err != nil {
				_ = rows.Close()
				return err
			}

			toDelete = append(toDelete, id)
			totalSize = totalSize + uint64(size)
			foldersAffected[folderUser{folder, userID}] = true
		}

		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return err
		}
	}

	if len(toDelete) == 0 {
		return nil // nothing to delete
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	// roll back if it fails
	defer func() { _ = tx.Rollback() }()

	tables := []string{"mailbox", "mailbox_data", "message_tags"}

	for _, chunk := range chunkBy(toDelete, maxSQLVariables) {
		args := idArgs(chunk)

		for _, t := range tables {
			sql := fmt.Sprintf(`DELETE FROM %s WHERE ID IN %s`, tenant(t), inPlaceholders(len(chunk))) // #nosec

			if _, err = tx.Exec(sql, args...); err != nil {
				return err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	dbLastAction.touch()
	addDeletedSize(totalSize)

	logMessagesDeleted(len(toDelete))

	_ = pruneUnusedTags()

	elapsed := time.Since(start)

	messages := "messages"
	if len(toDelete) == 1 {
		messages = "message"
	}

	logger.Log().Debugf("[db] deleted %d %s in %s", len(toDelete), messages, elapsed)

	BroadcastMailboxStats()

	// broadcast individual message deletions
	for _, id := range toDelete {
		invalidateEnvelopeCache(id)

		d := struct {
			ID string
		}{ID: id}

		websockets.Broadcast("delete", d)
	}

	// Notify IDLEing IMAP clients about the deletions in affected folders.
	for fu := range foldersAffected {
		if fu.user != "" {
			if total, _, _, err := MailboxCounts(fu.folder, fu.user); err == nil {
				IdleNotify.NotifyFolderChangeUser(fu.user, fu.folder, int(total))
			}
		} else {
			if total, _, _, err := MailboxCounts(fu.folder); err == nil {
				IdleNotify.NotifyFolderChange(fu.folder, int(total))
			}
		}
	}

	return nil
}

// MoveMessage changes the Folder of a message by ID.
// This is used by IMAP MOVE to atomically move a message between folders
// without creating a duplicate.
func MoveMessage(id, folder string) error {
	if id == "" {
		return fmt.Errorf("message ID cannot be empty")
	}
	ctx := context.Background()

	// Look up the current folder and user before moving, so we can notify both sides.
	var oldFolder, msgUserID string
	err := db.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT Folder, UserID FROM %s WHERE ID = ?`, tenant("mailbox")),
		id,
	).Scan(&oldFolder, &msgUserID)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx,
		fmt.Sprintf(`UPDATE %s SET Folder = ? WHERE ID = ?`, tenant("mailbox")),
		folder, id,
	)
	if err != nil {
		return err
	}
	dbLastAction.touch()

	// Notify IDLEing clients on both source and destination folders.
	if oldFolder != folder {
		if msgUserID != "" {
			// Per-user notification.
			if oldTotal, _, _, err := MailboxCounts(oldFolder, msgUserID); err == nil {
				IdleNotify.NotifyFolderChangeUser(msgUserID, oldFolder, int(oldTotal))
			}
			if newTotal, _, _, err := MailboxCounts(folder, msgUserID); err == nil {
				IdleNotify.NotifyFolderChangeUser(msgUserID, folder, int(newTotal))
			}
		} else {
			// Shared message: notify all.
			if oldTotal, _, _, err := MailboxCounts(oldFolder); err == nil {
				IdleNotify.NotifyFolderChange(oldFolder, int(oldTotal))
			}
			if newTotal, _, _, err := MailboxCounts(folder); err == nil {
				IdleNotify.NotifyFolderChange(folder, int(newTotal))
			}
		}
	}

	return nil
}

// DeleteAllMessages will delete all messages from a mailbox
func DeleteAllMessages() error {
	var (
		start = time.Now()
		total int
	)

	_ = sqlf.From(tenant("mailbox")).
		Select("COUNT(*)").To(&total).
		QueryRowAndClose(context.TODO(), db)

	// begin a transaction to ensure both the message
	// summaries and data are deleted successfully
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}

	// roll back if it fails
	defer func() { _ = tx.Rollback() }()

	tables := []string{"mailbox", "mailbox_data", "tags", "message_tags"}

	for _, t := range tables {
		sql := fmt.Sprintf(`DELETE FROM %s`, tenant(t)) // #nosec
		_, err := tx.Exec(sql)
		if err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	elapsed := time.Since(start)
	logger.Log().Debugf("[db] deleted %d messages in %s", total, elapsed)

	vacuumDb()

	dbLastAction.touch()
	if err := SettingPut("DeletedSize", "0"); err != nil {
		logger.Log().Warnf("[db] %s", err.Error())
	}

	logMessagesDeleted(total)

	BroadcastMailboxStats()

	invalidateAllEnvelopeCache()

	websockets.Broadcast("truncate", nil)

	// Notify IDLEing IMAP clients that INBOX is now empty.
	IdleNotify.NotifyFolderChange("", 0)

	return err
}

// GetMetadata retrieves the metadata for a message by its ID
func GetMetadata(id string) (Metadata, error) {
	var metadataJSON string
	row := db.QueryRow(fmt.Sprintf("SELECT Metadata FROM %s WHERE ID = ?", tenant("mailbox")), id)
	if err := row.Scan(&metadataJSON); err != nil {
		return Metadata{}, err
	}
	var meta Metadata
	if err := json.Unmarshal([]byte(metadataJSON), &meta); err != nil {
		return Metadata{}, err
	}
	return meta, nil
}

// SearchIDsBySearchText returns message IDs where SearchText contains the term.
func SearchIDsBySearchText(term string, userID ...string) ([]string, error) {
	if term == "" {
		return nil, nil
	}
	searchTerm := "%" + cleanString(escLikeChars(strings.ToLower(term))) + "%"
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where("SearchText LIKE ?", searchTerm)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}

// SearchIDsBySubject returns message IDs where Subject contains the term.
func SearchIDsBySubject(term string, userID ...string) ([]string, error) {
	if term == "" {
		return nil, nil
	}
	searchTerm := "%" + escLikeChars(term) + "%"
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where("Subject LIKE ?", searchTerm)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}

// SearchIDsByMetadata returns message IDs where a Metadata JSON field matches.
// field should be "From", "To", "Cc", "Bcc", or "ReplyTo" (safe, not user-controlled).
func SearchIDsByMetadata(field, term string, userID ...string) ([]string, error) {
	if term == "" {
		return nil, nil
	}
	searchTerm := "%" + escLikeChars(strings.ToLower(term)) + "%"
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where(fmt.Sprintf("json_extract(Metadata, '$.%s') LIKE ?", field), searchTerm)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}

// SearchIDsBySize returns message IDs matching the size comparison.
func SearchIDsBySize(operator string, size uint64, userID ...string) ([]string, error) {
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where(fmt.Sprintf("Size %s ?", operator), size)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}

// SearchIDsByCreated returns message IDs matching the Created timestamp comparison.
func SearchIDsByCreated(operator string, ts int64, userID ...string) ([]string, error) {
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where(fmt.Sprintf("Created %s ?", operator), ts)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}

// SearchIDsByCreatedBetween returns message IDs where Created is between start and end (inclusive).
func SearchIDsByCreatedBetween(start, end int64, userID ...string) ([]string, error) {
	var ids []string

	q := sqlf.From(tenant("mailbox")).
		Select("ID").
		Where("Created >= ? AND Created <= ?", start, end)

	if len(userID) > 0 && userID[0] != "" {
		q = q.Where("UserID = ?", userID[0])
	}

	if err := q.QueryAndClose(context.TODO(), db, func(row *sql.Rows) {
		var id string
		if err := row.Scan(&id); err != nil {
			logger.Log().Errorf("[db] %s", err.Error())
			return
		}
		ids = append(ids, id)
	}); err != nil {
		return nil, err
	}

	dbLastAction.touch()

	return ids, nil
}
