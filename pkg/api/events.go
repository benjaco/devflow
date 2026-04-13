package api

type EventType string

const (
	EventRunStarted      EventType = "run_started"
	EventRunFinished     EventType = "run_finished"
	EventWatchCycleStart EventType = "watch_cycle_started"
	EventWatchCycleDone  EventType = "watch_cycle_finished"
	EventInstanceUpdated EventType = "instance_updated"
	EventTaskState       EventType = "task_state_changed"
	EventLogLine         EventType = "log_line"
	EventCacheHit        EventType = "cache_hit"
	EventCacheMiss       EventType = "cache_miss"
	EventProcessExited   EventType = "process_exited"
	EventInteractionReq  EventType = "interaction_requested"
	EventInteractionAck  EventType = "interaction_answered"
	EventInteractionStop EventType = "interaction_cancelled"
)

type Event struct {
	TS            string    `json:"ts"`
	Type          EventType `json:"type"`
	InstanceID    string    `json:"instanceId,omitempty"`
	Worktree      string    `json:"worktree,omitempty"`
	Target        string    `json:"target,omitempty"`
	Task          string    `json:"task,omitempty"`
	Mode          RunMode   `json:"mode,omitempty"`
	State         NodeState `json:"state,omitempty"`
	PreviousState NodeState `json:"previousState,omitempty"`
	Stream        string    `json:"stream,omitempty"`
	Line          string    `json:"line,omitempty"`
	CacheKey      string    `json:"cacheKey,omitempty"`
	PID           int       `json:"pid,omitempty"`
	Files         []string  `json:"files,omitempty"`
	AffectedTasks []string  `json:"affectedTasks,omitempty"`
	PromptID      string    `json:"promptId,omitempty"`
	PromptKind    string    `json:"promptKind,omitempty"`
	Prompt        string    `json:"prompt,omitempty"`
	Error         string    `json:"error,omitempty"`
	Success       *bool     `json:"success,omitempty"`
}
