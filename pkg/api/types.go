package api

import "time"

type RunMode string

const (
	ModeDev   RunMode = "dev"
	ModeWatch RunMode = "watch"
	ModeCI    RunMode = "ci"
)

type NodeState string

const (
	StatePending NodeState = "pending"
	StateReady   NodeState = "ready"
	StateRunning NodeState = "running"
	StateCached  NodeState = "cached"
	StateDone    NodeState = "done"
	StateFailed  NodeState = "failed"
	StateStopped NodeState = "stopped"
	StateDirty   NodeState = "dirty"
	StateSkipped NodeState = "skipped"
)

type DBInstance struct {
	Name string `json:"name"`
	URL  string `json:"url,omitempty"`
}

type ProcessRef struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

type Instance struct {
	ID        string                `json:"id"`
	Label     string                `json:"label"`
	Worktree  string                `json:"worktree"`
	CreatedAt time.Time             `json:"createdAt"`
	Ports     map[string]int        `json:"ports"`
	Env       map[string]string     `json:"env"`
	DB        DBInstance            `json:"db"`
	Processes map[string]ProcessRef `json:"processes"`
}

type NodeStatus struct {
	Name       string    `json:"name"`
	Kind       string    `json:"kind"`
	State      NodeState `json:"state"`
	LastRunKey string    `json:"lastRunKey,omitempty"`
	LastError  string    `json:"lastError,omitempty"`
	PID        int       `json:"pid,omitempty"`
	LogPath    string    `json:"logPath,omitempty"`
}

type RunResult struct {
	Target     string   `json:"target"`
	Mode       RunMode  `json:"mode"`
	InstanceID string   `json:"instanceId"`
	Success    bool     `json:"success"`
	DurationMs int64    `json:"durationMs"`
	FailedNode string   `json:"failedNode,omitempty"`
	CacheHits  []string `json:"cacheHits"`
	StartedAt  string   `json:"startedAt"`
	FinishedAt string   `json:"finishedAt"`
}

type StatusResult struct {
	InstanceID string       `json:"instanceId"`
	Target     string       `json:"target"`
	Nodes      []NodeStatus `json:"nodes"`
}

type LogEvent struct {
	TS         string `json:"ts"`
	InstanceID string `json:"instanceId"`
	Task       string `json:"task"`
	Stream     string `json:"stream"`
	Line       string `json:"line"`
}

type InstanceSummary struct {
	ID       string            `json:"id"`
	Label    string            `json:"label"`
	Worktree string            `json:"worktree"`
	Ports    map[string]int    `json:"ports"`
	DB       DBInstance        `json:"db"`
	Target   string            `json:"target,omitempty"`
	States   map[string]string `json:"states,omitempty"`
}

type DoctorResult struct {
	Worktree     string   `json:"worktree"`
	InstanceID   string   `json:"instanceId,omitempty"`
	ChecksPassed bool     `json:"checksPassed"`
	Checks       []string `json:"checks"`
	Warnings     []string `json:"warnings,omitempty"`
}
