package api

import "time"

type RunMode string

const (
	ModeDev        RunMode = "dev"
	ModeWatch      RunMode = "watch"
	ModeCI         RunMode = "ci"
	ModeValidation RunMode = "validation"
)

type ValidationMode string

const (
	ValidationModeAll       ValidationMode = "all"
	ValidationModeArtifacts ValidationMode = "artifacts"
	ValidationModeOrders    ValidationMode = "orders"
)

type ValidationDetails string

const (
	ValidationDetailsSummary ValidationDetails = "summary"
	ValidationDetailsIssues  ValidationDetails = "issues"
	ValidationDetailsFull    ValidationDetails = "full"
)

type ValidationIssueSeverity string

const (
	ValidationIssueError   ValidationIssueSeverity = "error"
	ValidationIssueWarning ValidationIssueSeverity = "warning"
)

type NodeState string

const (
	StatePending         NodeState = "pending"
	StateReady           NodeState = "ready"
	StateRunning         NodeState = "running"
	StateCached          NodeState = "cached"
	StateDone            NodeState = "done"
	StateFailed          NodeState = "failed"
	StateMigrationNeeded NodeState = "migration_needed"
	StateCanceled        NodeState = "canceled"
	StateStopped         NodeState = "stopped"
	StateDirty           NodeState = "dirty"
	StateSkipped         NodeState = "skipped"
)

type DBInstance struct {
	Name            string `json:"name"`
	URL             string `json:"url,omitempty"`
	Host            string `json:"host,omitempty"`
	Port            int    `json:"port,omitempty"`
	ContainerPort   int    `json:"containerPort,omitempty"`
	User            string `json:"user,omitempty"`
	Password        string `json:"password,omitempty"`
	Flavor          string `json:"flavor,omitempty"`
	PostgresVersion int    `json:"postgresVersion,omitempty"`
	Image           string `json:"image,omitempty"`
	SidecarImage    string `json:"sidecarImage,omitempty"`
	ContainerName   string `json:"containerName,omitempty"`
	VolumeName      string `json:"volumeName,omitempty"`
	SnapshotRoot    string `json:"snapshotRoot,omitempty"`
}

type ProcessRef struct {
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
}

