// Package database owns the SQLite connection, schema migrations and all
// typed persistence helpers (users, sessions, nodes, settings).
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Sentinel errors returned by the store.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
)

// Store wraps the SQLite handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path and applies
// migrations. The modernc.org/sqlite driver is pure Go, so no CGO is needed.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite is single-writer; a small pool avoids lock contention while
	// still allowing concurrent reads under WAL.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the raw handle (used by health checks).
func (s *Store) DB() *sql.DB { return s.db }

const schemaVersion = 1

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version >= schemaVersion {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			username      TEXT NOT NULL UNIQUE,
			email         TEXT NOT NULL DEFAULT '',
			password_hash TEXT NOT NULL,
			role          TEXT NOT NULL DEFAULT 'user' CHECK (role IN ('admin','user')),
			created_at    INTEGER NOT NULL,
			last_login_at INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			ip         TEXT NOT NULL DEFAULT '',
			user_agent TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at)`,
		`CREATE TABLE IF NOT EXISTS nodes (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			name       TEXT NOT NULL,
			engine     TEXT NOT NULL DEFAULT 'auto',
			endpoint   TEXT NOT NULL UNIQUE,
			tls_ca     TEXT NOT NULL DEFAULT '',
			tls_cert   TEXT NOT NULL DEFAULT '',
			tls_key    TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", schemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- users ----

// User is an account that can sign in.
type User struct {
	ID           int64
	Username     string
	Email        string
	PasswordHash string
	Role         string
	CreatedAt    time.Time
	LastLoginAt  *time.Time
}

// IsAdmin reports whether the user has the admin role.
func (u *User) IsAdmin() bool { return u.Role == "admin" }

// CountUsers returns the number of accounts (used to trigger first-run setup).
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, err
}

// CountAdmins returns the number of admin accounts.
func (s *Store) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&n)
	return n, err
}

// CreateUser inserts a new account.
func (s *Store) CreateUser(ctx context.Context, u *User) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, email, password_hash, role, created_at) VALUES (?,?,?,?,?)`,
		u.Username, u.Email, u.PasswordHash, u.Role, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	u.ID, _ = res.LastInsertId()
	u.CreatedAt = time.Unix(now, 0)
	return nil
}

const userCols = `id, username, email, password_hash, role, created_at, last_login_at`

func scanUser(row interface{ Scan(...any) error }) (*User, error) {
	var u User
	var created int64
	var last sql.NullInt64
	if err := row.Scan(&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &created, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt = time.Unix(created, 0)
	if last.Valid {
		t := time.Unix(last.Int64, 0)
		u.LastLoginAt = &t
	}
	return &u, nil
}

// UserByLogin looks up an account by username or email (case-insensitive).
func (s *Store) UserByLogin(ctx context.Context, login string) (*User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+userCols+` FROM users WHERE username = ? COLLATE NOCASE OR (email != '' AND email = ? COLLATE NOCASE) LIMIT 1`,
		login, login)
	return scanUser(row)
}

// UserByID fetches an account by id.
func (s *Store) UserByID(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.db.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
}

// ListUsers returns all accounts ordered by creation.
func (s *Store) ListUsers(ctx context.Context) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+userCols+` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpdateUserRole changes a user's role.
func (s *Store) UpdateUserRole(ctx context.Context, id int64, role string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, role, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserPassword sets a new password hash.
func (s *Store) UpdateUserPassword(ctx context.Context, id int64, hash string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchLogin records a successful login timestamp.
func (s *Store) TouchLogin(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, time.Now().Unix(), id)
	return err
}

// DeleteUser removes an account and cascades its sessions.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ------------------------------------------------------------- sessions ----

// Session is a persisted login session.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
	IP        string
	UserAgent string
}

