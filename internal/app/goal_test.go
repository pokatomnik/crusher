package app

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/fantasy"
	"github.com/charmbracelet/crush/internal/agent"
	"github.com/charmbracelet/crush/internal/config"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/charmbracelet/crush/internal/session"
	"github.com/stretchr/testify/require"
)

type goalTestSessions struct {
	session.Service
	mu       sync.RWMutex
	sessions map[string]session.Session
	getErr   error
}

type blockingGoalGetSessions struct {
	*goalTestSessions
	started chan struct{}
	release chan struct{}
}

func (s *blockingGoalGetSessions) Get(ctx context.Context, id string) (session.Session, error) {
	close(s.started)
	<-s.release
	return s.goalTestSessions.Get(ctx, id)
}

func (s *goalTestSessions) Get(_ context.Context, id string) (session.Session, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.getErr != nil {
		return session.Session{}, s.getErr
	}
	sess, ok := s.sessions[id]
	if !ok {
		return session.Session{}, sql.ErrNoRows
	}
	return sess, nil
}

func (s *goalTestSessions) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	return nil
}

func (s *goalTestSessions) setGetError(err error) {
	s.mu.Lock()
	s.getErr = err
	s.mu.Unlock()
}

type goalTestRun struct {
	ctx       context.Context
	sessionID string
	prompt    string
}

type goalTestCoordinator struct {
	agent.Coordinator
	busy                atomic.Bool
	busyFunc            func() bool
	runFunc             func(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error)
	goalRemovalMu       sync.Mutex
	removedGoalInitials []goal.Token
	deferredGoalWakes   []goal.Token
}

func (c *goalTestCoordinator) Run(ctx context.Context, sessionID, prompt string, attachments ...message.Attachment) (*fantasy.AgentResult, error) {
	if c.runFunc != nil {
		return c.runFunc(ctx, sessionID, prompt, attachments...)
	}
	return nil, nil
}

func (c *goalTestCoordinator) IsSessionBusy(string) bool {
	if c.busyFunc != nil {
		return c.busyFunc()
	}
	return c.busy.Load()
}

func (c *goalTestCoordinator) CancelAll() {}

func (c *goalTestCoordinator) RemoveGoalInitial(token goal.Token) {
	c.goalRemovalMu.Lock()
	c.removedGoalInitials = append(c.removedGoalInitials, token)
	c.goalRemovalMu.Unlock()
}

func (c *goalTestCoordinator) DeferGoalWake(token goal.Token) {
	c.goalRemovalMu.Lock()
	c.deferredGoalWakes = append(c.deferredGoalWakes, token)
	c.goalRemovalMu.Unlock()
}

func (c *goalTestCoordinator) removedGoalInitialTokens() []goal.Token {
	c.goalRemovalMu.Lock()
	defer c.goalRemovalMu.Unlock()
	return append([]goal.Token(nil), c.removedGoalInitials...)
}

func (c *goalTestCoordinator) deferredGoalWakeTokens() []goal.Token {
	c.goalRemovalMu.Lock()
	defer c.goalRemovalMu.Unlock()
	return append([]goal.Token(nil), c.deferredGoalWakes...)
}

func newGoalRuntimeTestApp(t *testing.T, sessions session.Service, coordinator *goalTestCoordinator) (*App, *goal.Service) {
	t.Helper()
	service := goal.NewService()
	events := pubsub.NewBroker[tea.Msg]()
	app := &App{
		Sessions:         sessions,
		AgentCoordinator: coordinator,
		globalCtx:        t.Context(),
		events:           events,
		goalService:      service,
		goalWorkspaceID:  "workspace-a",
	}
	app.eventsCtx = t.Context()
	require.NoError(t, app.startGoalRuntime())
	t.Cleanup(func() {
		app.StopGoalRuntime()
		app.waitGoalRuntime()
		app.stopGoalEventFanIn()
		service.Shutdown()
		events.Shutdown()
	})
	return app, service
}