type SupervisorRef struct {
	PID       int       `json:"pid"`
	ExecPID   int       `json:"execPid,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	LogPath   string    `json:"logPath,omitempty"`
}

type SupervisorStatus struct {
	PID       int       `json:"pid,omitempty"`
	ExecPID   int       `json:"execPid,omitempty"`
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
	Name       string       `json:"name"`
	Kind       string       `json:"kind"`
	State      NodeState    `json:"state"`
	DurationMs int64        `json:"durationMs"`
	LastRunKey string       `json:"lastRunKey,omitempty"`
	LastError  string       `json:"lastError,omitempty"`
	PID        int          `json:"pid,omitempty"`
	LogPath    string       `json:"logPath,omitempty"`
	Cache      *CacheTiming `json:"cache,omitempty"`
	Debug      *DebugStatus `json:"debug,omitempty"`
}

type CacheTiming struct {
	Outcome                        string   `json:"outcome"`
	KeyDurationMs                  int64    `json:"keyDurationMs"`
	ReadDurationMs                 int64    `json:"readDurationMs"`
	WriteDurationMs                int64    `json:"writeDurationMs,omitempty"`
	ManifestValidationMs           int64    `json:"manifestValidationMs,omitempty"`
	ManifestComponents             []string `json:"manifestComponents,omitempty"`
	LocalInputsChangedFromManifest bool     `json:"localInputsChangedFromManifest,omitempty"`
	TotalDurationMs                int64    `json:"totalDurationMs"`
}

type CacheKeyResult struct {
	Project      string         `json:"project"`
	Target       string         `json:"target"`
	InstanceID   string         `json:"instanceId"`
	Namespace    string         `json:"namespace"`
	Key          string         `json:"key"`
	TaskKeys     []TaskCacheKey `json:"taskKeys"`
	ManifestPath string         `json:"manifestPath,omitempty"`
}

type TaskCacheKey struct {
	Task  string `json:"task"`
	Key   string `json:"key"`
	Stamp bool   `json:"stamp,omitempty"`
}

type CacheKeyManifest struct {
	SchemaVersion       int                    `json:"schemaVersion"`
	Compatibility       string                 `json:"compatibility"`
	Project             string                 `json:"project"`
	Namespace           string                 `json:"namespace"`
	InstanceID          string                 `json:"instanceId"`
	WorktreeDigest      string                 `json:"worktreeDigest"`
	Target              string                 `json:"target"`
	GraphDigest         string                 `json:"graphDigest"`
	ConfigurationDigest string                 `json:"configurationDigest"`
	EnvironmentHashes   map[string]string      `json:"environmentHashes"`
	Tasks               []CacheKeyManifestTask `json:"tasks"`
	AggregateKey        string                 `json:"aggregateKey"`
	CreatedAt           string                 `json:"createdAt"`
	ExpiresAt           string                 `json:"expiresAt"`
	Integrity           string                 `json:"integrity"`
}

type CacheKeyManifestTask struct {
	Task               string                      `json:"task"`
	Cache              bool                        `json:"cache"`
	Stamp              bool                        `json:"stamp"`
	TaskSignature      string                      `json:"taskSignature"`
	StaticInputDigest  string                      `json:"staticInputDigest,omitempty"`
	SemanticComponents []CacheKeyManifestComponent `json:"semanticComponents"`
	PreflightKey       string                      `json:"preflightKey,omitempty"`
}

type CacheKeyManifestComponent struct {
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

type CacheKeyManifestUsage struct {
	Path                   string   `json:"path,omitempty"`
	Validated              bool     `json:"validated"`
	Error                  string   `json:"error,omitempty"`
	ValidationDurationMs   int64    `json:"validationDurationMs"`
	ReusedTasks            []string `json:"reusedTasks"`
	ReusedComponents       int      `json:"reusedComponents"`
	LocalInputChangedTasks []string `json:"localInputChangedTasks"`
}

type CachePathResult struct {
	Project       string `json:"project"`
	Namespace     string `json:"namespace"`
	CacheRoot     string `json:"cacheRoot"`
	NamespacePath string `json:"namespacePath"`
}

type DebugStatus struct {
	Type     string            `json:"type,omitempty"`
	Host     string            `json:"host,omitempty"`
	Port     int               `json:"port,omitempty"`
	PortName string            `json:"portName,omitempty"`
	Protocol string            `json:"protocol,omitempty"`
	Binary   string            `json:"binary,omitempty"`
	Package  string            `json:"package,omitempty"`
	Attach   DebugAttachConfig `json:"attach,omitempty"`
}

type DebugAttachConfig struct {
	Name         string `json:"name,omitempty"`
	Type         string `json:"type,omitempty"`
	Request      string `json:"request,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Host         string `json:"host,omitempty"`
	Port         int    `json:"port,omitempty"`
	DebugAdapter string `json:"debugAdapter,omitempty"`
	CWD          string `json:"cwd,omitempty"`
}

type RunResult struct {
	Target            string                 `json:"target"`
	Mode              RunMode                `json:"mode"`
	InstanceID        string                 `json:"instanceId"`
	Success           bool                   `json:"success"`
	DurationMs        int64                  `json:"durationMs"`
	Error             string                 `json:"error,omitempty"`
	FailedNode        string                 `json:"failedNode,omitempty"`
	FailedNodeLogPath string                 `json:"failedNodeLogPath,omitempty"`
	LogTail           []string               `json:"logTail,omitempty"`
	FailureExcerpts   []FailureExcerpt       `json:"failureExcerpts"`
	CacheKeyManifest  *CacheKeyManifestUsage `json:"cacheKeyManifest,omitempty"`
	Nodes             []NodeStatus           `json:"nodes"`
	CacheHits         []string               `json:"cacheHits"`
	CacheMisses       []string               `json:"cacheMisses"`
	StartedAt         string                 `json:"startedAt"`
	FinishedAt        string                 `json:"finishedAt"`
}

type FailureExcerpt struct {
	Node      string   `json:"node"`
	LogPath   string   `json:"logPath"`
	Reason    string   `json:"reason"`
	StartLine int      `json:"startLine"`
	EndLine   int      `json:"endLine"`
	Lines     []string `json:"lines"`
}

type ValidationResult struct {
	Project         string                     `json:"project"`
	Target          string                     `json:"target"`
	Worktree        string                     `json:"worktree"`
	Mode            ValidationMode             `json:"mode"`
	Details         ValidationDetails          `json:"details"`
	MaxListedPaths  int                        `json:"maxListedPaths"`
	Success         bool                       `json:"success"`
	DurationMs      int64                      `json:"durationMs"`
	IssueCount      int                        `json:"issueCount"`
	Artifacts       *ArtifactValidationResult  `json:"artifacts,omitempty"`
	Orders          *OrderValidationResult     `json:"orders,omitempty"`
	Issues          []ValidationIssue          `json:"issues,omitempty"`
	Metrics         ValidationResourceMetrics  `json:"metrics"`
	ResourceFailure *ValidationResourceFailure `json:"resourceFailure,omitempty"`
}

