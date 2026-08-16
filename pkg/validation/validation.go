package validation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

const (
	DefaultMaxOrders = 1000
	maxCapturedLog   = 32 * 1024
)

var ErrValidationFailed = errors.New("validation failed")

type Request struct {
	Target                 string
	Worktree               string
	Mode                   api.ValidationMode
	Details                api.ValidationDetails
	MaxOrders              int
	MaxListedPaths         int
	MaxListedBytes         int64
	MaxFiles               int64
	MaxBytes               int64
	MaxTemporaryBytes      int64
	DiskSafetyReserveBytes int64
	OnEvent                func(api.Event)
}

type Validator struct {
	project project.Project
	graph   *graph.Graph
}

type runtimeTemplate struct {
	cfg       project.InstanceConfig
	id        string
	label     string
	ports     map[string]int
	createdAt time.Time
}

type executionRuntime struct {
	base    *project.Runtime
	handles *serviceHandles
}

type serviceHandles struct {
	mu      sync.Mutex
	handles []project.ServiceHandle
}

func New(p project.Project) (*Validator, error) {
	if p == nil {
		return nil, fmt.Errorf("validation project is required")
	}
	g, err := graph.New(p.Tasks(), p.Targets())
	if err != nil {
		return nil, err
	}
	return &Validator{project: p, graph: g}, nil
}

func (v *Validator) Run(ctx context.Context, req Request) (*api.ValidationResult, error) {
	started := time.Now()
	if strings.TrimSpace(req.Worktree) == "" {
		req.Worktree = "."
	}
	absWorktree, err := filepath.Abs(req.Worktree)
	if err != nil {
		return nil, err
	}
	realWorktree, err := filepath.EvalSymlinks(absWorktree)
	if err != nil {
		return nil, fmt.Errorf("resolve validation worktree: %w", err)
	}
	info, err := os.Stat(realWorktree)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("validation worktree %q is not a directory", realWorktree)
	}
	req.Worktree = filepath.Clean(realWorktree)
	if req.Target == "" {
		return nil, fmt.Errorf("validation target is required")
	}
	if req.Mode == "" {
		req.Mode = api.ValidationModeAll
	}
	if req.Mode != api.ValidationModeAll && req.Mode != api.ValidationModeArtifacts && req.Mode != api.ValidationModeOrders {
		return nil, fmt.Errorf("unknown validation mode %q (want all, artifacts, or orders)", req.Mode)
	}
	if req.MaxOrders <= 0 {
		req.MaxOrders = DefaultMaxOrders
	}
	if req.Details == "" {
		// Library callers historically received exhaustive path lists. The CLI
		// explicitly selects its bounded default; preserving this zero-value
		// behavior avoids silently changing embedded validator integrations.
		req.Details = api.ValidationDetailsFull
	}
	if req.Details != api.ValidationDetailsSummary && req.Details != api.ValidationDetailsIssues && req.Details != api.ValidationDetailsFull {
		return nil, fmt.Errorf("unknown validation details %q (want summary, issues, or full)", req.Details)
	}
	if req.MaxListedPaths <= 0 {
		req.MaxListedPaths = DefaultMaxListedPaths
	}
	if req.MaxListedBytes <= 0 {
		req.MaxListedBytes = DefaultMaxListedBytes
	}
	if req.MaxFiles == 0 {
		req.MaxFiles = DefaultValidationMaxFiles
	}
	if req.MaxBytes == 0 {
		req.MaxBytes = DefaultValidationMaxBytes
	}
	if req.MaxTemporaryBytes == 0 {
		req.MaxTemporaryBytes = DefaultValidationMaxTemp
	}
	if req.DiskSafetyReserveBytes == 0 {
		req.DiskSafetyReserveBytes = DefaultValidationDiskReserve
	}
	if req.DiskSafetyReserveBytes < 0 {
		req.DiskSafetyReserveBytes = 0
	}

	order, err := v.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, err
	}
	result := &api.ValidationResult{
		Project:        v.project.Name(),
		Target:         req.Target,
		Worktree:       req.Worktree,
		Mode:           req.Mode,
		Details:        req.Details,
		MaxListedPaths: req.MaxListedPaths,
	}
	tempRoot, err := os.MkdirTemp("", "devflow-validation-*")
	if err != nil {
		return nil, fmt.Errorf("create validation root: %w", err)
	}
	cleaned := false
	defer func() {
		if !cleaned {
			_ = fsutil.RemoveAllWritable(tempRoot)
		}
	}()
	budget := newValidationBudget(tempRoot, req)
	progress := newValidationProgress(req, budget)
	if err := progress.start(ctx, phasePreparing, false); err != nil {
		return nil, err
	}

	preflight := v.preflight(order)
	result.Issues = append(result.Issues, preflight...)
	progress.setIssues(len(result.Issues))
	if hasErrorIssues(preflight) {
		if err := finishValidationCleanup(ctx, tempRoot, budget, progress); err != nil {
			return nil, err
		}
		cleaned = true
		result.DurationMs = time.Since(started).Milliseconds()
		result.Metrics = budget.snapshot()
		applyValidationDetails(result, req)
		return result, nil
	}

	cfg, err := v.project.ConfigureInstance(ctx, req.Worktree)
	if err != nil {
		return nil, fmt.Errorf("configure validation instance: %w", err)
	}
	template, err := newRuntimeTemplate(req.Worktree, req.Target, cfg)
	if err != nil {
		return nil, err
	}
	resourceFailed := false
	if req.Mode == api.ValidationModeAll || req.Mode == api.ValidationModeArtifacts {
		artifacts, runErr := v.validateArtifacts(ctx, req, template, order, filepath.Join(tempRoot, "artifacts"), budget, progress)
		if runErr != nil {
			if !recordValidationResourceFailure(result, runErr) {
				return nil, runErr
			}
			resourceFailed = true
		} else {
			result.Artifacts = artifacts
			result.Issues = append(result.Issues, artifacts.Issues...)
			progress.setIssues(len(result.Issues))
		}
	}
	if !resourceFailed && (req.Mode == api.ValidationModeAll || req.Mode == api.ValidationModeOrders) {
		orders, runErr := v.validateOrders(ctx, req, template, order, filepath.Join(tempRoot, "orders"), budget, progress)
		if runErr != nil {
			if !recordValidationResourceFailure(result, runErr) {
				return nil, runErr
			}
			resourceFailed = true
		} else {
			result.Orders = orders
			result.Issues = append(result.Issues, orders.Issues...)
			progress.setIssues(len(result.Issues))
		}
	}

	result.Success = !resourceFailed && !hasErrorIssues(result.Issues)
	if result.Artifacts != nil {
		result.Success = result.Success && result.Artifacts.Success
	}
	if result.Orders != nil {
		result.Success = result.Success && result.Orders.Success
	}
	if err := finishValidationCleanup(ctx, tempRoot, budget, progress); err != nil {
		return nil, err
	}
	cleaned = true
	result.DurationMs = time.Since(started).Milliseconds()
	result.Metrics = budget.snapshot()
	applyValidationDetails(result, req)
	return result, nil
}