func TestResolveGoalOptionsUsesPrivateLocalServiceAndInjectedIdentity(t *testing.T) {
	t.Parallel()

	firstService, firstID, firstOwns, err := resolveGoalOptions(nil)
	require.NoError(t, err)
	t.Cleanup(firstService.Shutdown)
	secondService, secondID, secondOwns, err := resolveGoalOptions(nil)
	require.NoError(t, err)
	t.Cleanup(secondService.Shutdown)
	require.NotSame(t, firstService, secondService)
	require.NotEmpty(t, firstID)
	require.NotEqual(t, firstID, secondID)
	require.True(t, firstOwns)
	require.True(t, secondOwns)

	shared := goal.NewService()
	t.Cleanup(shared.Shutdown)
	injectedService, injectedID, injectedOwns, err := resolveGoalOptions([]GoalOptions{{Service: shared, WorkspaceID: "workspace-server"}})
	require.NoError(t, err)
	require.Same(t, shared, injectedService)
	require.Equal(t, "workspace-server", injectedID)
	require.False(t, injectedOwns)
}

func TestAppSetGoalDispatchesInitialPromptAsPending(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-a": {ID: "session-a"},
	}}
	calls := make(chan goalTestRun, 1)
	var svc *goal.Service
	coordinator := &goalTestCoordinator{}
	coordinator.busy.Store(true)
	coordinator.runFunc = func(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
		calls <- goalTestRun{ctx: ctx, sessionID: sessionID, prompt: prompt}
		if marker, ok := goal.CallMarkerFromContext(ctx); ok && marker.Kind == goal.CallInitial {
			svc.Activate(marker.Ticket)
		}
		return nil, nil
	}
	app, service := newGoalRuntimeTestApp(t, sessions, coordinator)
	svc = service

	pending, err := app.SetGoal(t.Context(), "session-a", "Research this topic")
	require.NoError(t, err)
	require.True(t, pending.Pending)
	require.False(t, pending.Active)

	select {
	case call := <-calls:
		require.Equal(t, "session-a", call.sessionID)
		require.Contains(t, call.prompt, "Research this topic")
		require.False(t, message.HiddenUserMessage(call.ctx))
		marker, ok := goal.CallMarkerFromContext(call.ctx)
		require.True(t, ok)
		require.Equal(t, goal.CallInitial, marker.Kind)
		require.Equal(t, pending.Revision, marker.Ticket.Token.Revision)
	case <-time.After(2 * time.Second):
		t.Fatal("initial goal prompt was not dispatched")
	}
	require.Eventually(t, func() bool { return service.Get().Active }, 2*time.Second, 10*time.Millisecond)
}

func TestAppInitialIngressDoesNotWaitForPreviousFollowup(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-a": {ID: "session-a"},
		"session-b": {ID: "session-b"},
	}}
	followupStarted := make(chan struct{})
	releaseFollowup := make(chan struct{})
	initialStarted := make(chan struct{}, 1)
	coordinator := &goalTestCoordinator{runFunc: func(ctx context.Context, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
		marker, _ := goal.CallMarkerFromContext(ctx)
		if marker.Kind == goal.CallFollowup {
			close(followupStarted)
			<-releaseFollowup
		} else if marker.Kind == goal.CallInitial {
			initialStarted <- struct{}{}
		}
		return nil, nil
	}}
	app, service := newGoalRuntimeTestApp(t, sessions, coordinator)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseFollowup) }) }
	t.Cleanup(release)
	oldTicket, _, err := service.RequestInstallation("workspace-a", "session-a", "old goal")
	require.NoError(t, err)
	_, ok := service.Activate(oldTicket)
	require.True(t, ok)
	select {
	case <-followupStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("previous follow-up did not start")
	}

	_, err = app.CancelGoal(t.Context(), oldTicket.Token)
	require.NoError(t, err)
	pending, err := app.SetGoal(t.Context(), "session-b", "new goal")
	require.NoError(t, err)
	require.True(t, pending.Pending)
	select {
	case <-initialStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("initial ingress waited behind the active follow-up")
	}
	release()
}

