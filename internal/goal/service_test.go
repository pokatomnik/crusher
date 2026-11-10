package goal

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServicePendingActivationAndCompletion(t *testing.T) {
	t.Parallel()

	service := NewService()
	ticket, pending, err := service.RequestInstallation("workspace-a", "session-a", "  Write a file\nwith this text  ")
	require.NoError(t, err)
	require.True(t, pending.Pending)
	require.False(t, pending.Active)
	require.Equal(t, "  Write a file\nwith this text  ", pending.Text)

	_, _, err = service.RequestInstallation("workspace-b", "session-b", "second")
	require.ErrorIs(t, err, ErrGoalAlreadyPending)
	require.False(t, service.Get().Active)

	active, ok := service.Activate(ticket)
	require.True(t, ok)
	require.True(t, active.Active)
	require.False(t, active.Pending)
	require.Equal(t, pending.Revision, active.Revision)
	require.Greater(t, active.Version, pending.Version)

	wake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)
	require.True(t, service.ReleaseWake(wake))

	closed, err := service.Complete(ticket.Token, Completion{Result: "done", Success: false})
	require.NoError(t, err)
	require.False(t, closed.Active)
	require.False(t, closed.Pending)
	require.Equal(t, active.Revision, closed.Revision)

	_, err = service.Complete(ticket.Token, Completion{})
	require.ErrorIs(t, err, ErrNoActiveGoal)
}

func TestServiceCancelPendingAndStaleToken(t *testing.T) {
	t.Parallel()

	service := NewService()
	first, pending, err := service.RequestInstallation("w", "s", "one")
	require.NoError(t, err)
	cancelled, err := service.Cancel(first.Token)
	require.NoError(t, err)
	require.False(t, cancelled.Pending)
	require.Greater(t, cancelled.Version, pending.Version)

	second, _, err := service.RequestInstallation("w", "s", "two")
	require.NoError(t, err)
	require.NotEqual(t, first.Token.Revision, second.Token.Revision)
	_, err = service.Cancel(first.Token)
	require.ErrorIs(t, err, ErrGoalConflict)
	require.True(t, service.Get().Pending)

	empty, err := service.Cancel(first.Token)
	require.ErrorIs(t, err, ErrGoalConflict)
	require.True(t, empty.Pending)
}

func TestServiceCancelEmptyIsNoop(t *testing.T) {
	t.Parallel()

	service := NewService()
	before := service.Get()
	after, err := service.Cancel(Token{WorkspaceID: "stale", SessionID: "stale", Revision: 99})
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestServicePauseAndWakeFencing(t *testing.T) {
	t.Parallel()

	service := NewService()
	ticket, _, err := service.RequestInstallation("w", "s", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)

	wake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)
	require.True(t, service.Pause(ticket.Token))
	require.False(t, service.WakeCurrent(wake))
	require.True(t, service.IsPaused(ticket.Token))
	_, claimed := service.ClaimWake(ticket.Token)
	require.False(t, claimed)

	require.True(t, service.ResumeOnMainRun("w", "s"))
	require.False(t, service.IsPaused(ticket.Token))
	resumedWake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)
	require.False(t, service.PauseWake(wake), "a stale wake error must not pause the resumed goal")
	require.False(t, service.IsPaused(ticket.Token))
	require.True(t, service.ResumeOnMainRun("w", "s"), "even an already-running goal gives precedence to a newly admitted main run")
	require.False(t, service.PauseWake(resumedWake), "a late error must not pause a goal superseded by a newer run")
	require.False(t, service.IsPaused(ticket.Token))
	currentWake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)
	require.True(t, service.PauseWake(currentWake), "a current preflight failure must pause its own wake")
	require.True(t, service.IsPaused(ticket.Token))
}

func TestServiceConcurrentReservationHasOneWinner(t *testing.T) {
	t.Parallel()

	service := NewService()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var accepted int
	var acceptedMu sync.Mutex
	for i := range 16 {
		wg.Go(func() {
			<-start
			_, _, err := service.RequestInstallation("w", string(rune('a'+i)), "goal")
			if err == nil {
				acceptedMu.Lock()
				accepted++
				acceptedMu.Unlock()
			}
		})
	}
	close(start)
	wg.Wait()
	require.Equal(t, 1, accepted)
	require.True(t, service.Get().Pending)
}

func TestServiceRejectsBlankAndMismatchedOwner(t *testing.T) {
	t.Parallel()

	service := NewService()
	_, _, err := service.RequestInstallation("w", "s", " \n\t ")
	require.Error(t, err)

	ticket, _, err := service.RequestInstallation("w", "s", "goal")
	require.NoError(t, err)
	_, changed := service.Forget("another-workspace", "s")
	require.False(t, changed)
	require.True(t, service.IsPending(ticket))
	_, changed = service.Forget("w", "s")
	require.True(t, changed)
	require.False(t, service.Get().Pending)
}