func recordValidationResourceFailure(result *api.ValidationResult, err error) bool {
	var limitErr *resourceLimitError
	if !errors.As(err, &limitErr) {
		return false
	}
	failure := limitErr.failure
	result.ResourceFailure = &failure
	result.Issues = append(result.Issues, errorIssue("resource_budget_exceeded", "", failure.Path, limitErr.Error()))
	return true
}

func finishValidationCleanup(ctx context.Context, tempRoot string, budget *validationBudget, progress *validationProgress) error {
	if err := progress.start(ctx, phaseCleaning, false); err != nil {
		return err
	}
	if err := fsutil.RemoveAllWritable(tempRoot); err != nil {
		return fmt.Errorf("clean validation temporary data: %w", err)
	}
	if err := budget.refreshTemporary(ctx); err != nil {
		return err
	}
	progress.complete()
	return nil
}

func (v *Validator) preflight(order []string) []api.ValidationIssue {
	issues := make([]api.ValidationIssue, 0)
	for _, name := range order {
		task := v.graph.Tasks[name]
		if project.IsServiceKind(task.Kind) {
			issues = append(issues, errorIssue("unsupported_task_kind", name, "", fmt.Sprintf("task %q is %s; validation only supports finite tasks", name, task.Kind)))
		}
		if task.Cache && len(taskOutputSpecs(task)) == 0 {
			issues = append(issues, errorIssue("missing_output_declaration", name, "", fmt.Sprintf("cacheable task %q does not declare any outputs", name)))
		}
		issues = append(issues, validateTaskPaths(task)...)
	}
	issues = append(issues, outputCollisionIssues(v.graph, order)...)
	return issues
}

func newRuntimeTemplate(worktree, target string, cfg project.InstanceConfig) (runtimeTemplate, error) {
	ports, err := allocateTemporaryPorts(cfg.PortNames)
	if err != nil {
		return runtimeTemplate{}, err
	}
	sum := sha256.Sum256([]byte(filepath.Clean(worktree) + "\x00" + target))
	label := cfg.Label
	if label == "" {
		label = filepath.Base(worktree)
	}
	return runtimeTemplate{
		cfg:       cfg,
		id:        "validation-" + hex.EncodeToString(sum[:6]),
		label:     label,
		ports:     ports,
		createdAt: time.Now().UTC(),
	}, nil
}