func TestAppGoalFollowupUsesClaimAndHiddenMarker(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-a": {ID: "session-a"},
	}}
	calls := make(chan goalTestRun, 1)
	coordinator := &goalTestCoordinator{runFunc: func(ctx context.Context, sessionID, prompt string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
		calls <- goalTestRun{ctx: ctx, sessionID: sessionID, prompt: prompt}
		return nil, nil
	}}
	_, service := newGoalRuntimeTestApp(t, sessions, coordinator)
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "continue the work")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	select {
	case call := <-calls:
		require.Equal(t, "session-a", call.sessionID)
		require.Contains(t, call.prompt, "continue the work")
		require.True(t, message.HiddenUserMessage(call.ctx))
		marker, ok := goal.CallMarkerFromContext(call.ctx)
		require.True(t, ok)
		require.Equal(t, goal.CallFollowup, marker.Kind)
		require.Equal(t, ticket.Token, marker.Token)
	case <-time.After(2 * time.Second):
		t.Fatal("goal follow-up was not dispatched")
	}
}

func TestAppGoalWorkerDefersWakeAtBothBusyChecks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		busyFunc func(*atomic.Int32) func() bool
	}{
		{
			name: "busy before claim",
			busyFunc: func(*atomic.Int32) func() bool {
				return func() bool { return true }
			},
		},
		{
			name: "busy after claim",
			busyFunc: func(checks *atomic.Int32) func() bool {
				return func() bool { return checks.Add(1) == 2 }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := goal.NewService()
			var checks atomic.Int32
			coordinator := &goalTestCoordinator{busyFunc: tc.busyFunc(&checks)}
			app := &App{
				Sessions:         &goalTestSessions{sessions: map[string]session.Session{"session-a": {ID: "session-a"}}},
				AgentCoordinator: coordinator,
				goalService:      service,
				goalWorkspaceID:  "workspace-a",
			}
			ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
			require.NoError(t, err)
			_, ok := service.Activate(ticket)
			require.True(t, ok)

			app.runGoalFollowup(t.Context())
			require.Equal(t, []goal.Token{ticket.Token}, coordinator.deferredGoalWakeTokens())
			require.True(t, service.Get().Active)
			require.False(t, service.IsPaused(ticket.Token))
		})
	}
}

func TestGoalFollowupCoordinatorErrorPausesItsCurrentWake(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	coordinator := &goalTestCoordinator{runFunc: func(context.Context, string, string, ...message.Attachment) (*fantasy.AgentResult, error) {
		return nil, errors.New("coordinator preflight failed")
	}}
	app := &App{
		Sessions:         &goalTestSessions{sessions: map[string]session.Session{"session-a": {ID: "session-a"}}},
		AgentCoordinator: coordinator,
		goalService:      service,
		goalWorkspaceID:  "workspace-a",
	}
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	app.runGoalFollowup(t.Context())
	require.True(t, service.IsPaused(ticket.Token), "a current preflight error must stop automatic retries")
}

func TestGoalFollowupErrorCannotPauseAfterNewRunAdmission(t *testing.T) {
	t.Parallel()

	service := goal.NewService()
	started := make(chan struct{})
	release := make(chan struct{})
	markers := make(chan goal.CallMarker, 1)
	coordinator := &goalTestCoordinator{runFunc: func(ctx context.Context, _, _ string, _ ...message.Attachment) (*fantasy.AgentResult, error) {
		marker, ok := goal.CallMarkerFromContext(ctx)
		if !ok {
			return nil, errors.New("goal marker was missing")
		}
		markers <- marker
		close(started)
		<-release
		return nil, errors.New("stale follow-up error")
	}}
	app := &App{
		Sessions:         &goalTestSessions{sessions: map[string]session.Session{"session-a": {ID: "session-a"}}},
		AgentCoordinator: coordinator,
		goalService:      service,
		goalWorkspaceID:  "workspace-a",
	}
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	done := make(chan struct{})
	go func() {
		app.runGoalFollowup(t.Context())
		close(done)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("goal follow-up did not reach the coordinator")
	}
	select {
	case marker := <-markers:
		require.Equal(t, goal.CallFollowup, marker.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("goal follow-up marker was not captured")
	}
	require.False(t, service.IsPaused(ticket.Token))
	require.True(t, service.ResumeOnMainRun("workspace-a", "session-a"), "a newer main run supersedes the still-active wake")
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goal follow-up did not return")
	}
	require.False(t, service.IsPaused(ticket.Token), "the old worker error must not undo the newer resume")
}

