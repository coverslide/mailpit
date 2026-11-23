package storage

import (
	"context"
	"fmt"
)

// RegisterIMAPUser adds or confirms a username in the imap_users table.
// Called on successful IMAP authentication to ensure the user exists for SMTP routing.
func RegisterIMAPUser(username string) error {
	if username == "" {
		return nil
	}
	_, err := db.ExecContext(context.Background(),
		fmt.Sprintf(`INSERT OR IGNORE INTO %s (Username, PasswordHash) VALUES (?, '')`, tenant("imap_users")),
		username,
	)
	return err
}

// IMAPUserExists checks whether a username is registered in the imap_users table.
func IMAPUserExists(username string) bool {
	if username == "" {
		return false
	}
	var count int
	_ = db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE Username = ?`, tenant("imap_users")),
		username,
	).Scan(&count)
	return count > 0
}

// IMAPUserAuth checks whether a username and password hash match in the imap_users table.
func IMAPUserAuth(username, passwordHash string) bool {
	if username == "" || passwordHash == "" {
		return false
	}
	var count int
	_ = db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE Username = ? AND PasswordHash = ?`, tenant("imap_users")),
		username, passwordHash,
	).Scan(&count)
	return count > 0
}

// SetIMAPUserPassword sets the password hash for an IMAP user.
func SetIMAPUserPassword(username, passwordHash string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	_, err := db.ExecContext(context.Background(),
		fmt.Sprintf(`INSERT INTO %s (Username, PasswordHash) VALUES (?, ?) ON CONFLICT(Username) DO UPDATE SET PasswordHash = ?`, tenant("imap_users")),
		username, passwordHash, passwordHash,
	)
	return err
}

// SeedIMAPUsers inserts users from the config into the imap_users table.
// Existing users are not overwritten.
func SeedIMAPUsers(users map[string]string) error {
	for username, passwordHash := range users {
		if username == "" {
			continue
		}
		_, err := db.ExecContext(context.Background(),
			fmt.Sprintf(`INSERT OR IGNORE INTO %s (Username, PasswordHash) VALUES (?, ?)`, tenant("imap_users")),
			username, passwordHash,
		)
		if err != nil {
			return fmt.Errorf("failed to seed user %s: %w", username, err)
		}
	}
	return nil
}

// GetIMAPUsers returns all registered IMAP usernames.
func GetIMAPUsers() ([]string, error) {
	var users []string
	rows, err := db.QueryContext(context.Background(),
		fmt.Sprintf(`SELECT Username FROM %s`, tenant("imap_users")),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, nil
}

// DeleteIMAPUser removes an IMAP user.
func DeleteIMAPUser(username string) error {
	if username == "" {
		return fmt.Errorf("username cannot be empty")
	}
	_, err := db.ExecContext(context.Background(),
		fmt.Sprintf(`DELETE FROM %s WHERE Username = ?`, tenant("imap_users")),
		username,
	)
	return err
}
