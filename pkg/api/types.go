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
	StatePending  NodeState = "pending"
	StateReady    NodeState = "ready"
	StateRunning  NodeState = "running"
	StateCached   NodeState = "cached"
	StateDone     NodeState = "done"
	StateFailed   NodeState = "failed"
	StateCanceled NodeState = "canceled"
	StateStopped  NodeState = "stopped"
	StateDirty    NodeState = "dirty"
	StateSkipped  NodeState = "skipped"
)

type DBInstance struct {
	Name          string `json:"name"`
	URL           string `json:"url,omitempty"`
	Host          string `json:"host,omitempty"`
	Port          int    `json:"port,omitempty"`
	User          string `json:"user,omitempty"`
	Password      string `json:"password,omitempty"`
	Image         string `json:"image,omitempty"`
	ContainerName string `json:"containerName,omitempty"`
	VolumeName    string `json:"volumeName,omitempty"`
	SnapshotRoot  string `json:"snapshotRoot,omitempty"`
}

type ProcessRef struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

type SupervisorRef struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
	LogPath   string    `json:"logPath,omitempty"`
}

type SupervisorStatus struct {
	PID       int       `json:"pid,omitempty"`
	Alive     bool      `json:"alive"`
	StartedAt time.Time `json:"startedAt,omitempty"`
	LogPath   string    `json:"logPath,omitempty"`
}

type RunConfig struct {
	Project     string  `json:"project"`
	Target      string  `json:"target"`
	Mode        RunMode `json:"mode"`
	MaxParallel int     `json:"maxParallel,omitempty"`
	Detached    bool    `json:"detached,omitempty"`
}

type Instance struct {
	ID         string                `json:"id"`
	Label      string                `json:"label"`
	Worktree   string                `json:"worktree"`
	CreatedAt  time.Time             `json:"createdAt"`
	Ports      map[string]int        `json:"ports"`
	Env        map[string]string     `json:"env"`
	DB         DBInstance            `json:"db"`
	Processes  map[string]ProcessRef `json:"processes"`
	Supervisor SupervisorRef         `json:"supervisor,omitempty"`
	LastRun    RunConfig             `json:"lastRun,omitempty"`
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

type FlushRequest struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	SyncPath  string    `json:"syncPath"`
}

type FlushResult struct {
	RequestID  string         `json:"requestId"`
	InstanceID string         `json:"instanceId"`
	Worktree   string         `json:"worktree"`
	Project    string         `json:"project,omitempty"`
	Target     string         `json:"target"`
	Mode       RunMode        `json:"mode"`
	Started    bool           `json:"started"`
	Synced     bool           `json:"synced"`
	Success    bool           `json:"success"`
	TimedOut   bool           `json:"timedOut,omitempty"`
	DurationMs int64          `json:"durationMs"`
	UpdatedAt  time.Time      `json:"updatedAt,omitempty"`
	Nodes      []NodeStatus   `json:"nodes,omitempty"`
	Services   []FlushService `json:"services,omitempty"`
	Issues     []FlushIssue   `json:"issues,omitempty"`
}

type FlushService struct {
	Task    string    `json:"task"`
	State   NodeState `json:"state"`
	PID     int       `json:"pid,omitempty"`
	Alive   bool      `json:"alive"`
	Ready   bool      `json:"ready"`
	Error   string    `json:"error,omitempty"`
	LogPath string    `json:"logPath,omitempty"`
}

type FlushIssue struct {
	Task    string `json:"task,omitempty"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
	LogPath string `json:"logPath,omitempty"`
}

type StatusResult struct {
	InstanceID string            `json:"instanceId"`
	Worktree   string            `json:"worktree,omitempty"`
	Target     string            `json:"target"`
	Mode       RunMode           `json:"mode,omitempty"`
	UpdatedAt  time.Time         `json:"updatedAt,omitempty"`
	Ports      map[string]int    `json:"ports,omitempty"`
	DB         DBInstance        `json:"db,omitempty"`
	URLs       map[string]string `json:"urls,omitempty"`
	Supervisor *SupervisorStatus `json:"supervisor,omitempty"`
	Nodes      []NodeStatus      `json:"nodes"`
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
