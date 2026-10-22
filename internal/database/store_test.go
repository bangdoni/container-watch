package database_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerwatch/containerwatch/internal/database"
)

// newStore opens a throwaway SQLite store.
func newStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// newUser inserts a user and returns it.
func newUser(t *testing.T, store *database.Store, username, email, role string) *database.User {
	t.Helper()
	u := &database.User{
		Username:     username,
		Email:        email,
		PasswordHash: "hash",
		Role:         role,
	}
	if err := store.CreateUser(context.Background(), u); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	return u
}

// TestCreateUserAssignsID checks that the inserted row is readable back.
func TestCreateUserAssignsID(t *testing.T) {
	store := newStore(t)
	u := newUser(t, store, "alice", "alice@example.com", "admin")

	if u.ID == 0 {
		t.Error("CreateUser did not assign an ID")
	}
	if u.CreatedAt.IsZero() {
		t.Error("CreateUser did not set CreatedAt")
	}
	got, err := store.UserByID(context.Background(), u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.Username != "alice" || got.Email != "alice@example.com" {
		t.Errorf("UserByID returned %+v", got)
	}
}

// TestCreateUserRejectsDuplicateUsername checks the unique constraint.
func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	store := newStore(t)
	newUser(t, store, "alice", "alice@example.com", "user")

	dup := &database.User{
		Username:     "alice",
		Email:        "other@example.com",
		PasswordHash: "hash",
		Role:         "user",
	}
	err := store.CreateUser(context.Background(), dup)
	if !errors.Is(err, database.ErrConflict) {
		t.Errorf("CreateUser error = %v, want ErrConflict", err)
	}
}

// TestUserByLoginIsCaseInsensitive checks the login lookup, which accepts
// either a username or an email in any case.
func TestUserByLoginIsCaseInsensitive(t *testing.T) {
	store := newStore(t)
	newUser(t, store, "Alice", "Alice@Example.com", "user")

	for _, login := range []string{"alice", "ALICE", "alice@example.com", "ALICE@EXAMPLE.COM"} {
		got, err := store.UserByLogin(context.Background(), login)
		if err != nil {
			t.Errorf("UserByLogin(%q): %v", login, err)
			continue
		}
		if got.Username != "Alice" {
			t.Errorf("UserByLogin(%q) returned %q", login, got.Username)
		}
	}
}

// TestUserByLoginNotFound checks the missing-user path.
func TestUserByLoginNotFound(t *testing.T) {
	store := newStore(t)
	if _, err := store.UserByLogin(context.Background(), "nobody"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("UserByLogin error = %v, want ErrNotFound", err)
	}
}

// TestCountUsersAndAdmins checks the first-run detection helpers.
func TestCountUsersAndAdmins(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	n, err := store.CountUsers(ctx)
	if err != nil {
		t.Fatalf("CountUsers: %v", err)
	}
	if n != 0 {
		t.Errorf("CountUsers on an empty store = %d, want 0", n)
	}

	newUser(t, store, "alice", "alice@example.com", "user")
	newUser(t, store, "bob", "bob@example.com", "admin")

	n, _ = store.CountUsers(ctx)
	if n != 2 {
		t.Errorf("CountUsers = %d, want 2", n)
	}
	a, err := store.CountAdmins(ctx)
	if err != nil {
		t.Fatalf("CountAdmins: %v", err)
	}
	if a != 1 {
		t.Errorf("CountAdmins = %d, want 1", a)
	}
}

// TestUpdateUserRole checks the role change and its effect on IsAdmin.
func TestUpdateUserRole(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	if u.IsAdmin() {
		t.Error("a user-role account reports IsAdmin")
	}
	if err := store.UpdateUserRole(ctx, u.ID, "admin"); err != nil {
		t.Fatalf("UpdateUserRole: %v", err)
	}
	got, err := store.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if !got.IsAdmin() {
		t.Error("the role change did not take effect")
	}
}

// TestTouchLogin checks that the last-login timestamp is recorded.
func TestTouchLogin(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	if err := store.TouchLogin(ctx, u.ID); err != nil {
		t.Fatalf("TouchLogin: %v", err)
	}
	got, err := store.UserByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("UserByID: %v", err)
	}
	if got.LastLoginAt == nil {
		t.Fatal("LastLoginAt is nil after TouchLogin")
	}
	if time.Since(*got.LastLoginAt) > time.Minute {
		t.Errorf("LastLoginAt = %v, want a recent timestamp", *got.LastLoginAt)
	}
}

