package storage

import (
	"os"
	"testing"
	"time"

	"github.com/axllent/mailpit/config"
)

func TestTextEmailInserts(t *testing.T) {
	setup("")
	defer Close()

	t.Log("Testing text email storage")

	start := time.Now()

	for range testRuns {
		if _, err := Store(&testTextEmail, nil, ""); err != nil {
			t.Log("error ", err)
			t.Fail()
		}
	}

	assertEqual(t, CountTotal(), uint64(testRuns), "Incorrect number of text emails stored")

	t.Logf("Inserted %d text emails in %s", testRuns, time.Since(start))

	delStart := time.Now()
	if err := DeleteAllMessages(); err != nil {
		t.Log("error ", err)
		t.Fail()
	}

	assertEqual(t, CountTotal(), uint64(0), "incorrect number of text emails deleted")

	t.Logf("deleted %d text emails in %s", testRuns, time.Since(delStart))

	assertEqualStats(t, 0, 0)
}

func TestMimeEmailInserts(t *testing.T) {
	for _, tenantID := range []string{"", "MyServer 3", "host.example.com"} {
		tenantID = config.DBTenantID(tenantID)

		setup(tenantID)

		if tenantID == "" {
			t.Log("Testing mime email storage")
		} else {
			t.Logf("Testing mime email storage (tenant %s)", tenantID)
		}

		start := time.Now()

		for range testRuns {
			if _, err := Store(&testMimeEmail, nil, ""); err != nil {
				t.Log("error ", err)
				t.Fail()
			}
		}

		assertEqual(t, CountTotal(), uint64(testRuns), "Incorrect number of mime emails stored")

		t.Logf("Inserted %d text emails in %s", testRuns, time.Since(start))

		delStart := time.Now()
		if err := DeleteAllMessages(); err != nil {
			t.Log("error ", err)
			t.Fail()
		}

		assertEqual(t, CountTotal(), uint64(0), "incorrect number of mime emails deleted")

		t.Logf("Deleted %d mime emails in %s", testRuns, time.Since(delStart))

		Close()
	}
}

func TestRetrieveMimeEmail(t *testing.T) {
	compressionLevels := []int{0, 1, 2, 3}

	for _, compressionLevel := range compressionLevels {
		t.Logf("Testing compression level: %d", compressionLevel)
		for _, tenantID := range []string{"", "MyServer 3", "host.example.com"} {
			tenantID = config.DBTenantID(tenantID)
			config.Compression = compressionLevel
			setup(tenantID)

			if tenantID == "" {
				t.Log("Testing mime email retrieval")
			} else {
				t.Logf("Testing mime email retrieval (tenant %s)", tenantID)
			}

			id, err := Store(&testMimeEmail, nil, "")
			if err != nil {
				t.Log("error ", err)
				t.Fail()
			}

			msg, err := GetMessage(id)
			if err != nil {
				t.Log("error ", err)
				t.Fail()
			}

			assertEqual(t, msg.From.Name, "Sender Smith", "\"From\" name does not match")
			assertEqual(t, msg.From.Address, "sender2@example.com", "\"From\" address does not match")
			assertEqual(t, msg.Subject, "inline + attachment", "subject does not match")
			assertEqual(t, len(msg.To), 1, "incorrect number of recipients")
			assertEqual(t, msg.To[0].Name, "Recipient Ross", "\"To\" name does not match")
			assertEqual(t, msg.To[0].Address, "recipient2@example.com", "\"To\" address does not match")
			assertEqual(t, len(msg.Attachments), 1, "incorrect number of attachments")
			assertEqual(t, msg.Attachments[0].FileName, "Sample PDF.pdf", "attachment filename does not match")
			assertEqual(t, len(msg.Inline), 1, "incorrect number of inline attachments")
			assertEqual(t, msg.Inline[0].FileName, "inline-image.jpg", "inline attachment filename does not match")

			attachmentData, err := GetAttachmentPart(id, msg.Attachments[0].PartID)
			if err != nil {
				t.Log("error ", err)
				t.Fail()
			}
			assertEqual(t, uint64(len(attachmentData.Content)), msg.Attachments[0].Size, "attachment size does not match")

			inlineData, err := GetAttachmentPart(id, msg.Inline[0].PartID)
			if err != nil {
				t.Log("error ", err)
				t.Fail()
			}
			assertEqual(t, uint64(len(inlineData.Content)), msg.Inline[0].Size, "inline attachment size does not match")

			Close()
		}
	}

	// reset compression
	config.Compression = 1
}

