package leave

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

type Config struct {
	Location *time.Location
	// FinalApprover is the username that approves for anyone with no usable
	// manager in the directory.
	FinalApprover    string
	SickCycleAnchor  int
	SickBackdateDays int
}

// Service is the leave domain. Every state change runs in one transaction that
// also calls the event hook, so the notification and calendar jobs it enqueues
// commit with the change or not at all -- never an approval nobody hears
// about, never an email about an approval that rolled back.
type Service struct {
	db    *sql.DB
	users *users.Store
	cfg   Config
	hook  EventHook
	// committed runs after every successful state change, outside the
	// transaction: the place to wake whatever reads what the hook wrote.
	committed func()
	// Now is replaceable so tests can pin "today".
	Now func() time.Time
}

func New(db *sql.DB, us *users.Store, cfg Config) *Service {
	if cfg.Location == nil {
		cfg.Location = time.UTC
	}
	return &Service{db: db, users: us, cfg: cfg, Now: time.Now,
		hook:      func(context.Context, *sql.Tx, Event) error { return nil },
		committed: func() {}}
}

// EventKind names a state change worth telling someone about.
type EventKind string

const (
	EventSubmitted  EventKind = "submitted"  // leave or cancellation awaiting a decision
	EventDecided    EventKind = "decided"    // approved or declined
	EventWithdrawn  EventKind = "withdrawn"  // the requester withdrew it while pending
	EventReassigned EventKind = "reassigned" // an admin moved it to another approver
)

type Event struct {
	Kind EventKind
	// Request is the request as it stands after the change.
	Request Request
	// Cancelled is the leave a just-approved cancellation cancelled.
	Cancelled *Request
	Actor     users.User
}

// EventHook runs inside the transaction making the change. An error rolls the
// change back.
type EventHook func(ctx context.Context, tx *sql.Tx, e Event) error

func (s *Service) OnEvent(h EventHook) { s.hook = h }

// OnCommit registers f to run after each committed state change.
func (s *Service) OnCommit(f func()) { s.committed = f }

// ErrNotAllowed is a transition the request's state or the actor's role does
// not permit: approving your own leave, withdrawing a decided request, acting
// twice on one request from two tabs.
var ErrNotAllowed = errors.New("leave: that is not possible for this request")

func (s *Service) today() time.Time { return Today(s.Now(), s.cfg.Location) }

func stampNow(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// withTx runs f in a transaction. _txlock=immediate (see db.Open) makes BEGIN
// take the write lock, so every read inside f is consistent with the write that
// follows it: two approvers clicking at once serialise here.
func (s *Service) withTx(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.committed()
	return nil
}
