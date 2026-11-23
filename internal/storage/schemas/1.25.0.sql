-- Add UserID column for per-user inbox isolation
-- Empty string = legacy shared message (visible to all users)
-- Non-empty = IMAP username that owns this mailbox row
ALTER TABLE {{ tenant "mailbox" }} ADD COLUMN UserID TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS {{ tenant "idx_userid" }} ON {{ tenant "mailbox" }} (UserID);