func TestMessageSummary(t *testing.T) {
	for _, tenantID := range []string{"", "MyServer 3", "host.example.com"} {
		tenantID = config.DBTenantID(tenantID)

		setup(tenantID)

		if tenantID == "" {
			t.Log("Testing message summary")
		} else {
			t.Logf("Testing message summary (tenant %s)", tenantID)
		}

		if _, err := Store(&testMimeEmail, nil, ""); err != nil {
			t.Log("error ", err)
			t.Fail()
		}

		summaries, err := List(0, 0, 1, "")
		if err != nil {
			t.Log("error ", err)
			t.Fail()
		}

		assertEqual(t, len(summaries), 1, "Expected 1 result")

		msg := summaries[0]

		assertEqual(t, msg.From.Name, "Sender Smith", "\"From\" name does not match")
		assertEqual(t, msg.From.Address, "sender2@example.com", "\"From\" address does not match")
		assertEqual(t, msg.Subject, "inline + attachment", "subject does not match")
		assertEqual(t, len(msg.To), 1, "incorrect number of recipients")
		assertEqual(t, msg.To[0].Name, "Recipient Ross", "\"To\" name does not match")
		assertEqual(t, msg.To[0].Address, "recipient2@example.com", "\"To\" address does not match")
		assertEqual(t, msg.Snippet, "Message with inline image and attachment:", "\"Snippet\" does does not match")
		assertEqual(t, msg.Attachments, 1, "Expected 1 attachment")
		assertEqual(t, msg.MessageID, "33af2ac1-c33d-9738-35e3-a6daf90bbd89@gmail.com", "\"MessageID\" does not match")

		Close()
	}
}

func BenchmarkImportText(b *testing.B) {
	setup("")
	defer Close()

	for i := 0; i < b.N; i++ {
		if _, err := Store(&testTextEmail, nil, ""); err != nil {
			b.Log("error ", err)
			b.Fail()
		}
	}
}

func BenchmarkImportMime(b *testing.B) {
	setup("")
	defer Close()

	for i := 0; i < b.N; i++ {
		if _, err := Store(&testMimeEmail, nil, ""); err != nil {
			b.Log("error ", err)
			b.Fail()
		}
	}

}

func TestInlineImageContentIdHandling(t *testing.T) {
	setup("")
	defer Close()
	t.Log("Testing inline content handling")
	// Test case: Proper inline image with Content-Disposition: inline
	inlineAttachment, err := os.ReadFile("testdata/inline-attachment.eml")
	if err != nil {
		t.Fatalf("Failed to read test email: %v", err)
	}
	storedMessage, err := Store(&inlineAttachment, nil, "")
	if err != nil {
		t.Fatal("Failed to store test case 1:", err)
	}

	msg, err := GetMessage(storedMessage)
	if err != nil {
		t.Fatal("Failed to retrieve test case 1:", err)
	}
	// Assert
	if len(msg.Inline) != 1 {
		t.Errorf("Test case 1: Expected 1 inline attachment, got %d", len(msg.Inline))
	}
	if len(msg.Attachments) != 0 {
		t.Errorf("Test case 1: Expected 0 regular attachments, got %d", len(msg.Attachments))
	}
	if msg.Inline[0].ContentID != "test1@example.com" {
		t.Errorf("Test case 1: Expected ContentID 'test1@example.com', got '%s'", msg.Inline[0].ContentID)
	}
}

func TestRegularAttachmentHandling(t *testing.T) {
	setup("")
	defer Close()
	t.Log("Testing regular attachment handling")
	// Test case: Regular attachment without Content-ID
	regularAttachment, err := os.ReadFile("testdata/regular-attachment.eml")
	if err != nil {
		t.Fatalf("Failed to read test email: %v", err)
	}
	storedMessage, err := Store(&regularAttachment, nil, "")
	if err != nil {
		t.Fatal("Failed to store test case 3:", err)
	}
	msg, err := GetMessage(storedMessage)
	if err != nil {
		t.Fatal("Failed to retrieve test case 3:", err)
	}
	// Assert
	if len(msg.Inline) != 0 {
		t.Errorf("Test case 3: Expected 0 inline attachments, got %d", len(msg.Inline))
	}
	if len(msg.Attachments) != 1 {
		t.Errorf("Test case 3: Expected 1 regular attachment, got %d", len(msg.Attachments))
	}
	if msg.Attachments[0].ContentID != "" {
		t.Errorf("Test case 3: Expected empty ContentID, got '%s'", msg.Attachments[0].ContentID)
	}

	// Checksum tests
	assertEqual(t, msg.Attachments[0].Checksums.MD5, "b04930eb1ba0c62066adfa87e5d262c4", "Attachment MD5 checksum does not match")
	assertEqual(t, msg.Attachments[0].Checksums.SHA1, "15605d6a2fca44e966209d1701f16ecf816df880", "Attachment SHA1 checksum does not match")
	assertEqual(t, msg.Attachments[0].Checksums.SHA256, "92c4ccff376003381bd9054d3da7b32a3c5661905b55e3b0728c17aba6d223ec", "Attachment SHA256 checksum does not match")
}