type ValidationResourceMetrics struct {
	TotalFilesProcessed        int64                   `json:"totalFilesProcessed"`
	TotalLogicalBytesProcessed int64                   `json:"totalLogicalBytesProcessed"`
	TemporaryBytesCurrent      int64                   `json:"temporaryBytesCurrent"`
	TemporaryBytesPeak         int64                   `json:"temporaryBytesPeak"`
	TemporaryPhysicalCurrent   int64                   `json:"temporaryPhysicalBytesCurrent"`
	TemporaryPhysicalPeak      int64                   `json:"temporaryPhysicalBytesPeak"`
	TemporaryPhysicalMeasured  bool                    `json:"temporaryPhysicalBytesMeasured"`
	MaxFiles                   int64                   `json:"maxFiles"`
	MaxLogicalBytes            int64                   `json:"maxLogicalBytes"`
	MaxTemporaryBytes          int64                   `json:"maxTemporaryBytes"`
	RemainingFiles             int64                   `json:"remainingFiles"`
	RemainingLogicalBytes      int64                   `json:"remainingLogicalBytes"`
	RemainingTemporaryBytes    int64                   `json:"remainingTemporaryBytes"`
	DiskSafetyReserveBytes     int64                   `json:"diskSafetyReserveBytes"`
	Phases                     []ValidationPhaseMetric `json:"phases"`
}

type ValidationPhaseMetric struct {
	Phase                 string `json:"phase"`
	DurationMs            int64  `json:"durationMs"`
	FilesProcessed        int64  `json:"filesProcessed"`
	LogicalBytesProcessed int64  `json:"logicalBytesProcessed"`
	IssueCount            int    `json:"issueCount"`
}

type ValidationResourceFailure struct {
	Phase          string `json:"phase"`
	Resource       string `json:"resource"`
	Observed       int64  `json:"observed"`
	Limit          int64  `json:"limit"`
	AvailableBytes int64  `json:"availableBytes,omitempty"`
	ReserveBytes   int64  `json:"reserveBytes,omitempty"`
	Path           string `json:"path,omitempty"`
}

type ValidationIssue struct {
	Severity ValidationIssueSeverity `json:"severity"`
	Kind     string                  `json:"kind"`
	Task     string                  `json:"task,omitempty"`
	Path     string                  `json:"path,omitempty"`
	Message  string                  `json:"message"`
}

type ArtifactValidationResult struct {
	Success              bool                     `json:"success"`
	Tasks                []ArtifactTaskValidation `json:"tasks"`
	IssueCount           int                      `json:"issueCount"`
	ObservedWriteCount   int                      `json:"observedWriteCount"`
	ProducedPathCount    int                      `json:"producedPathCount"`
	UndeclaredWriteCount int                      `json:"undeclaredWriteCount"`
	MissingOutputCount   int                      `json:"missingOutputCount"`
	Samples              ValidationPathSamples    `json:"samples"`
	Truncated            ValidationPathTruncation `json:"truncated"`
	Issues               []ValidationIssue        `json:"issues,omitempty"`
}

type ArtifactTaskValidation struct {
	Task                   string                   `json:"task"`
	Kind                   string                   `json:"kind"`
	Success                bool                     `json:"success"`
	InputCheck             string                   `json:"inputCheck"`
	OutputCheck            string                   `json:"outputCheck"`
	DeclaredInputs         []string                 `json:"declaredInputs"`
	MaterializedInputs     []string                 `json:"materializedInputs"`
	DependencyOutputs      []string                 `json:"dependencyOutputs"`
	DeclaredOutputs        []string                 `json:"declaredOutputs"`
	ProducedOutputs        []string                 `json:"producedOutputs"`
	ObservedWrites         []string                 `json:"observedWrites"`
	UndeclaredWrites       []string                 `json:"undeclaredWrites"`
	MissingOutputs         []string                 `json:"missingOutputs"`
	MaterializedInputCount int                      `json:"materializedInputCount"`
	DependencyOutputCount  int                      `json:"dependencyOutputCount"`
	ProducedPathCount      int                      `json:"producedPathCount"`
	ObservedWriteCount     int                      `json:"observedWriteCount"`
	UndeclaredWriteCount   int                      `json:"undeclaredWriteCount"`
	MissingOutputCount     int                      `json:"missingOutputCount"`
	IssueCount             int                      `json:"issueCount"`
	Samples                ValidationPathSamples    `json:"samples"`
	Truncated              ValidationPathTruncation `json:"truncated"`
	DurationMs             int64                    `json:"durationMs"`
	Error                  string                   `json:"error,omitempty"`
	Log                    string                   `json:"log,omitempty"`
	Issues                 []ValidationIssue        `json:"issues,omitempty"`
}

