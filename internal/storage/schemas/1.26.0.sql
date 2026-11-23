-- Users table for per-user inbox routing
-- Stores IMAP usernames and password hashes for authentication and SMTP routing
CREATE TABLE IF NOT EXISTS {{ tenant "imap_users" }} (
    Username TEXT NOT NULL PRIMARY KEY,
    PasswordHash TEXT NOT NULL DEFAULT ''
);
