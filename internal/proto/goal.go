package proto

// GoalState is the wire snapshot of the backend's single in-memory goal slot.
type GoalState struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Revision    uint64 `json:"revision"`
	Version     uint64 `json:"version"`
	Text        string `json:"text,omitempty"`
	Active      bool   `json:"active"`
	Pending     bool   `json:"pending"`
}

// SetGoalRequest asks the backend to enqueue an initial goal prompt.
type SetGoalRequest struct {
	Text string `json:"text"`
}

// CancelGoalRequest fences a cancellation to the logical goal selected by
// the client.
type CancelGoalRequest struct {
	ExpectedToken GoalToken `json:"expected_token"`
}

// GoalToken identifies one goal installation.
type GoalToken struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	Revision    uint64 `json:"revision"`
}

// GoalError carries a machine-readable error code for goal endpoints.
type GoalError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