// TestDeleteUserCascadesSessions checks that deleting an account removes
// its sessions rather than leaving them usable.
func TestDeleteUserCascadesSessions(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	sess := &database.Session{
		UserID:    u.ID,
		TokenHash: "hash-alice",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := store.DeleteUser(ctx, u.ID); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if _, _, err := store.SessionByHash(ctx, "hash-alice"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("the session survived the user deletion (err=%v)", err)
	}
}

// TestSessionByHashExpires checks that an expired session is not returned.
func TestSessionByHashExpires(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	sess := &database.Session{
		UserID:    u.ID,
		TokenHash: "hash-expired",
		ExpiresAt: time.Now().Add(-time.Hour),
	}
	if err := store.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if _, _, err := store.SessionByHash(ctx, "hash-expired"); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("an expired session was returned (err=%v)", err)
	}
}

// TestDeleteUserSessionsKeepsOne checks the "log out everywhere else"
// helper: it must delete the other sessions but keep the named one.
func TestDeleteUserSessionsKeepsOne(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	for _, h := range []string{"keep-me", "drop-1", "drop-2"} {
		if err := store.CreateSession(ctx, &database.Session{
			UserID:    u.ID,
			TokenHash: h,
			ExpiresAt: time.Now().Add(time.Hour),
		}); err != nil {
			t.Fatalf("CreateSession(%s): %v", h, err)
		}
	}

	if err := store.DeleteUserSessions(ctx, u.ID, "keep-me"); err != nil {
		t.Fatalf("DeleteUserSessions: %v", err)
	}
	if _, _, err := store.SessionByHash(ctx, "keep-me"); err != nil {
		t.Errorf("the kept session was deleted: %v", err)
	}
	for _, h := range []string{"drop-1", "drop-2"} {
		if _, _, err := store.SessionByHash(ctx, h); !errors.Is(err, database.ErrNotFound) {
			t.Errorf("session %q survived (err=%v)", h, err)
		}
	}
}

// TestPurgeExpiredSessions checks the maintenance query.
func TestPurgeExpiredSessions(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	u := newUser(t, store, "alice", "alice@example.com", "user")

	if err := store.CreateSession(ctx, &database.Session{
		UserID:    u.ID,
		TokenHash: "old",
		ExpiresAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := store.CreateSession(ctx, &database.Session{
		UserID:    u.ID,
		TokenHash: "fresh",
		ExpiresAt: time.Now().Add(time.Hour),
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	n, err := store.PurgeExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("PurgeExpiredSessions: %v", err)
	}
	if n != 1 {
		t.Errorf("purged %d sessions, want 1", n)
	}
	if _, _, err := store.SessionByHash(ctx, "fresh"); err != nil {
		t.Errorf("the live session was purged: %v", err)
	}
}

// TestSettingsRoundTrip checks the key/value settings table.
func TestSettingsRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	if got := store.GetSetting(ctx, "missing", "fallback"); got != "fallback" {
		t.Errorf("GetSetting for a missing key = %q, want the default", got)
	}
	if err := store.SetSetting(ctx, "theme", "dark"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := store.GetSetting(ctx, "theme", "fallback"); got != "dark" {
		t.Errorf("GetSetting = %q, want dark", got)
	}
	// Overwriting must replace, not duplicate.
	if err := store.SetSetting(ctx, "theme", "light"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if got := store.GetSetting(ctx, "theme", "fallback"); got != "light" {
		t.Errorf("GetSetting = %q, want light", got)
	}
}

// TestNodeRoundTrip checks the node table, which the engine manager uses.
func TestNodeRoundTrip(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	n := &database.Node{
		Name:     "edge-01",
		Endpoint: "unix:///var/run/docker.sock",
		Engine:   "docker",
	}
	if err := store.CreateNode(ctx, n); err != nil {
		t.Fatalf("CreateNode: %v", err)
	}
	if n.ID == 0 {
		t.Error("CreateNode did not assign an ID")
	}

	got, err := store.NodeByID(ctx, n.ID)
	if err != nil {
		t.Fatalf("NodeByID: %v", err)
	}
	if got.Name != "edge-01" {
		t.Errorf("NodeByID returned name %q", got.Name)
	}

	byEndpoint, err := store.NodeByEndpoint(ctx, "unix:///var/run/docker.sock")
	if err != nil {
		t.Fatalf("NodeByEndpoint: %v", err)
	}
	if byEndpoint.ID != n.ID {
		t.Errorf("NodeByEndpoint returned id %d, want %d", byEndpoint.ID, n.ID)
	}

	list, err := store.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("ListNodes returned %d nodes, want 1", len(list))
	}

	if err := store.DeleteNode(ctx, n.ID); err != nil {
		t.Fatalf("DeleteNode: %v", err)
	}
	if _, err := store.NodeByID(ctx, n.ID); !errors.Is(err, database.ErrNotFound) {
		t.Errorf("the node survived deletion (err=%v)", err)
	}
}
