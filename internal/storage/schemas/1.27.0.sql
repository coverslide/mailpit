-- Custom IMAP mailboxes/folders created by users
-- Stores user-created mailboxes beyond the standard ones (INBOX, Sent, Trash, Drafts)
CREATE TABLE IF NOT EXISTS {{ tenant "custom_mailboxes" }} (
    Name TEXT NOT NULL,
    UserID TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (Name, UserID)
);
