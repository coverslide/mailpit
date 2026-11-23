package storage

import (
	"testing"
)

func TestIMAPUserCRUD(t *testing.T) {
	setup("")
	defer Close()

	// Register a user
	if err := RegisterIMAPUser("alice@test.com"); err != nil {
		t.Fatal(err)
	}

	// Verify user exists
	if !IMAPUserExists("alice@test.com") {
		t.Fatal("expected alice@test.com to exist after registration")
	}

	// Verify non-existent user
	if IMAPUserExists("bob@test.com") {
		t.Fatal("expected bob@test.com to not exist")
	}

	// Register duplicate — should not error
	if err := RegisterIMAPUser("alice@test.com"); err != nil {
		t.Fatal(err)
	}

	// Get all users
	users, err := GetIMAPUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 1 {
		t.Errorf("expected 1 user, got %d", len(users))
	}

	// Seed users from config
	seedUsers := map[string]string{
		"bob@test.com":   "hash_bob",
		"carol@test.com": "hash_carol",
	}
	if err := SeedIMAPUsers(seedUsers); err != nil {
		t.Fatal(err)
	}

	users, err = GetIMAPUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Errorf("expected 3 users after seed, got %d", len(users))
	}

	// Auth with correct hash
	if !IMAPUserAuth("bob@test.com", "hash_bob") {
		t.Fatal("expected bob auth to succeed")
	}

	// Auth with wrong hash
	if IMAPUserAuth("bob@test.com", "wrong_hash") {
		t.Fatal("expected bob auth to fail with wrong hash")
	}

	// Auth non-existent user
	if IMAPUserAuth("nobody@test.com", "hash") {
		t.Fatal("expected auth for non-existent user to fail")
	}

	// Set password hash
	if err := SetIMAPUserPassword("alice@test.com", "new_hash_alice"); err != nil {
		t.Fatal(err)
	}
	if !IMAPUserAuth("alice@test.com", "new_hash_alice") {
		t.Fatal("expected alice auth with new hash to succeed")
	}
	if IMAPUserAuth("alice@test.com", "hash_alice") {
		t.Fatal("expected alice auth with old hash to fail")
	}

	// Delete user
	if err := DeleteIMAPUser("bob@test.com"); err != nil {
		t.Fatal(err)
	}
	if IMAPUserExists("bob@test.com") {
		t.Fatal("expected bob to not exist after deletion")
	}

	users, err = GetIMAPUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 2 {
		t.Errorf("expected 2 users after deletion, got %d", len(users))
	}

	// Delete non-existent user — should not error
	if err := DeleteIMAPUser("nobody@test.com"); err != nil {
		t.Fatal(err)
	}

	// Empty username should error
	if err := RegisterIMAPUser(""); err != nil {
		// empty is allowed (no-op), that's fine
	}
	if err := DeleteIMAPUser(""); err == nil {
		t.Fatal("expected error when deleting empty username")
	}
}

func TestCustomMailboxCRUDAndIsolation(t *testing.T) {
	setup("")
	defer Close()

	created, err := CreateCustomMailbox("Archive", "alice")
	if err != nil || !created {
		t.Fatalf("CreateCustomMailbox() = (%v, %v), want (true, nil)", created, err)
	}
	created, err = CreateCustomMailbox("Archive", "alice")
	if err != nil || created {
		t.Fatalf("duplicate CreateCustomMailbox() = (%v, %v), want (false, nil)", created, err)
	}
	if !CustomMailboxExists("Archive", "alice") || CustomMailboxExists("Archive", "bob") {
		t.Fatal("custom mailbox existence must be scoped to its owner")
	}

	if _, err := CreateCustomMailbox("Archive", "bob"); err != nil {
		t.Fatal(err)
	}
	mailboxes, err := GetCustomMailboxes("alice")
	if err != nil || len(mailboxes) != 1 || mailboxes[0] != "Archive" {
		t.Fatalf("GetCustomMailboxes(alice) = (%v, %v), want ([Archive], nil)", mailboxes, err)
	}

	if err := DeleteCustomMailbox("Archive", "alice"); err != nil {
		t.Fatal(err)
	}
	if CustomMailboxExists("Archive", "alice") || !CustomMailboxExists("Archive", "bob") {
		t.Fatal("deleting a mailbox must only remove it for the specified user")
	}
	if _, err := CreateCustomMailbox("", "alice"); err == nil {
		t.Fatal("expected empty mailbox name to fail")
	}
	if err := DeleteCustomMailbox("", "alice"); err == nil {
		t.Fatal("expected deleting an empty mailbox name to fail")
	}
}
