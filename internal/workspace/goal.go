package workspace

import (
	"context"
	"errors"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/proto"
	"github.com/charmbracelet/crush/internal/pubsub"
)

// GoalAccess is the narrow optional capability for managing the global goal
// slot. Workspace remains focused on the broader operations shared by all
// frontends.
type GoalAccess interface {
	SetGoal(context.Context, string, string) (goal.Snapshot, error)
	GetGoal(context.Context) (goal.Snapshot, error)
	CancelGoal(context.Context, goal.Token) (goal.Snapshot, error)
}

// ErrGoalUnsupported reports that a workspace's backing app does not expose
// goal management.
var (
	ErrGoalUnsupported      = errors.New("goal management is not supported by this workspace")
	ErrGoalWorkspaceChanged = errors.New("workspace runtime changed while the goal request was in flight")
)

// SetGoal reserves a goal through the local app.
func (w *AppWorkspace) SetGoal(ctx context.Context, sessionID, text string) (goal.Snapshot, error) {
	return w.app.SetGoal(ctx, sessionID, text)
}

// GetGoal retrieves the app's global goal snapshot.
func (w *AppWorkspace) GetGoal(ctx context.Context) (goal.Snapshot, error) {
	return w.app.GetGoal(ctx)
}

// CancelGoal cancels the local app's goal if expected still identifies it.
func (w *AppWorkspace) CancelGoal(ctx context.Context, expected goal.Token) (goal.Snapshot, error) {
	return w.app.CancelGoal(ctx, expected)
}

// SetGoal reserves a goal on the remote backend for the requested session.
func (w *ClientWorkspace) SetGoal(ctx context.Context, sessionID, text string) (goal.Snapshot, error) {
	workspaceID, generation := w.goalRuntime()
	state, err := w.client.SetGoal(ctx, workspaceID, sessionID, text)
	if err != nil {
		return goal.Snapshot{}, err
	}
	snapshot := goalStateToSnapshot(state)
	if !w.cacheGoalSnapshot(generation, snapshot) {
		return goal.Snapshot{}, ErrGoalWorkspaceChanged
	}
	return snapshot, nil
}

// GetGoal retrieves the remote backend's global goal snapshot.
func (w *ClientWorkspace) GetGoal(ctx context.Context) (goal.Snapshot, error) {
	workspaceID, generation := w.goalRuntime()
	state, err := w.client.GetGoal(ctx, workspaceID)
	if err != nil {
		return goal.Snapshot{}, err
	}
	snapshot := goalStateToSnapshot(state)
	if !w.cacheGoalSnapshot(generation, snapshot) {
		return goal.Snapshot{}, ErrGoalWorkspaceChanged
	}
	return snapshot, nil
}

// CancelGoal cancels the remote goal only if expected still identifies it.
func (w *ClientWorkspace) CancelGoal(ctx context.Context, expected goal.Token) (goal.Snapshot, error) {
	workspaceID, generation := w.goalRuntime()
	state, err := w.client.CancelGoal(ctx, workspaceID, expected)
	if err != nil {
		return goal.Snapshot{}, err
	}
	snapshot := goalStateToSnapshot(state)
	if !w.cacheGoalSnapshot(generation, snapshot) {
		return goal.Snapshot{}, ErrGoalWorkspaceChanged
	}
	return snapshot, nil
}

func goalStateToSnapshot(state proto.GoalState) goal.Snapshot {
	return goal.Snapshot{
		WorkspaceID: state.WorkspaceID,
		SessionID:   state.SessionID,
		Revision:    state.Revision,
		Version:     state.Version,
		Text:        state.Text,
		Active:      state.Active,
		Pending:     state.Pending,
	}
}

func (w *ClientWorkspace) goalRuntime() (string, uint64) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ws.ID, w.goalRuntimeGeneration
}

func (w *ClientWorkspace) cacheGoalSnapshot(generation uint64, snapshot goal.Snapshot) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if generation != w.goalRuntimeGeneration {
		return false
	}
	if snapshot.Version >= w.goalSnapshot.Version {
		w.goalSnapshot = snapshot
	}
	return true
}

func (w *ClientWorkspace) cachedGoalSnapshot() goal.Snapshot {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.goalSnapshot
}

const goalResyncTimeout = 5 * time.Second

// resyncGoal fetches the global snapshot after an event stream attaches. The
// snapshot is sent through the same event channel as local Goal changes.
func (w *ClientWorkspace) resyncGoal(send func(tea.Msg)) {
	ctx, cancel := context.WithTimeout(w.subCtx, goalResyncTimeout)
	defer cancel()

	snapshot, err := w.GetGoal(ctx)
	if err != nil {
		slog.Debug("Failed to resync goal after event subscription", "error", err)
		return
	}
	if send != nil {
		send(pubsub.Event[goal.Snapshot]{Type: pubsub.UpdatedEvent, Payload: snapshot})
	}
}

// notifyGoalAvailability makes the UI re-evaluate goal command availability
// after the cached workspace configuration changes.
func (w *ClientWorkspace) notifyGoalAvailability(send func(tea.Msg)) {
	if send == nil {
		return
	}
	send(pubsub.Event[goal.Snapshot]{
		Type:    pubsub.UpdatedEvent,
		Payload: w.cachedGoalSnapshot(),
	})
}

var (
	_ GoalAccess = (*AppWorkspace)(nil)
	_ GoalAccess = (*ClientWorkspace)(nil)
)