func TestMixedAttachmentHandling(t *testing.T) {
	setup("")
	defer Close()
	t.Log("Testing mixed attachment handling")
	// Mixed scenario with both inline and regular attachment
	mixedAttachment, err := os.ReadFile("testdata/mixed-attachment.eml")
	if err != nil {
		t.Fatalf("Failed to read test email: %v", err)
	}
	storedMessage, err := Store(&mixedAttachment, nil, "")
	if err != nil {
		t.Fatal("Failed to store test case 4:", err)
	}
	msg, err := GetMessage(storedMessage)
	if err != nil {
		t.Fatal("Failed to retrieve test case 4:", err)
	}
	// Assert: Should have 1 inline (with ContentID) and 1 attachment (without ContentID)
	if len(msg.Inline) != 1 {
		t.Errorf("Test case 4: Expected 1 inline attachment, got %d", len(msg.Inline))
	}
	if len(msg.Attachments) != 1 {
		t.Errorf("Test case 4: Expected 1 regular attachment, got %d", len(msg.Attachments))
	}
	if msg.Inline[0].ContentID != "inline@example.com" {
		t.Errorf("Test case 4: Expected inline ContentID 'inline@example.com', got '%s'", msg.Inline[0].ContentID)
	}
	if msg.Attachments[0].ContentID != "" {
		t.Errorf("Test case 4: Expected attachment ContentID to be empty, got '%s'", msg.Attachments[0].ContentID)
	}
}

func TestMailboxCounts(t *testing.T) {
	setup("")
	defer Close()

	body := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: one\r\n\r\nbody")
	if _, err := Store(&body, nil, ""); err != nil {
		t.Fatal(err)
	}
	body2 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: two\r\n\r\nbody")
	if _, err := Store(&body2, nil, ""); err != nil {
		t.Fatal(err)
	}

	total, unread, nextUID, err := MailboxCounts("")
	if err != nil {
		t.Fatalf("MailboxCounts error: %v", err)
	}
	if total != 2 {
		t.Errorf("expected total 2, got %d", total)
	}
	if unread != 2 {
		t.Errorf("expected unread 2, got %d", unread)
	}
	if nextUID != 3 {
		t.Errorf("expected nextUID 3, got %d", nextUID)
	}
}

func TestMailboxCountsWithUserID(t *testing.T) {
	setup("")
	defer Close()

	// Store messages for different users
	body1 := []byte("From: a@b.com\r\nTo: alice@test.com\r\nSubject: alice msg\r\n\r\nbody for alice")
	if _, err := Store(&body1, nil, "", "alice"); err != nil {
		t.Fatal(err)
	}
	body2 := []byte("From: a@b.com\r\nTo: bob@test.com\r\nSubject: bob msg\r\n\r\nbody for bob")
	if _, err := Store(&body2, nil, "", "bob"); err != nil {
		t.Fatal(err)
	}
	body3 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: shared msg\r\n\r\nshared body")
	if _, err := Store(&body3, nil, ""); err != nil {
		t.Fatal(err)
	}

	// Alice should see only her own message (shared not visible to IMAP users)
	total, unread, _, err := MailboxCounts("", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("alice: expected total 1 (own only), got %d", total)
	}
	if unread != 1 {
		t.Errorf("alice: expected unread 1, got %d", unread)
	}

	// Bob should see only his own message
	total, unread, _, err = MailboxCounts("", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("bob: expected total 1 (own only), got %d", total)
	}
	if unread != 1 {
		t.Errorf("bob: expected unread 1, got %d", unread)
	}

	// No user filter should see all 3
	total, _, _, err = MailboxCounts("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("no filter: expected total 3, got %d", total)
	}

	// Test with folder filter
	body4 := []byte("From: a@b.com\r\nTo: alice@test.com\r\nSubject: alice sent\r\n\r\nsent body")
	if _, err := Store(&body4, nil, "Sent", "alice"); err != nil {
		t.Fatal(err)
	}

	// Alice should see 1 in Sent
	total, _, _, err = MailboxCounts("Sent", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("alice Sent: expected total 1, got %d", total)
	}

	// Bob should see 0 in Sent
	total, _, _, err = MailboxCounts("Sent", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("bob Sent: expected total 0, got %d", total)
	}
}

