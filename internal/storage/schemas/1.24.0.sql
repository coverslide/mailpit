-- Add Folder column to support IMAP virtual folders (Sent, Trash, Drafts)
-- Empty string = INBOX (default), 'Sent', 'Trash', 'Drafts' for standard folders
ALTER TABLE {{ tenant "mailbox" }} ADD COLUMN Folder TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS {{ tenant "idx_folder" }} ON {{ tenant "mailbox" }} (Folder);
