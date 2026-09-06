package api

import "time"

type RunState string

const (
	RunQueued    RunState = "queued"
	RunRunning   RunState = "running"
	RunWaiting   RunState = "waiting"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunCanceled  RunState = "canceled"
)

func (s RunState) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunCanceled
}

// RunRecord preserves what this execution observed, not evidence about later edits.
type RunRecord struct {
	RunID          string        `json:"runId"`
	InstanceID     string        `json:"instanceId"`
	Project        string        `json:"project"`
	Target         string        `json:"target"`
	Mode           RunMode       `json:"mode"`
	OwnerPID       int           `json:"ownerPid,omitempty"`
	State          RunState      `json:"state"`
	CreatedAt      time.Time     `json:"createdAt"`
	StartedAt      time.Time     `json:"startedAt,omitempty"`
	FinishedAt     time.Time     `json:"finishedAt,omitempty"`
	UpdatedAt      time.Time     `json:"updatedAt"`
	Deadline       time.Time     `json:"deadline,omitempty"`
	GraphDigest    string        `json:"graphDigest,omitempty"`
	AdapterVersion string        `json:"adapterVersion,omitempty"`
	Attempts       []TaskAttempt `json:"attempts"`
	Result         *RunResult    `json:"result,omitempty"`
}

type TaskAttempt struct {
	AttemptID       string           `json:"attemptId"`
	Task            string           `json:"task"`
	State           NodeState        `json:"state"`
	Executed        bool             `json:"executed"`
	CacheKey        string           `json:"cacheKey,omitempty"`
	LogPath         string           `json:"logPath"`
	StartedAt       time.Time        `json:"startedAt"`
	FinishedAt      time.Time        `json:"finishedAt,omitempty"`
	LastError       string           `json:"lastError,omitempty"`
	FailureExcerpts []FailureExcerpt `json:"failureExcerpts,omitempty"`
}
