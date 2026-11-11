package app

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/agent"
	agenttools "github.com/charmbracelet/crush/internal/agent/tools"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/message"
	"github.com/charmbracelet/crush/internal/pubsub"
	"github.com/google/uuid"
)

var (
	ErrGoalCoordinatorNotReady = errors.New("goal requires an initialized agent coordinator")
	ErrGoalRuntimeStopped      = errors.New("application is shutting down")
	ErrGoalChildSession        = errors.New("goals can only be set on a root session")
)

const completeGoalToolName = agenttools.CompleteGoalToolName

// GoalOptions injects the backend-owned goal service into a workspace App.
// When omitted, New creates a private RAM-only service for the local App.
type GoalOptions struct {
	Service     *goal.Service
	WorkspaceID string
}

func resolveGoalOptions(options []GoalOptions) (*goal.Service, string, bool, error) {
	if len(options) > 1 {
		return nil, "", false, errors.New("at most one GoalOptions value may be provided")
	}
	if len(options) == 1 {
		if options[0].Service == nil || options[0].WorkspaceID == "" {
			return nil, "", false, errors.New("goal service and workspace ID are required together")
		}
		return options[0].Service, options[0].WorkspaceID, false, nil
	}
	return goal.NewService(), uuid.NewString(), true, nil
}

func (app *App) startGoalRuntime() error {
	if app.goalService == nil || app.goalWorkspaceID == "" {
		return errors.New("goal service and workspace ID are required")
	}

	parent := app.globalCtx
	if parent == nil {
		parent = context.Background()
	}
	goalCtx, goalCancel := context.WithCancel(parent)
	eventParent := app.eventsCtx
	if eventParent == nil {
		eventParent = parent
	}
	eventCtx, eventCancel := context.WithCancel(eventParent)

	// Subscribe before launching the fan-in goroutine: a goal mutation cannot
	// land in the gap between App construction and subscriber registration.
	goalEvents := app.goalService.Subscribe(eventCtx)
	app.goalRuntimeMu.Lock()
	app.goalCtx = goalCtx
	app.goalCancel = goalCancel
	app.goalWake = make(chan struct{}, 1)
	app.goalEventCancel = eventCancel
	app.goalRuntimeMu.Unlock()

	app.goalWorkerWG.Add(1)
	go app.runGoalWorker(goalCtx)
	app.goalEventWG.Add(1)
	go app.forwardGoalEvents(eventCtx, goalEvents)
	return nil
}

func (app *App) forwardGoalEvents(ctx context.Context, events <-chan pubsub.Event[goal.Snapshot]) {
	defer app.goalEventWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			msg := tea.Msg(event)
			if event.MustDeliver {
				app.events.PublishMustDeliver(ctx, event.Type, msg)
			} else {
				app.events.Publish(event.Type, msg)
			}
			app.signalGoalWorker()
		}
	}
}

func (app *App) goalWakeForGeneration(generation uint64) func() {
	return func() {
		if app.goalCoordinatorGen.Load() == generation {
			app.signalGoalWorker()
		}
	}
}

func (app *App) signalGoalWorker() {
	app.goalRuntimeMu.Lock()
	defer app.goalRuntimeMu.Unlock()
	if app.goalRuntimeStopped || app.goalWake == nil {
		return
	}
	select {
	case app.goalWake <- struct{}{}:
	default:
	}
}

// SetGoal reserves the global slot and starts delivery of its initial prompt
// asynchronously. The returned snapshot is pending until the provider stream
// for that ordinary Coordinator.Run call is successfully started.
func (app *App) SetGoal(ctx context.Context, sessionID, text string) (goal.Snapshot, error) {
	if app.goalService == nil {
		return goal.Snapshot{}, errors.New("goal service is unavailable")
	}
	coordinator := app.goalCoordinator()
	if coordinator == nil {
		return app.goalService.Get(), ErrGoalCoordinatorNotReady
	}
	if app.goalDisabled() {
		return app.goalService.Get(), goal.ErrGoalDisabled
	}
	if !app.goalAccepting() {
		return app.goalService.Get(), ErrGoalRuntimeStopped
	}

	if err := app.reconcileGoalOwner(ctx, app.goalService.Get()); err != nil {
		return app.goalService.Get(), err
	}
	if app.Sessions == nil {
		return app.goalService.Get(), errors.New("session service is unavailable")
	}
	sess, err := app.Sessions.Get(ctx, sessionID)
	if err != nil {
		return app.goalService.Get(), err
	}
	if sess.ParentSessionID != "" {
		return app.goalService.Get(), ErrGoalChildSession
	}
	if strings.TrimSpace(text) == "" {
		return app.goalService.Get(), errors.New("goal text must not be empty")
	}
	initialPrompt, err := agent.GoalInitialPrompt(text)
	if err != nil {
		return app.goalService.Get(), err
	}

	app.goalRuntimeMu.Lock()
	defer app.goalRuntimeMu.Unlock()
	if app.goalRuntimeStopped || app.goalCtx == nil {
		return app.goalService.Get(), ErrGoalRuntimeStopped
	}
	if app.goalDisabled() {
		return app.goalService.Get(), goal.ErrGoalDisabled
	}
	ticket, pending, err := app.goalService.RequestInstallation(app.goalWorkspaceID, sessionID, text)
	if err != nil {
		return pending, err
	}
	app.goalInitialWG.Add(1)
	go app.deliverGoalInitial(app.goalCtx, coordinator, ticket, initialPrompt)
	return pending, nil
}

