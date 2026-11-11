package model

import (
	"testing"

	"github.com/charmbracelet/crush/internal/app"
	"github.com/charmbracelet/crush/internal/ui/util"
	"github.com/stretchr/testify/require"
)

func TestDevelopmentUpdateNoticeIsHiddenAndReleaseNoticeRemains(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.Update(app.UpdateAvailableMsg{
		CurrentVersion: "dev",
		LatestVersion:  "2.0.0",
		IsDevelopment:  true,
	})
	require.True(t, ui.status.msg.IsEmpty(), "development builds must not show an update notice")

	ui.Update(app.UpdateAvailableMsg{
		CurrentVersion: "1.0.0",
		LatestVersion:  "2.0.0",
	})
	require.Equal(t, util.InfoTypeUpdate, ui.status.msg.Type)
	require.Equal(t, "Crush update available: v1.0.0 → v2.0.0.", ui.status.msg.Msg)
}
