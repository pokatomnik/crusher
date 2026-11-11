// Package goal contains the in-memory goal state used by the Crusher fork.
// Goal state is deliberately not persisted; only prompts sent through the
// ordinary agent pipeline are stored in conversation history.
package goal

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/charmbracelet/crush/internal/pubsub"
)

var (
	ErrGoalAlreadyActive  = errors.New("a goal is already active")
	ErrGoalAlreadyPending = errors.New("a goal installation is already pending")
	ErrGoalConflict       = errors.New("goal state changed; refresh and try again")
	ErrNoActiveGoal       = errors.New("no current goal is set")
	ErrGoalDisabled       = errors.New("goal completion is disabled by configuration")
)

// Snapshot is the public, immutable view of the single goal slot.
type Snapshot struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Revision    uint64 `json:"revision"`
	Version     uint64 `json:"version"`
	Text        string `json:"text,omitempty"`
	Active      bool   `json:"active"`
	Pending     bool   `json:"pending"`
}

// Token identifies one logical installation across its pending and active
// phases. It is also used to fence stale UI actions and tool calls.
type Token struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	Revision    uint64 `json:"revision"`
}

// Ticket identifies an accepted installation whose initial prompt has not
// yet been successfully sent to a model.
type Ticket struct {
	Token Token  `json:"token"`
	Text  string `json:"text"`
}

// Completion is the model-reported outcome. Both success values close the
// goal; Success describes the outcome rather than tool execution success.
type Completion struct {
	Result  string `json:"result"`
	Success bool   `json:"success"`
}

// Wake identifies one claimed automatic follow-up.
type Wake struct {
	Token      Token
	Generation uint64
}

// Service owns one process-wide goal slot. It does not know about sessions,
// persistence, agents, transport, or UI.
type Service struct {
	mu sync.Mutex

	snapshot Snapshot
	ticket   *Ticket
	paused   bool

	wakeGeneration uint64
	wakeClaimed    bool

	events *pubsub.Broker[Snapshot]
}

// NewService returns an empty in-memory goal service.
func NewService() *Service {
	return &Service{events: pubsub.NewBroker[Snapshot]()}
}

// Subscribe subscribes to public snapshot changes.
func (s *Service) Subscribe(ctx context.Context) <-chan pubsub.Event[Snapshot] {
	return s.events.Subscribe(ctx)
}

// Get returns the current global snapshot.
func (s *Service) Get() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

// RequestInstallation reserves the only slot without activating a goal.
func (s *Service) RequestInstallation(workspaceID, sessionID, text string) (Ticket, Snapshot, error) {
	if workspaceID == "" || sessionID == "" || strings.TrimSpace(text) == "" {
		return Ticket{}, s.Get(), errors.New("workspace, session, and non-empty goal text are required")
	}

	s.mu.Lock()
	if s.snapshot.Active {
		snapshot := s.snapshot
		s.mu.Unlock()
		return Ticket{}, snapshot, ErrGoalAlreadyActive
	}
	if s.snapshot.Pending {
		snapshot := s.snapshot
		s.mu.Unlock()
		return Ticket{}, snapshot, ErrGoalAlreadyPending
	}

	s.snapshot.Revision++
	if s.snapshot.Revision == 0 {
		s.snapshot.Revision++
	}
	s.snapshot.Version++
	ticket := Ticket{
		Token: Token{WorkspaceID: workspaceID, SessionID: sessionID, Revision: s.snapshot.Revision},
		Text:  text,
	}
	s.ticket = &ticket
	s.paused = false
	s.snapshot.WorkspaceID = workspaceID
	s.snapshot.SessionID = sessionID
	s.snapshot.Text = text
	s.snapshot.Active = false
	s.snapshot.Pending = true
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.CreatedEvent, snapshot)
	return ticket, snapshot, nil
}

// Activate promotes a still-current pending installation after its initial
// user message has been persisted and its provider stream has started.
func (s *Service) Activate(ticket Ticket) (Snapshot, bool) {
	s.mu.Lock()
	if !s.snapshot.Pending || s.ticket == nil || s.ticket.Token != ticket.Token {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, false
	}
	s.snapshot.Version++
	s.snapshot.Pending = false
	s.snapshot.Active = true
	s.ticket = nil
	s.paused = false
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.UpdatedEvent, snapshot)
	return snapshot, true
}

// IsPending reports whether ticket still owns the pending slot.
func (s *Service) IsPending(ticket Ticket) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot.Pending && s.ticket != nil && s.ticket.Token == ticket.Token
}

// CancelPending invalidates an initial prompt that has not started a model
// stream. It never turns a pending request into a paused active goal.
func (s *Service) CancelPending(ticket Ticket) (Snapshot, bool) {
	s.mu.Lock()
	if !s.snapshot.Pending || s.ticket == nil || s.ticket.Token != ticket.Token {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, false
	}
	s.clearLocked()
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.DeletedEvent, snapshot)
	return snapshot, true
}

// Cancel clears the current pending or active slot if expected matches. An
// already-empty slot is an idempotent success and does not publish an event.
func (s *Service) Cancel(expected Token) (Snapshot, error) {
	s.mu.Lock()
	if !s.snapshot.Active && !s.snapshot.Pending {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, nil
	}
	if !matchesToken(s.snapshot, expected) {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, ErrGoalConflict
	}
	s.clearLocked()
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.DeletedEvent, snapshot)
	return snapshot, nil
}

