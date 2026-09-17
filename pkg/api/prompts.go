package api

import "time"

type HeadlessPolicy string

const (
	HeadlessFail HeadlessPolicy = "fail"
	HeadlessWait HeadlessPolicy = "wait"
)

type PromptState string

const (
	PromptPending   PromptState = "pending"
	PromptAnswered  PromptState = "answered"
	PromptCancelled PromptState = "cancelled"
	PromptExpired   PromptState = "expired"
)

// Prompt metadata is reconnectable; answers deliberately never enter this record.
type Prompt struct {
	ID        string      `json:"id"`
	RunID     string      `json:"runId"`
	Task      string      `json:"task"`
	AttemptID string      `json:"attemptId"`
	Kind      string      `json:"kind"`
	Message   string      `json:"message"`
	Choices   []string    `json:"choices,omitempty"`
	Secret    bool        `json:"secret,omitempty"`
	State     PromptState `json:"state"`
	CreatedAt time.Time   `json:"createdAt"`
	Deadline  time.Time   `json:"deadline,omitempty"`
}

// Pointer values distinguish false and empty text from an omitted answer.
type PromptAnswer struct {
	RunID     string  `json:"runId"`
	Task      string  `json:"task"`
	AttemptID string  `json:"attemptId"`
	PromptID  string  `json:"promptId"`
	Confirm   *bool   `json:"confirm,omitempty"`
	Text      *string `json:"text,omitempty"`
	Choice    *int    `json:"choice,omitempty"`
	Cancel    bool    `json:"cancel,omitempty"`
}