func TestAppSetGoalReclaimsConfirmedMissingOwner(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-b": {ID: "session-b"},
	}}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	oldTicket, _, err := service.RequestInstallation("workspace-a", "deleted-session", "old goal")
	require.NoError(t, err)
	_, ok := service.Activate(oldTicket)
	require.True(t, ok)

	snapshot, err := app.SetGoal(t.Context(), "session-b", "new goal")
	require.NoError(t, err)
	require.True(t, snapshot.Pending)
	require.Equal(t, "session-b", snapshot.SessionID)
}

func TestAppGoalEventsAreForwardedToAppEvents(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{}}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	events := app.events.Subscribe(t.Context())
	_, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
	require.NoError(t, err)

	select {
	case forwarded := <-events:
		goalEvent, ok := forwarded.Payload.(pubsub.Event[goal.Snapshot])
		require.True(t, ok)
		require.Equal(t, "session-a", goalEvent.Payload.SessionID)
	case <-time.After(2 * time.Second):
		t.Fatal("goal service event was not forwarded")
	}
}

func TestAppGetGoalKeepsOwnerOnTransientSessionError(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-a": {ID: "session-a"},
	}}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	sessions.setGetError(errors.New("database unavailable"))
	snapshot, err := app.GetGoal(t.Context())
	require.ErrorContains(t, err, "database unavailable")
	require.True(t, snapshot.Active)
	require.Equal(t, ticket.Token.Revision, service.Get().Revision)
}

func TestAppGetGoalForgetsOnlyConfirmedMissingOwner(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{}}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	_, _, err := service.RequestInstallation("workspace-a", "missing", "goal")
	require.NoError(t, err)

	snapshot, err := app.GetGoal(t.Context())
	require.NoError(t, err)
	require.False(t, snapshot.Active)
	require.False(t, snapshot.Pending)
}

func TestAppSetGoalRejectsChildAndDisabledToolWithoutReservation(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"child": {ID: "child", ParentSessionID: "root"},
		"root":  {ID: "root"},
	}}
	coordinator := &goalTestCoordinator{}
	app, service := newGoalRuntimeTestApp(t, sessions, coordinator)
	app.config = config.NewTestStore(&config.Config{
		Options: &config.Options{DisabledTools: []string{completeGoalToolName}},
	})

	_, err := app.SetGoal(t.Context(), "child", "goal")
	require.ErrorIs(t, err, goal.ErrGoalDisabled)
	require.False(t, service.Get().Pending)

	app.config = config.NewTestStore(&config.Config{Options: &config.Options{}})
	_, err = app.SetGoal(t.Context(), "child", "goal")
	require.ErrorIs(t, err, ErrGoalChildSession)
	require.False(t, service.Get().Pending)
}

