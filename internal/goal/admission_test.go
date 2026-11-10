package goal

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAdmitWakeRejectsRevokedToken(t *testing.T) {
	t.Parallel()

	service := NewService()
	ticket, _, err := service.RequestInstallation("workspace", "session", "goal")
	require.NoError(t, err)
	_, ok := service.Activate(ticket)
	require.True(t, ok)
	wake, ok := service.ClaimWake(ticket.Token)
	require.True(t, ok)

	admitted := false
	require.True(t, service.AdmitWake(wake, func() bool {
		admitted = true
		return true
	}))
	require.True(t, admitted)

	_, err = service.Cancel(ticket.Token)
	require.NoError(t, err)
	admitted = false
	require.False(t, service.AdmitWake(wake, func() bool {
		admitted = true
		return true
	}))
	require.False(t, admitted, "a revoked wake must not register new work")
}
