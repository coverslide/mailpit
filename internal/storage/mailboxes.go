package storage

import (
	"context"
	"fmt"
)

// CreateCustomMailbox creates a new custom mailbox/folder for a user.
// Returns true if created, false if it already exists.
func CreateCustomMailbox(name string, userID string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("mailbox name cannot be empty")
	}
	result, err := db.ExecContext(context.Background(),
		fmt.Sprintf(`INSERT OR IGNORE INTO %s (Name, UserID) VALUES (?, ?)`, tenant("custom_mailboxes")),
		name, userID,
	)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return rows > 0, nil
}

// CustomMailboxExists checks if a custom mailbox exists for a user.
func CustomMailboxExists(name string, userID string) bool {
	if name == "" {
		return false
	}
	var count int
	_ = db.QueryRowContext(context.Background(),
		fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE Name = ? AND UserID = ?`, tenant("custom_mailboxes")),
		name, userID,
	).Scan(&count)
	return count > 0
}

// GetCustomMailboxes returns all custom mailboxes for a user.
func GetCustomMailboxes(userID string) ([]string, error) {
	var mailboxes []string
	rows, err := db.QueryContext(context.Background(),
		fmt.Sprintf(`SELECT Name FROM %s WHERE UserID = ?`, tenant("custom_mailboxes")),
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			continue
		}
		mailboxes = append(mailboxes, name)
	}
	return mailboxes, nil
}

// DeleteCustomMailbox removes a custom mailbox for a user.
// Note: This does not delete messages in the mailbox.
func DeleteCustomMailbox(name string, userID string) error {
	if name == "" {
		return fmt.Errorf("mailbox name cannot be empty")
	}
	_, err := db.ExecContext(context.Background(),
		fmt.Sprintf(`DELETE FROM %s WHERE Name = ? AND UserID = ?`, tenant("custom_mailboxes")),
		name, userID,
	)
	return err
}
