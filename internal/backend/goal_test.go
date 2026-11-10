package backend

import (
	"context"
	"testing"

	"github.com/charmbracelet/crush/internal/goal"
	"github.com/stretchr/testify/require"
)

func TestBackendGoalOperationsRequireWorkspaceAndPreserveEmptySnapshot(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	b.goalService = goal.NewService()
	t.Cleanup(b.goalService.Shutdown)

	snapshot, err := b.GetGoal(t.Context())
	require.NoError(t, err)
	require.Empty(t, snapshot.WorkspaceID)

	_, err = b.SetGoal(t.Context(), "missing", "session", "goal")
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
	require.False(t, b.goalService.Get().Pending)

	snapshot, err = b.CancelGoal(t.Context(), goal.Token{WorkspaceID: "stale", SessionID: "session", Revision: 1})
	require.NoError(t, err)
	require.Empty(t, snapshot.WorkspaceID)
}

func TestBackendGoalGetClearsOwnerWhoseWorkspaceWasRemoved(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	b.goalService = goal.NewService()
	t.Cleanup(b.goalService.Shutdown)
	ticket, _, err := b.goalService.RequestInstallation("removed-workspace", "session", "goal")
	require.NoError(t, err)
	_, ok := b.goalService.Activate(ticket)
	require.True(t, ok)

	snapshot, err := b.GetGoal(t.Context())
	require.NoError(t, err)
	require.False(t, snapshot.Active)
	require.False(t, snapshot.Pending)
	require.Empty(t, snapshot.WorkspaceID)
}

func TestBackendGoalCancelClearsRemovedOwnerAndReturnsSnapshot(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	b.goalService = goal.NewService()
	t.Cleanup(b.goalService.Shutdown)
	ticket, _, err := b.goalService.RequestInstallation("removed-workspace", "session", "goal")
	require.NoError(t, err)
	_, ok := b.goalService.Activate(ticket)
	require.True(t, ok)

	snapshot, err := b.CancelGoal(context.Background(), ticket.Token)
	require.NoError(t, err)
	require.False(t, snapshot.Active)
	require.False(t, snapshot.Pending)
	require.Empty(t, snapshot.WorkspaceID)
}

func TestBackendShutdownClosesSharedGoalServiceAfterWorkspaceShutdown(t *testing.T) {
	t.Parallel()

	b, _ := newTestBackend(t)
	b.goalService = goal.NewService()
	ws, _ := insertTestWorkspace(t, b, "/tmp/goal-shutdown")
	started := make(chan struct{})
	release := make(chan struct{})
	SetWorkspaceShutdownFnForTest(ws, func() {
		close(started)
		<-release
	})

	done := make(chan error, 1)
	go func() { done <- b.ShutdownWorkspaces(context.Background()) }()
	<-started
	secondDone := make(chan error, 1)
	go func() { secondDone <- b.ShutdownWorkspaces(context.Background()) }()
	require.NoError(t, <-secondDone)
	select {
	case <-b.goalService.Subscribe(t.Context()):
		t.Fatal("shared goal service closed before workspace shutdown completed")
	default:
	}
	close(release)
	require.NoError(t, <-done)

	events := b.goalService.Subscribe(t.Context())
	select {
	case _, ok := <-events:
		require.False(t, ok)
	default:
		t.Fatal("shared goal service was not closed after workspace shutdown")
	}
}
