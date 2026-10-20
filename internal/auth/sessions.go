// Package auth holds sign-in sessions.
//
// A session is a row, not a signed cookie: disabling someone in AD (picked up
// by the sync) must end their session at once, and that is a per-request
// lookup either way -- so the table is the mechanism, not a cost. The cookie
// carries an opaque random token; the table stores only its sha256.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

var ErrNoSession = errors.New("auth: no valid session")

type Sessions struct {
	db  *sql.DB
	ttl time.Duration
}

func NewSessions(db *sql.DB, ttl time.Duration) *Sessions { return &Sessions{db: db, ttl: ttl} }

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Create starts a session and returns the token for the cookie.
func (s *Sessions) Create(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
		VALUES (?, ?, ?, ?)`, hash(token), userID, now.Format(time.RFC3339), now.Add(s.ttl).Format(time.RFC3339))
	if err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return token, nil
}

// Lookup returns the signed-in user for a token, sliding its expiry forward.
// A disabled user has no session, whatever the table says.
func (s *Sessions) Lookup(ctx context.Context, token string) (users.User, error) {
	if token == "" {
		return users.User{}, ErrNoSession
	}
	now := time.Now().UTC()
	var u users.User
	err := s.db.QueryRowContext(ctx, `
		UPDATE sessions SET expires_at = ?
		WHERE token_hash = ? AND expires_at > ?
		  AND user_id IN (SELECT id FROM users WHERE disabled = 0)
		RETURNING user_id`,
		now.Add(s.ttl).Format(time.RFC3339), hash(token), now.Format(time.RFC3339)).Scan(&u.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return users.User{}, ErrNoSession
	}
	if err != nil {
		return users.User{}, err
	}
	return users.NewStore(s.db).ByID(ctx, u.ID)
}

func (s *Sessions) Delete(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash(token))
	return err
}

// DeleteExpired is housekeeping: expired rows are already refused on read.
func (s *Sessions) DeleteExpired(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