// CreateSession persists a session row.
func (s *Store) CreateSession(ctx context.Context, sess *Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at, ip, user_agent) VALUES (?,?,?,?,?,?)`,
		sess.TokenHash, sess.UserID, sess.CreatedAt.Unix(), sess.ExpiresAt.Unix(), sess.IP, sess.UserAgent)
	return err
}

// SessionByHash returns a session and its owning user if the session is valid.
func (s *Store) SessionByHash(ctx context.Context, hash string) (*Session, *User, error) {
	var sess Session
	var created, expires int64
	var u User
	var uCreated int64
	var uLast sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT s.token_hash, s.user_id, s.created_at, s.expires_at, s.ip, s.user_agent,
		       u.id, u.username, u.email, u.password_hash, u.role, u.created_at, u.last_login_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ?`, hash).
		Scan(&sess.TokenHash, &sess.UserID, &created, &expires, &sess.IP, &sess.UserAgent,
			&u.ID, &u.Username, &u.Email, &u.PasswordHash, &u.Role, &uCreated, &uLast)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	sess.CreatedAt = time.Unix(created, 0)
	sess.ExpiresAt = time.Unix(expires, 0)
	u.CreatedAt = time.Unix(uCreated, 0)
	if uLast.Valid {
		t := time.Unix(uLast.Int64, 0)
		u.LastLoginAt = &t
	}
	if time.Now().After(sess.ExpiresAt) {
		return nil, nil, ErrNotFound
	}
	return &sess, &u, nil
}

// DeleteSession removes one session by token hash.
func (s *Store) DeleteSession(ctx context.Context, hash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash)
	return err
}

// DeleteUserSessions removes every session for a user, optionally keeping one
// token hash (so a password change keeps the current device signed in).
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, keepHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND token_hash != ?`, userID, keepHash)
	return err
}

// PurgeExpiredSessions deletes expired rows; intended to run periodically.
func (s *Store) PurgeExpiredSessions(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------- nodes ----

// Node is a monitored container engine endpoint.
type Node struct {
	ID        int64
	Name      string
	Engine    string
	Endpoint  string
	TLSCA     string
	TLSCert   string
	TLSKey    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

const nodeCols = `id, name, engine, endpoint, tls_ca, tls_cert, tls_key, created_at, updated_at`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	var created, updated int64
	if err := row.Scan(&n.ID, &n.Name, &n.Engine, &n.Endpoint, &n.TLSCA, &n.TLSCert, &n.TLSKey, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	n.CreatedAt = time.Unix(created, 0)
	n.UpdatedAt = time.Unix(updated, 0)
	return &n, nil
}

// CreateNode inserts a node.
func (s *Store) CreateNode(ctx context.Context, n *Node) error {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO nodes (name, engine, endpoint, tls_ca, tls_cert, tls_key, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`,
		n.Name, n.Engine, n.Endpoint, n.TLSCA, n.TLSCert, n.TLSKey, now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	n.ID, _ = res.LastInsertId()
	n.CreatedAt = time.Unix(now, 0)
	n.UpdatedAt = n.CreatedAt
	return nil
}

// UpdateNode saves node connection settings.
func (s *Store) UpdateNode(ctx context.Context, n *Node) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE nodes SET name=?, engine=?, endpoint=?, tls_ca=?, tls_cert=?, tls_key=?, updated_at=? WHERE id=?`,
		n.Name, n.Engine, n.Endpoint, n.TLSCA, n.TLSCert, n.TLSKey, time.Now().Unix(), n.ID)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteNode removes a node by id.
func (s *Store) DeleteNode(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		return ErrNotFound
	}
	return nil
}

// NodeByID fetches one node.
func (s *Store) NodeByID(ctx context.Context, id int64) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE id = ?`, id))
}

// NodeByEndpoint fetches a node by its connection string.
func (s *Store) NodeByEndpoint(ctx context.Context, endpoint string) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM nodes WHERE endpoint = ?`, endpoint))
}

// ListNodes returns all configured nodes in creation order.
func (s *Store) ListNodes(ctx context.Context) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ------------------------------------------------------------- settings ----

// GetSetting returns a setting value or def when unset.
func (s *Store) GetSetting(ctx context.Context, key, def string) string {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err != nil {
		return def
	}
	return v
}

// SetSetting upserts a setting value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}