func (app *App) deliverGoalInitial(ctx context.Context, coordinator agent.Coordinator, ticket goal.Ticket, prompt string) {
	defer app.goalInitialWG.Done()
	ctx = goal.WithCallMarker(ctx, goal.CallMarker{Kind: goal.CallInitial, Ticket: ticket})
	if _, err := coordinator.Run(ctx, ticket.Token.SessionID, prompt); err != nil {
		app.goalService.CancelPending(ticket)
		if !errors.Is(err, context.Canceled) {
			slog.Warn("Failed to deliver initial goal prompt", "session", ticket.Token.SessionID, "error", err)
		}
	}
}

// GetGoal returns the process-wide goal snapshot. Only the owner App checks
// session existence; a confirmed missing owner is removed with a token fence.
func (app *App) GetGoal(ctx context.Context) (goal.Snapshot, error) {
	if app.goalService == nil {
		return goal.Snapshot{}, errors.New("goal service is unavailable")
	}
	snapshot := app.goalService.Get()
	if !snapshot.Active && !snapshot.Pending || snapshot.WorkspaceID != app.goalWorkspaceID {
		return snapshot, nil
	}
	if err := app.reconcileGoalOwner(ctx, snapshot); err != nil {
		return snapshot, err
	}
	return app.goalService.Get(), nil
}

// CancelGoal clears only the installation identified by expected. A pending
// initial prompt is also removed from the ordinary agent queue.
func (app *App) CancelGoal(_ context.Context, expected goal.Token) (goal.Snapshot, error) {
	if app.goalService == nil {
		return goal.Snapshot{}, errors.New("goal service is unavailable")
	}
	before := app.goalService.Get()
	wasPending := before.Pending && before.WorkspaceID == expected.WorkspaceID && before.SessionID == expected.SessionID && before.Revision == expected.Revision
	snapshot, err := app.goalService.Cancel(expected)
	if err != nil {
		return snapshot, err
	}
	if wasPending {
		app.removeGoalInitial(expected)
	}
	return snapshot, nil
}

func (app *App) removeGoalInitial(token goal.Token) {
	coordinator := app.goalCoordinator()
	if queue, ok := coordinator.(agent.GoalInitialQueue); ok {
		queue.RemoveGoalInitial(token)
	}
}

func (app *App) goalCoordinator() agent.Coordinator {
	app.goalRuntimeMu.Lock()
	defer app.goalRuntimeMu.Unlock()
	return app.AgentCoordinator
}

func (app *App) goalAccepting() bool {
	app.goalRuntimeMu.Lock()
	defer app.goalRuntimeMu.Unlock()
	return !app.goalRuntimeStopped
}

func (app *App) goalDisabled() bool {
	if app.config == nil {
		return false
	}
	cfg := app.config.Config()
	return cfg != nil && cfg.Options != nil && slices.Contains(cfg.Options.DisabledTools, completeGoalToolName)
}

func (app *App) cancelDisabledGoal() {
	if app.goalService == nil {
		return
	}
	app.goalRuntimeMu.Lock()
	if app.goalRuntimeStopped || !app.goalDisabled() {
		app.goalRuntimeMu.Unlock()
		return
	}
	snapshot := app.goalService.Get()
	pending := snapshot.Pending && snapshot.WorkspaceID == app.goalWorkspaceID
	if (snapshot.Active || pending) && snapshot.WorkspaceID == app.goalWorkspaceID {
		_, _ = app.goalService.Cancel(goal.Token{
			WorkspaceID: snapshot.WorkspaceID,
			SessionID:   snapshot.SessionID,
			Revision:    snapshot.Revision,
		})
	}
	coordinator := app.AgentCoordinator
	app.goalRuntimeMu.Unlock()
	if pending {
		if queue, ok := coordinator.(agent.GoalInitialQueue); ok {
			queue.RemoveGoalInitial(goal.Token{
				WorkspaceID: snapshot.WorkspaceID,
				SessionID:   snapshot.SessionID,
				Revision:    snapshot.Revision,
			})
		}
	}
}

