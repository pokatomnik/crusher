package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/charmbracelet/crush/internal/goal"
	"github.com/charmbracelet/crush/internal/session"
)

type goalSessionService struct {
	session.Service
	goals       *goal.Service
	workspaceID string
}

func newGoalSessionService(sessions session.Service, goals *goal.Service, workspaceID string) session.Service {
	return &goalSessionService{Service: sessions, goals: goals, workspaceID: workspaceID}
}

func (s *goalSessionService) Delete(ctx context.Context, sessionID string) error {
	if err := s.Service.Delete(ctx, sessionID); err != nil {
		return err
	}
	if s.goals != nil {
		s.goals.Forget(s.workspaceID, sessionID)
	}
	return nil
}

func (app *App) goalSessionExists(ctx context.Context, sessionID string) (bool, error) {
	if app.Sessions == nil {
		return false, errors.New("session service is unavailable")
	}
	_, err := app.Sessions.Get(ctx, sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// reconcileGoalOwner removes only the still-current goal revision after the
// owner's session has been confirmed absent. Transient database errors retain
// the slot so an unrelated caller cannot replace it.
func (app *App) reconcileGoalOwner(ctx context.Context, snapshot goal.Snapshot) error {
	if (!snapshot.Active && !snapshot.Pending) || snapshot.WorkspaceID != app.goalWorkspaceID {
		return nil
	}
	exists, err := app.goalSessionExists(ctx, snapshot.SessionID)
	if err != nil {
		return fmt.Errorf("failed to verify goal owner session: %w", err)
	}
	if !exists {
		app.cancelMissingGoalOwner(snapshot)
	}
	return nil
}
