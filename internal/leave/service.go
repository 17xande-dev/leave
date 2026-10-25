package leave

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// Config is what is fixed at deployment. Everything an admin may change as
// policy lives in Settings, in the database.
type Config struct {
	Location *time.Location
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
	// beforeEditTx lets a test land a decision between Edit's first read and
	// its transaction; nil outside tests.
	beforeEditTx func()
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
	// EventDraftsCreated: a shutdown created drafts for someone to submit.
	// Request is their first draft; one event per person, not per draft.
	EventDraftsCreated EventKind = "drafts"
	// EventEscalated: an approver approved a request needing special
	// permission and it moved up to the next level, now Request.ApproverID.
	EventEscalated EventKind = "escalated"
	// EventNegativeRequested: someone asked HR to let them go below zero.
	EventNegativeRequested EventKind = "negative_requested"
	// EventNegativeGranted: HR allowed it; the request is a draft for the
	// person to submit.
	EventNegativeGranted EventKind = "negative_granted"
	// EventRecorded: an admin recorded leave the person already took. It is
	// approved from the start.
	EventRecorded EventKind = "recorded"
	// EventRevoked: an admin removed leave they had recorded. Request is the
	// now-cancelled leave.
	EventRevoked EventKind = "revoked"
	// EventEdited: the requester changed a pending request. Request is as it
	// is now and Before as it was; the approver is told.
	EventEdited EventKind = "edited"
)

type Event struct {
	Kind EventKind
	// Request is the request as it stands after the change.
	Request Request
	// Before is the request as it stood before an edit.
	Before *Request
	// Cancelled is the leave a just-approved cancellation cancelled.
	Cancelled *Request
	Actor     users.User
	// Coverage and Steps accompany an escalated request: the warnings as the
	// department and teams stand, and who has approved so far.
	Coverage []Coverage
	Steps    []Step
	// Shortfall is how many days below zero a negative-balance request asks for.
	Shortfall int
	// Note is what the actor said, where it is not on the request (a grant).
	Note string
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

// EarliestStart is the first day a request of type t may start on, as
// YYYY-MM-DD: the same cut-off Preview and Submit enforce, so the apply page's
// date picker can grey out what the server would refuse.
func (s *Service) EarliestStart(t Type) string {
	return fmtDate(s.today().AddDate(0, 0, -t.BackdateDays))
}

// Today is the current date in the organisation's timezone, as midnight UTC.
func (s *Service) Today() time.Time { return s.today() }

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