// Complete closes only an active goal identified by expected. A pending
// installation is deliberately unaffected.
func (s *Service) Complete(expected Token, completion Completion) (Snapshot, error) {
	s.mu.Lock()
	if !s.snapshot.Active {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, ErrNoActiveGoal
	}
	if !matchesToken(s.snapshot, expected) {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, ErrGoalConflict
	}
	s.clearLocked()
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.DeletedEvent, snapshot)
	return snapshot, nil
}

// Forget removes a slot only when it belongs to the specified owner.
func (s *Service) Forget(workspaceID, sessionID string) (Snapshot, bool) {
	s.mu.Lock()
	if (!s.snapshot.Active && !s.snapshot.Pending) ||
		s.snapshot.WorkspaceID != workspaceID ||
		s.snapshot.SessionID != sessionID {
		snapshot := s.snapshot
		s.mu.Unlock()
		return snapshot, false
	}
	s.clearLocked()
	snapshot := s.snapshot
	s.mu.Unlock()

	s.events.Publish(pubsub.DeletedEvent, snapshot)
	return snapshot, true
}

// Pause disables automatic follow-ups for the current active goal. It is
// internal runtime state and does not alter the public snapshot.
func (s *Service) Pause(expected Token) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Active || !matchesToken(s.snapshot, expected) {
		return false
	}
	s.paused = true
	s.invalidateWakeLocked()
	return true
}

// PauseWake pauses only the still-current automatic run that owns wake. A
// late error from an older run must not pause a goal resumed by newer input.
func (s *Service) PauseWake(wake Wake) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Active || s.paused || !matchesToken(s.snapshot, wake.Token) ||
		!s.wakeClaimed || s.wakeGeneration != wake.Generation {
		return false
	}
	s.paused = true
	s.invalidateWakeLocked()
	return true
}

// ResumeOnMainRun lets an admitted run from the owning main agent supersede an
// automatic wake and clears any internal pause.
func (s *Service) ResumeOnMainRun(workspaceID, sessionID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Active || s.snapshot.WorkspaceID != workspaceID || s.snapshot.SessionID != sessionID {
		return false
	}
	s.paused = false
	s.invalidateWakeLocked()
	return true
}

// IsPaused reports internal pause state for lifecycle decisions.
func (s *Service) IsPaused(expected Token) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot.Active && matchesToken(s.snapshot, expected) && s.paused
}

// ClaimWake atomically claims the one automatic follow-up slot.
func (s *Service) ClaimWake(expected Token) (Wake, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.snapshot.Active || s.paused || !matchesToken(s.snapshot, expected) || s.wakeClaimed {
		return Wake{}, false
	}
	s.wakeGeneration++
	if s.wakeGeneration == 0 {
		s.wakeGeneration++
	}
	s.wakeClaimed = true
	return Wake{Token: expected, Generation: s.wakeGeneration}, true
}

// ReleaseWake releases a still-current automatic follow-up claim.
func (s *Service) ReleaseWake(wake Wake) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.wakeClaimed || s.wakeGeneration != wake.Generation || !matchesToken(s.snapshot, wake.Token) {
		return false
	}
	s.wakeClaimed = false
	return true
}

// WakeCurrent reports whether wake is still eligible to dispatch.
func (s *Service) WakeCurrent(wake Wake) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.wakeClaimed && s.wakeGeneration == wake.Generation && s.snapshot.Active && !s.paused && matchesToken(s.snapshot, wake.Token)
}

// AdmitWake runs a short RAM-only admission callback while the wake token is
// still current. The caller must hold its own dispatch and accepted locks
// before calling this method; the callback must not perform I/O or call back
// into Service. This makes goal validation and session active-registration one
// ordered admission point against concurrent Cancel/Complete.
func (s *Service) AdmitWake(wake Wake, admit func() bool) bool {
	if admit == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.snapshot.Active && !s.paused && matchesToken(s.snapshot, wake.Token)
	if wake.Generation != 0 {
		current = current && s.wakeClaimed && s.wakeGeneration == wake.Generation
	}
	return current && admit()
}

// Shutdown closes event subscriptions. The service is not reusable afterward.
func (s *Service) Shutdown() {
	s.events.Shutdown()
}

func (s *Service) clearLocked() {
	s.snapshot.Version++
	s.snapshot.WorkspaceID = ""
	s.snapshot.SessionID = ""
	s.snapshot.Text = ""
	s.snapshot.Active = false
	s.snapshot.Pending = false
	s.ticket = nil
	s.paused = false
	s.invalidateWakeLocked()
}

func (s *Service) invalidateWakeLocked() {
	s.wakeGeneration++
	if s.wakeGeneration == 0 {
		s.wakeGeneration++
	}
	s.wakeClaimed = false
}

func matchesToken(snapshot Snapshot, token Token) bool {
	return snapshot.WorkspaceID == token.WorkspaceID && snapshot.SessionID == token.SessionID && snapshot.Revision == token.Revision
}