type ValidationPathSamples struct {
	UndeclaredWrites []string `json:"undeclaredWrites"`
	MissingOutputs   []string `json:"missingOutputs"`
	RejectedSymlinks []string `json:"rejectedSymlinks"`
	CopyFailures     []string `json:"copyFailures"`
	OtherIssues      []string `json:"otherIssues"`
}

type ValidationPathTruncation struct {
	MaterializedInputs bool `json:"materializedInputs"`
	DependencyOutputs  bool `json:"dependencyOutputs"`
	ProducedPaths      bool `json:"producedPaths"`
	ObservedWrites     bool `json:"observedWrites"`
	UndeclaredWrites   bool `json:"undeclaredWrites"`
	MissingOutputs     bool `json:"missingOutputs"`
	RejectedSymlinks   bool `json:"rejectedSymlinks"`
	CopyFailures       bool `json:"copyFailures"`
	OtherIssues        bool `json:"otherIssues"`
}

type OrderValidationResult struct {
	Success          bool                 `json:"success"`
	Complete         bool                 `json:"complete"`
	MaxOrders        int                  `json:"maxOrders"`
	TotalOrders      int                  `json:"totalOrders,omitempty"`
	DiscoveredOrders int                  `json:"discoveredOrders"`
	BaselineDigest   string               `json:"baselineDigest,omitempty"`
	Runs             []ValidationOrderRun `json:"runs"`
	Issues           []ValidationIssue    `json:"issues,omitempty"`
}

type ValidationOrderRun struct {
	Index             int      `json:"index"`
	Tasks             []string `json:"tasks"`
	Success           bool     `json:"success"`
	FailedTask        string   `json:"failedTask,omitempty"`
	Error             string   `json:"error,omitempty"`
	Log               string   `json:"log,omitempty"`
	OutputDigest      string   `json:"outputDigest,omitempty"`
	OutputDifferences []string `json:"outputDifferences,omitempty"`
	DurationMs        int64    `json:"durationMs"`
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

type GraphAffectedResult struct {
	Files            []string          `json:"files"`
	DirectlyAffected []string          `json:"directlyAffected"`
	Downstream       []string          `json:"downstream"`
	Explanations     []GraphFileImpact `json:"explanations,omitempty"`
	UnmatchedFiles   []string          `json:"unmatchedFiles,omitempty"`
}

type GraphFileImpact struct {
	File     string `json:"file"`
	Task     string `json:"task"`
	Affected bool   `json:"affected"`
	Reason   string `json:"reason"`
	Input    string `json:"input,omitempty"`
	Relative string `json:"relative,omitempty"`
	Ignore   string `json:"ignore,omitempty"`
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
	Worktree     string            `json:"worktree"`
	InstanceID   string            `json:"instanceId,omitempty"`
	Project      string            `json:"project,omitempty"`
	Target       string            `json:"target,omitempty"`
	CLIScope     string            `json:"cliScope,omitempty"`
	ChecksPassed bool              `json:"checksPassed"`
	Checks       []string          `json:"checks"`
	RequiredEnv  []DoctorEnvStatus `json:"requiredEnv,omitempty"`
	Warnings     []string          `json:"warnings,omitempty"`
}

type DoctorEnvStatus struct {
	Name   string `json:"name"`
	Set    bool   `json:"set"`
	Source string `json:"source,omitempty"`
}

type VersionResult struct {
	Version     string `json:"version"`
	ModulePath  string `json:"modulePath"`
	GoVersion   string `json:"goVersion"`
	VCSRevision string `json:"vcsRevision,omitempty"`
	VCSTime     string `json:"vcsTime,omitempty"`
	Modified    bool   `json:"modified,omitempty"`
}

type UpgradeResult struct {
	Command       []string `json:"command"`
	Package       string   `json:"package"`
	VersionTarget string   `json:"versionTarget"`
	Success       bool     `json:"success"`
	DurationMs    int64    `json:"durationMs"`
	Error         string   `json:"error,omitempty"`
	Output        string   `json:"output,omitempty"`
}