func TestAppSetGoalRechecksToolDisableAfterSessionLookup(t *testing.T) {
	t.Parallel()

	base := &goalTestSessions{sessions: map[string]session.Session{"session-a": {ID: "session-a"}}}
	sessions := &blockingGoalGetSessions{
		goalTestSessions: base,
		started:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	result := make(chan error, 1)
	go func() {
		_, err := app.SetGoal(t.Context(), "session-a", "goal")
		result <- err
	}()

	select {
	case <-sessions.started:
	case <-time.After(2 * time.Second):
		t.Fatal("SetGoal did not reach the session lookup")
	}
	app.config = config.NewTestStore(&config.Config{
		Options: &config.Options{DisabledTools: []string{completeGoalToolName}},
	})
	close(sessions.release)
	select {
	case err := <-result:
		require.ErrorIs(t, err, goal.ErrGoalDisabled)
	case <-time.After(2 * time.Second):
		t.Fatal("SetGoal did not return after the session lookup")
	}
	require.False(t, service.Get().Pending, "a disable applied during lookup must not reserve a goal")
}

func TestDisabledGoalHotReloadRemovesPendingInitial(t *testing.T) {
	t.Parallel()

	coordinator := &goalTestCoordinator{}
	app, service := newGoalRuntimeTestApp(t, &goalTestSessions{sessions: map[string]session.Session{}}, coordinator)
	app.config = config.NewTestStore(&config.Config{
		Options: &config.Options{DisabledTools: []string{completeGoalToolName}},
	})
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "pending initial")
	require.NoError(t, err)

	app.cancelDisabledGoal()
	require.False(t, service.Get().Pending)
	require.Equal(t, []goal.Token{ticket.Token}, coordinator.removedGoalInitialTokens())
}

func TestAppCancelGoalRemovesPendingInitialFromCoordinator(t *testing.T) {
	t.Parallel()

	coordinator := &goalTestCoordinator{}
	app, service := newGoalRuntimeTestApp(t, &goalTestSessions{sessions: map[string]session.Session{}}, coordinator)
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "pending initial")
	require.NoError(t, err)

	snapshot, err := app.CancelGoal(t.Context(), ticket.Token)
	require.NoError(t, err)
	require.False(t, snapshot.Pending)
	require.Equal(t, []goal.Token{ticket.Token}, coordinator.removedGoalInitialTokens())
}

func TestStopGoalRuntimeClearsOwnedGoalAndClosesAdmission(t *testing.T) {
	t.Parallel()

	sessions := &goalTestSessions{sessions: map[string]session.Session{
		"session-a": {ID: "session-a"},
	}}
	app, service := newGoalRuntimeTestApp(t, sessions, &goalTestCoordinator{})
	_, _, err := service.RequestInstallation("workspace-a", "session-a", "owned goal")
	require.NoError(t, err)

	app.StopGoalRuntime()
	app.StopGoalRuntime()
	require.False(t, service.Get().Pending)
	_, err = app.SetGoal(t.Context(), "session-a", "later goal")
	require.ErrorIs(t, err, ErrGoalRuntimeStopped)
	require.False(t, service.Get().Pending)
}

func TestStopGoalRuntimeLeavesForeignGoalUntouched(t *testing.T) {
	t.Parallel()

	app, service := newGoalRuntimeTestApp(t, &goalTestSessions{sessions: map[string]session.Session{}}, &goalTestCoordinator{})
	_, pending, err := service.RequestInstallation("workspace-b", "session-b", "foreign goal")
	require.NoError(t, err)
	require.True(t, pending.Pending)

	app.StopGoalRuntime()
	require.Equal(t, pending, service.Get())
}

func TestGoalSessionDeleteForgetsOnlyDeletedOwner(t *testing.T) {
	t.Parallel()

	base := &goalTestSessions{sessions: map[string]session.Session{"session-a": {ID: "session-a"}}}
	service := goal.NewService()
	t.Cleanup(service.Shutdown)
	appSessions := newGoalSessionService(base, service, "workspace-a")
	ticket, _, err := service.RequestInstallation("workspace-a", "session-a", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	require.NoError(t, appSessions.Delete(t.Context(), "session-a"))
	require.False(t, service.Get().Active)

	foreign, _, err := service.RequestInstallation("workspace-b", "session-a", "foreign goal")
	require.NoError(t, err)
	_, ok = service.Activate(foreign)
	require.True(t, ok)
	require.NoError(t, appSessions.Delete(t.Context(), "session-a"))
	require.True(t, service.Get().Active)
}
