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

	"github.com/17xande-dev/websec/session"

	"github.com/17xande-dev/leave/internal/users"
)

var ErrNoSession = errors.New("auth: no valid session")

// Sessions end by the Lifetime's absolute limit (counted from created_at) and
// idle limit (counted from last_seen). last_seen is written at most once a
// minute; nothing else is written per request.
type Sessions struct {
	db       *sql.DB
	lifetime session.Lifetime
	now      func() time.Time
}

// NewSessions refuses an invalid Lifetime, so a typo cannot mean "never expires".
func NewSessions(db *sql.DB, l session.Lifetime) (*Sessions, error) {
	if err := l.Validate(); err != nil {
		return nil, fmt.Errorf("auth: %w", err)
	}
	return &Sessions{db: db, lifetime: l, now: time.Now}, nil
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// stamp is the text form of every session time: UTC, RFC 3339, whole seconds,
// so string comparison in SQL orders the same as time does.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// Create starts a session and returns the token for the cookie.
func (s *Sessions) Create(ctx context.Context, userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := s.now().UTC()
	// expires_at is the absolute limit, fixed at creation and never moved.
	_, err := s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, created_at, last_seen, expires_at)
		VALUES (?, ?, ?, ?, ?)`, hash(token), userID, stamp(now), stamp(now), stamp(s.lifetime.AbsoluteExpiry(now)))
	if err != nil {
		return "", fmt.Errorf("auth: create session: %w", err)
	}
	return token, nil
}

// Lookup returns the signed-in user for a token. A disabled user has no
// session, whatever the table says, and a session past either limit is refused.
// It touches last_seen when it is a minute old (or in the future, after a clock
// step), which is the only write a request makes.
func (s *Sessions) Lookup(ctx context.Context, token string) (users.User, error) {
	if token == "" {
		return users.User{}, ErrNoSession
	}
	now := s.now().UTC()
	var (
		id                        int64
		createdS, seenS, expiresS string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT s.user_id, s.created_at, s.last_seen, s.expires_at FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = ? AND u.disabled = 0`, hash(token)).Scan(&id, &createdS, &seenS, &expiresS)
	if errors.Is(err, sql.ErrNoRows) {
		return users.User{}, ErrNoSession
	}
	if err != nil {
		return users.User{}, err
	}
	created, err1 := time.Parse(time.RFC3339, createdS)
	seen, err2 := time.Parse(time.RFC3339, seenS)
	expires, err3 := time.Parse(time.RFC3339, expiresS)
	if err := errors.Join(err1, err2, err3); err != nil {
		// A row nobody can date is not a session; refuse rather than guess.
		return users.User{}, ErrNoSession
	}
	// expires_at still holds for a row written before last_seen existed, whose
	// sliding expiry may be earlier than the absolute limit.
	if !now.Before(expires) || s.lifetime.Expired(created, seen, now) {
		return users.User{}, ErrNoSession
	}
	if session.NeedsTouch(seen, now) {
		// The same now in the predicate and the write keeps the throttle exact
		// under concurrent requests; a future last_seen is pulled back to now.
		if _, err := s.db.ExecContext(ctx, `UPDATE sessions SET last_seen = ?
			WHERE token_hash = ? AND (last_seen <= ? OR last_seen > ?)`,
			stamp(now), hash(token), stamp(now.Add(-session.TouchEvery)), stamp(now)); err != nil {
			return users.User{}, err
		}
	}
	return users.NewStore(s.db).ByID(ctx, id)
}

func (s *Sessions) Delete(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hash(token))
	return err
}

// DeleteExpired is housekeeping: expired rows are already refused on read. It
// removes rows past the absolute limit (by created_at, which also catches a
// sliding expires_at written before last_seen existed) or the idle limit.
func (s *Sessions) DeleteExpired(ctx context.Context) (int64, error) {
	now := s.now().UTC()
	q := `DELETE FROM sessions WHERE expires_at <= ? OR created_at <= ?`
	args := []any{stamp(now), stamp(now.Add(-s.lifetime.Absolute))}
	if !s.lifetime.IdleOff {
		q += ` OR last_seen <= ?`
		args = append(args, stamp(now.Add(-s.lifetime.Idle)))
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
