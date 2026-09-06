package api

// CommandError identifies an actionable failure without requiring prose parsing.
// It is shared by CLI results and the daemon transport.
type CommandError struct {
	Code    string `json:"code"`
	Phase   string `json:"phase"`
	Message string `json:"message"`
}

func (e *CommandError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}
