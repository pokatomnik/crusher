package backend

import (
	"context"
	"errors"

	"github.com/charmbracelet/crush/internal/goal"
)

var ErrGoalServiceUnavailable = errors.New("goal service is unavailable")

// GetGoal returns the single backend-wide goal snapshot, validating it
// through the App that owns the current session.
func (b *Backend) GetGoal(ctx context.Context) (goal.Snapshot, error) {
	if b.goalService == nil {
		return goal.Snapshot{}, ErrGoalServiceUnavailable
	}
	snapshot := b.goalService.Get()
	if !goalSnapshotHasOwner(snapshot) {
		return snapshot, nil
	}
	owner, err := b.GetWorkspace(snapshot.WorkspaceID)
	if errors.Is(err, ErrWorkspaceNotFound) {
		b.clearGoalIfCurrent(snapshot)
		return b.goalService.Get(), nil
	}
	if err != nil {
		return snapshot, err
	}
	if owner.App == nil {
		return snapshot, ErrWorkspaceNotFound
	}
	return owner.App.GetGoal(ctx)
}

// SetGoal validates the requested workspace and routes stale-owner validation
// through the current owner's App before asking the target App to reserve the
// singleton slot.
func (b *Backend) SetGoal(ctx context.Context, workspaceID, sessionID, text string) (goal.Snapshot, error) {
	if b.goalService == nil {
		return goal.Snapshot{}, ErrGoalServiceUnavailable
	}
	workspace, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return b.goalService.Get(), err
	}
	workspace.runMu.Lock()
	closing := workspace.closing
	workspace.runMu.Unlock()
	if closing {
		return b.goalService.Get(), ErrWorkspaceClosing
	}
	if workspace.App == nil {
		return b.goalService.Get(), ErrWorkspaceNotFound
	}

	current := b.goalService.Get()
	if goalSnapshotHasOwner(current) && current.WorkspaceID != workspaceID {
		owner, ownerErr := b.GetWorkspace(current.WorkspaceID)
		if errors.Is(ownerErr, ErrWorkspaceNotFound) {
			b.clearGoalIfCurrent(current)
		} else if ownerErr != nil {
			return b.goalService.Get(), ownerErr
		} else if owner.App == nil {
			return b.goalService.Get(), ErrWorkspaceNotFound
		} else if _, ownerErr = owner.App.GetGoal(ctx); ownerErr != nil {
			return b.goalService.Get(), ownerErr
		}
	}
	return workspace.App.SetGoal(ctx, sessionID, text)
}

// CancelGoal routes cancellation through the owner App when the expected token
// is current. A stale token is checked directly by the shared service, and a
// missing owner workspace is treated as already torn down.
func (b *Backend) CancelGoal(ctx context.Context, expected goal.Token) (goal.Snapshot, error) {
	if b.goalService == nil {
		return goal.Snapshot{}, ErrGoalServiceUnavailable
	}
	current := b.goalService.Get()
	if !goalSnapshotHasOwner(current) || !goalSnapshotMatchesToken(current, expected) {
		return b.goalService.Cancel(expected)
	}
	owner, err := b.GetWorkspace(current.WorkspaceID)
	if errors.Is(err, ErrWorkspaceNotFound) {
		b.clearGoalIfCurrent(current)
		return b.goalService.Get(), nil
	}
	if err != nil {
		return current, err
	}
	if owner.App == nil {
		return current, ErrWorkspaceNotFound
	}
	return owner.App.CancelGoal(ctx, expected)
}

func (b *Backend) clearGoalIfCurrent(snapshot goal.Snapshot) {
	if b.goalService == nil || !goalSnapshotHasOwner(snapshot) {
		return
	}
	b.goalService.Cancel(goal.Token{
		WorkspaceID: snapshot.WorkspaceID,
		SessionID:   snapshot.SessionID,
		Revision:    snapshot.Revision,
	})
}

func goalSnapshotHasOwner(snapshot goal.Snapshot) bool {
	return snapshot.Active || snapshot.Pending
}

func goalSnapshotMatchesToken(snapshot goal.Snapshot, token goal.Token) bool {
	return snapshot.WorkspaceID == token.WorkspaceID &&
		snapshot.SessionID == token.SessionID &&
		snapshot.Revision == token.Revision
}