func (t runtimeTemplate) runtime(sandbox, validationMode string) (*executionRuntime, error) {
	inst := &api.Instance{
		ID:        t.id,
		Label:     t.label,
		Worktree:  sandbox,
		CreatedAt: t.createdAt,
		Ports:     cloneIntMap(t.ports),
		Env:       cloneStringMap(t.cfg.Env),
		DB:        t.cfg.DB,
		Processes: map[string]api.ProcessRef{},
	}
	inst.Env["DEVFLOW_INSTANCE_ID"] = inst.ID
	inst.Env["DEVFLOW_WORKTREE"] = sandbox
	inst.Env["DEVFLOW_VALIDATION"] = "1"
	inst.Env["DEVFLOW_VALIDATION_MODE"] = validationMode
	if t.cfg.Finalize != nil {
		if err := t.cfg.Finalize(inst); err != nil {
			return nil, fmt.Errorf("finalize validation instance: %w", err)
		}
	}
	if inst.Env == nil {
		inst.Env = map[string]string{}
	}
	inst.Env["DEVFLOW_INSTANCE_ID"] = inst.ID
	inst.Env["DEVFLOW_WORKTREE"] = sandbox
	inst.Env["DEVFLOW_VALIDATION"] = "1"
	inst.Env["DEVFLOW_VALIDATION_MODE"] = validationMode
	handles := &serviceHandles{}
	rt := &project.Runtime{
		Worktree: sandbox,
		Instance: inst,
		Mode:     api.ModeValidation,
		Env:      cloneStringMap(inst.Env),
		OnServiceHandle: func(_ string, handle project.ServiceHandle) {
			handles.add(handle)
		},
		OnPrompt: func(task string, _ process.PromptRequest) (process.PromptResponse, error) {
			return process.PromptResponse{}, fmt.Errorf("task %q requested an interactive prompt during validation", task)
		},
	}
	return &executionRuntime{base: rt, handles: handles}, nil
}

func (r *executionRuntime) runTask(ctx context.Context, task project.Task, logPath string, depKeys []string) (string, error) {
	if task.Kind == project.KindGroup || task.Run == nil {
		return "", nil
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(logPath, nil, 0o600); err != nil {
		return "", err
	}
	rt := r.base.WithTask(task.Name, logPath)
	rt.DepKeys = append([]string(nil), depKeys...)
	beforeHandles := r.handles.len()
	runErr := task.Run(ctx, rt)
	startedHandles := r.handles.takeFrom(beforeHandles)
	for _, handle := range startedHandles {
		_ = handle.Stop()
	}
	if len(startedHandles) > 0 && runErr == nil {
		runErr = fmt.Errorf("finite task %q started %d supervised service(s)", task.Name, len(startedHandles))
	}
	return readCapturedLog(logPath), runErr
}

func (h *serviceHandles) add(handle project.ServiceHandle) {
	if handle == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.handles = append(h.handles, handle)
}

func (h *serviceHandles) len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.handles)
}

func (h *serviceHandles) takeFrom(index int) []project.ServiceHandle {
	h.mu.Lock()
	defer h.mu.Unlock()
	if index < 0 || index > len(h.handles) {
		index = len(h.handles)
	}
	out := append([]project.ServiceHandle(nil), h.handles[index:]...)
	h.handles = h.handles[:index]
	return out
}

func allocateTemporaryPorts(names []string) (map[string]int, error) {
	unique := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		unique = append(unique, name)
	}
	sort.Strings(unique)
	listeners := make([]net.Listener, 0, len(unique))
	defer func() {
		for _, listener := range listeners {
			_ = listener.Close()
		}
	}()
	ports := make(map[string]int, len(unique))
	for _, name := range unique {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return nil, fmt.Errorf("allocate validation port %q: %w", name, err)
		}
		listeners = append(listeners, listener)
		ports[name] = listener.Addr().(*net.TCPAddr).Port
	}
	return ports, nil
}

func readCapturedLog(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if len(data) > maxCapturedLog {
		data = data[len(data)-maxCapturedLog:]
		return "...\n" + strings.TrimSpace(string(data))
	}
	return strings.TrimSpace(string(data))
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in)+4)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func hasErrorIssues(issues []api.ValidationIssue) bool {
	for _, issue := range issues {
		if issue.Severity == api.ValidationIssueError {
			return true
		}
	}
	return false
}

func errorIssue(kind, task, pathValue, message string) api.ValidationIssue {
	return api.ValidationIssue{Severity: api.ValidationIssueError, Kind: kind, Task: task, Path: pathValue, Message: message}
}
