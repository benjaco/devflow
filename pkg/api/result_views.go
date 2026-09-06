package api

import "time"

// ExecutionView is a bounded presentation of a snapshot, never the stored evidence.
// Status inspection has no success field: a readable snapshot does not prove health.
type ExecutionView struct {
	Details           string                  `json:"details"`
	RunID             string                  `json:"runId,omitempty"`
	InstanceID        string                  `json:"instanceId"`
	Worktree          string                  `json:"worktree,omitempty"`
	Target            string                  `json:"target"`
	Mode              RunMode                 `json:"mode,omitempty"`
	Success           *bool                   `json:"success,omitempty"`
	Error             *CommandError           `json:"error,omitempty"`
	ResourceConflict  *ResourceConflict       `json:"resourceConflict,omitempty"`
	FailedNode        string                  `json:"failedNode,omitempty"`
	RequestID         string                  `json:"requestId,omitempty"`
	Synced            *bool                   `json:"synced,omitempty"`
	Started           *bool                   `json:"started,omitempty"`
	TimedOut          bool                    `json:"timedOut,omitempty"`
	DurationMs        int64                   `json:"durationMs,omitempty"`
	StartedAt         string                  `json:"startedAt,omitempty"`
	FinishedAt        string                  `json:"finishedAt,omitempty"`
	UpdatedAt         time.Time               `json:"updatedAt,omitempty"`
	Daemon            *DaemonStatus           `json:"daemon,omitempty"`
	RepositoryChanges *RepositoryChangeResult `json:"repositoryChanges,omitempty"`
	Counts            ExecutionCounts         `json:"counts"`
	Nodes             []NodeStatus            `json:"nodes"`
	FailureExcerpts   []FailureExcerpt        `json:"failureExcerpts,omitempty"`
	Issues            []FlushIssue            `json:"issues"`
	PendingPrompts    []Prompt                `json:"pendingPrompts"`
	Truncated         ExecutionTruncation     `json:"truncated"`
	Evidence          ExecutionEvidence       `json:"evidence"`
}

type ExecutionCounts struct {
	Nodes           int               `json:"nodes"`
	NodeStates      map[NodeState]int `json:"nodeStates"`
	ProblemNodes    int               `json:"problemNodes"`
	CacheHits       int               `json:"cacheHits"`
	CacheMisses     int               `json:"cacheMisses"`
	Services        int               `json:"services"`
	UnreadyServices int               `json:"unreadyServices"`
	Issues          int               `json:"issues"`
	PendingPrompts  int               `json:"pendingPrompts"`
	FailureExcerpts int               `json:"failureExcerpts"`
}

type ExecutionTruncation struct {
	Nodes           bool `json:"nodes"`
	Issues          bool `json:"issues"`
	PendingPrompts  bool `json:"pendingPrompts"`
	Text            bool `json:"text"`
	FailureExcerpts bool `json:"failureExcerpts"`
}

type ExecutionEvidence struct {
	Run     []string `json:"run,omitempty"`
	Status  []string `json:"status,omitempty"`
	Prompts []string `json:"prompts,omitempty"`
}
