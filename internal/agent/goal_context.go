package agent

import "github.com/charmbracelet/crush/internal/goal"

func goalTokenForSnapshot(snapshot goal.Snapshot) goal.Token {
	return goal.Token{
		WorkspaceID: snapshot.WorkspaceID,
		SessionID:   snapshot.SessionID,
		Revision:    snapshot.Revision,
	}
}

func matchesGoalToken(snapshot goal.Snapshot, token goal.Token) bool {
	return snapshot.Active && snapshot.WorkspaceID == token.WorkspaceID &&
		snapshot.SessionID == token.SessionID && snapshot.Revision == token.Revision
}

func goalCallHidden(marker goal.CallMarker, hidden bool) bool {
	switch marker.Kind {
	case goal.CallInitial:
		return false
	case goal.CallFollowup:
		return true
	default:
		return hidden
	}
}