func TestPerUserIsolation(t *testing.T) {
	setup("")
	defer Close()

	// Store messages for alice
	body1 := []byte("From: a@b.com\r\nTo: alice@test.com\r\nSubject: alice private\r\n\r\nalice body")
	if _, err := Store(&body1, nil, "", "alice"); err != nil {
		t.Fatal(err)
	}

	// Store shared message
	body2 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: shared\r\n\r\nshared body")
	if _, err := Store(&body2, nil, ""); err != nil {
		t.Fatal(err)
	}

	// Alice should see her message via List (shared not visible to IMAP users)
	msgs, err := List(0, 0, 0, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("alice List: expected 1 message (own only), got %d", len(msgs))
	}

	// Bob should see nothing (no messages tagged to bob, shared invisible)
	msgs, err = List(0, 0, 0, "", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("bob List: expected 0 messages, got %d", len(msgs))
	}

	// No user filter sees all
	msgs, err = List(0, 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Errorf("no filter List: expected 2 messages, got %d", len(msgs))
	}
}

func TestMoveMessage(t *testing.T) {
	setup("")
	defer Close()

	body := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: moveme\r\n\r\nmove body")
	id, err := Store(&body, nil, "", "alice")
	if err != nil {
		t.Fatal(err)
	}

	// Verify it's in INBOX (folder="")
	msgs, err := List(0, 0, 0, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message in INBOX, got %d", len(msgs))
	}

	// Move to Trash
	if err := MoveMessage(id, "Trash"); err != nil {
		t.Fatal(err)
	}

	// Should be gone from INBOX
	msgs, err = List(0, 0, 0, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Errorf("expected 0 messages in INBOX after move, got %d", len(msgs))
	}

	// Should be in Trash
	msgs, err = List(0, 0, 0, "Trash", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 message in Trash after move, got %d", len(msgs))
	}

	// Move back to INBOX
	if err := MoveMessage(id, ""); err != nil {
		t.Fatal(err)
	}

	msgs, err = List(0, 0, 0, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Errorf("expected 1 message back in INBOX, got %d", len(msgs))
	}
}

func TestDeleteMessages(t *testing.T) {
	setup("")
	defer Close()

	body1 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: delete me\r\n\r\nbody1")
	id1, err := Store(&body1, nil, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	body2 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: keep me\r\n\r\nbody2")
	id2, err := Store(&body2, nil, "", "alice")
	if err != nil {
		t.Fatal(err)
	}
	body3 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: shared\r\n\r\nbody3")
	_, err = Store(&body3, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	// Verify 3 total (no user filter sees all)
	total, _, _, err := MailboxCounts("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Fatalf("expected 3 total, got %d", total)
	}

	// Delete alice's first message
	if err := DeleteMessages([]string{id1}); err != nil {
		t.Fatal(err)
	}

	// Verify alice has 1, shared still 1, total 2
	total, _, _, err = MailboxCounts("", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("alice: expected 1 after delete, got %d", total)
	}

	// Delete bob's message
	if err := DeleteMessages([]string{id2}); err != nil {
		t.Fatal(err)
	}

	// Only shared left
	total, _, _, err = MailboxCounts("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("expected 1 shared message remaining, got %d", total)
	}
}

func TestDeleteAllMessages(t *testing.T) {
	setup("")
	defer Close()

	body1 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: one\r\n\r\nbody1")
	if _, err := Store(&body1, nil, "", "alice"); err != nil {
		t.Fatal(err)
	}
	body2 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: two\r\n\r\nbody2")
	if _, err := Store(&body2, nil, "", "bob"); err != nil {
		t.Fatal(err)
	}
	body3 := []byte("From: a@b.com\r\nTo: c@d.com\r\nSubject: three\r\n\r\nbody3")
	if _, err := Store(&body3, nil, ""); err != nil {
		t.Fatal(err)
	}

	if err := DeleteAllMessages(); err != nil {
		t.Fatal(err)
	}

	total, _, _, err := MailboxCounts("")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("expected 0 after delete all, got %d", total)
	}

	// Verify per-user counts are also 0
	total, _, _, err = MailboxCounts("", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Errorf("alice: expected 0 after delete all, got %d", total)
	}
}