func (app *App) runGoalWorker(ctx context.Context) {
	defer app.goalWorkerWG.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-app.goalWake:
			app.runGoalFollowup(ctx)
		}
	}
}

func deferGoalWake(coordinator agent.Coordinator, token goal.Token) {
	if deferrer, ok := coordinator.(agent.GoalWakeDeferrer); ok {
		deferrer.DeferGoalWake(token)
	}
}

func (app *App) runGoalFollowup(ctx context.Context) {
	if app.goalService == nil || !app.goalAccepting() {
		return
	}
	snapshot := app.goalService.Get()
	if !snapshot.Active || snapshot.WorkspaceID != app.goalWorkspaceID {
		return
	}
	if app.goalDisabled() {
		app.cancelDisabledGoal()
		return
	}
	coordinator := app.goalCoordinator()
	if coordinator == nil {
		return
	}
	if exists, err := app.goalSessionExists(ctx, snapshot.SessionID); err != nil {
		slog.Warn("Failed to verify goal owner session before follow-up", "session", snapshot.SessionID, "error", err)
		return
	} else if !exists {
		app.cancelMissingGoalOwner(snapshot)
		return
	}
	token := goal.Token{WorkspaceID: snapshot.WorkspaceID, SessionID: snapshot.SessionID, Revision: snapshot.Revision}
	if coordinator.IsSessionBusy(snapshot.SessionID) {
		deferGoalWake(coordinator, token)
		return
	}

	wake, ok := app.goalService.ClaimWake(token)
	if !ok {
		return
	}
	defer app.goalService.ReleaseWake(wake)
	if !app.goalService.WakeCurrent(wake) || !app.goalAccepting() {
		return
	}
	current := app.goalService.Get()
	if !current.Active || current.WorkspaceID != token.WorkspaceID || current.SessionID != token.SessionID || current.Revision != token.Revision {
		return
	}
	if exists, err := app.goalSessionExists(ctx, snapshot.SessionID); err != nil {
		slog.Warn("Failed to re-verify goal owner session before follow-up", "session", snapshot.SessionID, "error", err)
		return
	} else if !exists {
		app.cancelMissingGoalOwner(snapshot)
		return
	}
	if coordinator.IsSessionBusy(snapshot.SessionID) {
		deferGoalWake(coordinator, token)
		return
	}
	prompt, err := agent.GoalFollowupPrompt(current.Text)
	if err != nil {
		slog.Error("Failed to render goal follow-up prompt", "error", err)
		return
	}
	ctx = goal.WithCallMarker(ctx, goal.CallMarker{Kind: goal.CallFollowup, Token: token, Wake: wake})
	ctx = message.WithHiddenUserMessage(ctx)
	if _, err := coordinator.Run(ctx, token.SessionID, prompt); err != nil && !errors.Is(err, context.Canceled) {
		app.goalService.PauseWake(wake)
		slog.Warn("Failed to run goal follow-up", "session", token.SessionID, "error", err)
	}
}

func (app *App) cancelMissingGoalOwner(snapshot goal.Snapshot) {
	_, _ = app.goalService.Cancel(goal.Token{
		WorkspaceID: snapshot.WorkspaceID,
		SessionID:   snapshot.SessionID,
		Revision:    snapshot.Revision,
	})
}

// StopGoalRuntime closes goal admission and cancels goal-owned work without
// cancelling ordinary agent calls. Backend calls this before its first
// CancelAll during workspace teardown.
func (app *App) StopGoalRuntime() {
	app.goalStopOnce.Do(func() {
		app.goalRuntimeMu.Lock()
		app.goalRuntimeStopped = true
		cancel := app.goalCancel
		app.goalRuntimeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if app.goalService != nil {
			snapshot := app.goalService.Get()
			if (snapshot.Active || snapshot.Pending) && snapshot.WorkspaceID == app.goalWorkspaceID {
				_, _ = app.goalService.Cancel(goal.Token{
					WorkspaceID: snapshot.WorkspaceID,
					SessionID:   snapshot.SessionID,
					Revision:    snapshot.Revision,
				})
			}
		}
	})
}

func (app *App) waitGoalRuntime() {
	app.goalInitialWG.Wait()
	app.goalWorkerWG.Wait()
}

func (app *App) stopGoalEventFanIn() {
	app.goalEventStopOnce.Do(func() {
		app.goalRuntimeMu.Lock()
		cancel := app.goalEventCancel
		app.goalRuntimeMu.Unlock()
		if cancel != nil {
			cancel()
		}
		app.goalEventWG.Wait()
	})
}

func (app *App) rollbackGoalRuntime() {
	app.StopGoalRuntime()
	app.waitGoalRuntime()
	app.stopGoalEventFanIn()
	if app.ownsGoalService && app.goalService != nil {
		app.goalService.Shutdown()
	}
}
