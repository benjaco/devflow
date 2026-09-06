package instance

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/api"
)

type State struct {
	RunID     string                    `json:"runId,omitempty"`
	Target    string                    `json:"target"`
	Mode      api.RunMode               `json:"mode"`
	Nodes     map[string]api.NodeStatus `json:"nodes"`
	UpdatedAt time.Time                 `json:"updatedAt"`
}

type TaskStamp struct {
	Task      string    `json:"task"`
	Key       string    `json:"key"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func Resolve(worktree, label string) (*api.Instance, error) {
	real, err := fsutil.Realpath(worktree)
	if err != nil {
		real, err = filepath.Abs(worktree)
		if err != nil {
			return nil, err
		}
	}
	id := instanceID(real)
	path := instancePath(real, id)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}

	var inst api.Instance
	if err := jsonutil.ReadFile(filepath.Join(path, "instance.json"), &inst); err == nil {
		if err := overlayDaemonControl(&inst); err != nil {
			return nil, err
		}
		if err := registerIndex(inst.ID, real); err != nil {
			return nil, err
		}
		return &inst, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read instance state %s: %w", filepath.Join(path, "instance.json"), err)
	}

	inst = api.Instance{
		ID:        id,
		Label:     label,
		Worktree:  real,
		CreatedAt: time.Now().UTC(),
		Ports:     map[string]int{},
		Env:       map[string]string{},
		Processes: map[string]api.ProcessRef{},
	}
	if err := overlayDaemonControl(&inst); err != nil {
		return nil, err
	}
	if err := Save(&inst); err != nil {
		return nil, err
	}
	return &inst, registerIndex(inst.ID, real)
}

func IDForWorktree(worktree string) (string, string, error) {
	real, err := fsutil.Realpath(worktree)
	if err != nil {
		real, err = filepath.Abs(worktree)
		if err != nil {
			return "", "", err
		}
	}
	return instanceID(real), real, nil
}

func Save(inst *api.Instance) error {
	if err := os.MkdirAll(instancePath(inst.Worktree, inst.ID), 0o755); err != nil {
		return err
	}
	if err := jsonutil.WriteFileAtomic(filepath.Join(instancePath(inst.Worktree, inst.ID), "instance.json"), inst); err != nil {
		return err
	}
	return fsutil.WriteEnvFile(filepath.Join(instancePath(inst.Worktree, inst.ID), "runtime.env"), inst.Env)
}

func SaveStatus(worktree, instanceID, target string, mode api.RunMode, nodes map[string]api.NodeStatus) error {
	state := State{
		Target:    target,
		Mode:      mode,
		Nodes:     nodes,
		UpdatedAt: time.Now().UTC(),
	}
	for _, node := range nodes {
		state.RunID = node.RunID
		break
	}
	return jsonutil.WriteFileAtomic(filepath.Join(instancePath(worktree, instanceID), "status.json"), state)
}

func LoadStatus(worktree, instanceID string) (*State, error) {
	var state State
	if err := jsonutil.ReadFile(filepath.Join(instancePath(worktree, instanceID), "status.json"), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func TaskStampPath(worktree, instanceID, task string) string {
	sum := sha1.Sum([]byte(task))
	name := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(instancePath(worktree, instanceID), "task-stamps", name)
}

func WriteTaskStamp(worktree, instanceID, task, key string) error {
	path := TaskStampPath(worktree, instanceID, task)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return jsonutil.WriteFileAtomic(path, TaskStamp{
		Task:      task,
		Key:       key,
		UpdatedAt: time.Now().UTC(),
	})
}

func LoadTaskStamp(worktree, instanceID, task string) (TaskStamp, bool, error) {
	var stamp TaskStamp
	if err := jsonutil.ReadFile(TaskStampPath(worktree, instanceID, task), &stamp); err != nil {
		if os.IsNotExist(err) {
			return TaskStamp{}, false, nil
		}
		return TaskStamp{}, false, err
	}
	if stamp.Task != task {
		return TaskStamp{}, false, nil
	}
	return stamp, true, nil
}

func RemoveTaskStamp(worktree, instanceID, task string) error {
	if err := os.Remove(TaskStampPath(worktree, instanceID, task)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func RemoveTaskStamps(worktree, instanceID string) error {
	path := filepath.Join(instancePath(worktree, instanceID), "task-stamps")
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func Load(worktree, instanceID string) (*api.Instance, error) {
	var inst api.Instance
	if err := jsonutil.ReadFile(filepath.Join(instancePath(worktree, instanceID), "instance.json"), &inst); err != nil {
		return nil, err
	}
	if err := overlayDaemonControl(&inst); err != nil {
		return nil, err
	}
	return &inst, nil
}

// LoadDaemon reads the daemon's own record so execution snapshot writes cannot
// change daemon identity or make daemon control depend on runtime.env.
func LoadDaemon(worktree, instanceID string) (api.DaemonRef, error) {
	var ref api.DaemonRef
	path := filepath.Join(instancePath(worktree, instanceID), "daemon.json")
	if err := jsonutil.ReadFile(path, &ref); err != nil {
		if os.IsNotExist(err) {
			return api.DaemonRef{}, nil
		}
		return api.DaemonRef{}, fmt.Errorf("read daemon control %s: %w", path, err)
	}
	return ref, nil
}

func overlayDaemonControl(inst *api.Instance) error {
	ref, err := LoadDaemon(inst.Worktree, inst.ID)
	if err != nil {
		return err
	}
	inst.Daemon = ref
	return nil
}

func List() ([]api.InstanceSummary, error) {
	index, err := readIndex()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(index))
	for id := range index {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := make([]api.InstanceSummary, 0, len(ids))
	for _, id := range ids {
		worktree := index[id]
		inst, err := Load(worktree, id)
		if err != nil {
			continue
		}
		summary := api.InstanceSummary{
			ID:       inst.ID,
			Label:    inst.Label,
			Worktree: inst.Worktree,
			Ports:    inst.Ports,
			DB:       DisplayDB(inst.DB),
		}
		if state, err := LoadStatus(worktree, id); err == nil {
			summary.Target = state.Target
			summary.States = map[string]string{}
			for name, node := range state.Nodes {
				summary.States[name] = string(node.State)
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

func DisplayDB(db api.DBInstance) api.DBInstance {
	db.URL = ""
	db.Password = ""
	return db
}

func LogPath(worktree, instanceID, task string) string {
	return filepath.Join(worktree, ".devflow", "logs", instanceID, task+".log")
}

func EventsPath(worktree, instanceID string) string {
	return filepath.Join(instancePath(worktree, instanceID), "events.jsonl")
}

func DaemonSocketPath(instanceID string) (string, error) {
	base := os.TempDir()
	if info, err := os.Stat("/tmp"); err == nil && info.IsDir() {
		base = "/tmp"
	}
	path := filepath.Join(base, fmt.Sprintf("devflow-daemon-%d", os.Getuid()))
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(path, instanceID+".sock"), nil
}

func FlushRoot(worktree, instanceID string) string {
	return filepath.Join(instancePath(worktree, instanceID), "flush")
}

func FlushRequestPath(worktree, instanceID, requestID string) string {
	return filepath.Join(FlushRoot(worktree, instanceID), "requests", requestID+".json")
}

func FlushAckPath(worktree, instanceID, requestID string) string {
	return filepath.Join(FlushRoot(worktree, instanceID), "acks", requestID+".json")
}

func FlushSyncDir(worktree, instanceID string) string {
	return filepath.Join(FlushRoot(worktree, instanceID), "sync")
}

func FlushSyncPath(worktree, instanceID, requestID string) string {
	return filepath.Join(FlushSyncDir(worktree, instanceID), requestID+".sync")
}

func FlushWatchReadyPath(worktree, instanceID string) string {
	return filepath.Join(FlushRoot(worktree, instanceID), "watch.ready")
}

func WriteFlushRequest(worktree, instanceID string, req api.FlushRequest) error {
	path := FlushRequestPath(worktree, instanceID, req.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return jsonutil.WriteFileAtomic(path, req)
}

func LoadFlushRequest(worktree, instanceID, requestID string) (api.FlushRequest, error) {
	var req api.FlushRequest
	if err := jsonutil.ReadFile(FlushRequestPath(worktree, instanceID, requestID), &req); err != nil {
		return api.FlushRequest{}, err
	}
	return req, nil
}

func WriteFlushAck(worktree, instanceID string, result api.FlushResult) error {
	path := FlushAckPath(worktree, instanceID, result.RequestID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return jsonutil.WriteFileAtomic(path, result)
}

func LoadFlushAck(worktree, instanceID, requestID string) (api.FlushResult, error) {
	var result api.FlushResult
	if err := jsonutil.ReadFile(FlushAckPath(worktree, instanceID, requestID), &result); err != nil {
		return api.FlushResult{}, err
	}
	return result, nil
}

func RemoveFlushRequest(worktree, instanceID, requestID string) error {
	if err := os.Remove(FlushRequestPath(worktree, instanceID, requestID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func CacheRoot() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "devflow", "cache")
}

func GlobalStateRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(base, "devflow", "state")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func RepoSharedStateRoot(worktree string) (string, error) {
	if root, err := repoSharedRoot(worktree); err == nil {
		path := filepath.Join(root, "state")
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", err
		}
		return path, nil
	}
	return GlobalStateRoot()
}

func instancePath(worktree, instanceID string) string {
	return filepath.Join(worktree, ".devflow", "state", "instances", instanceID)
}

func GitCommonDir(worktree string) (string, error) {
	root, err := filepath.Abs(worktree)
	if err != nil {
		return "", err
	}
	cmd := exec.Command("git", "rev-parse", "--git-common-dir")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("git common dir output was empty")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	if real, err := fsutil.Realpath(common); err == nil {
		return real, nil
	}
	return filepath.Abs(common)
}

func repoSharedRoot(worktree string) (string, error) {
	common, err := GitCommonDir(worktree)
	if err != nil {
		return "", err
	}
	path := filepath.Join(common, "devflow")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func StopProcesses(inst *api.Instance, task string) ([]string, error) {
	refs := map[string]int{}
	for name, ref := range inst.Processes {
		if task != "" && name != task {
			continue
		}
		if ref.PID <= 0 {
			continue
		}
		refs[name] = ref.PID
	}
	stopped, err := stopNamedProcessGroups(refs, 3*time.Second)
	if err != nil {
		return stopped, err
	}
	for name := range refs {
		delete(inst.Processes, name)
	}
	return stopped, Save(inst)
}

// RecordDaemon owns only daemon control metadata. Daemon startup can run while
// a direct executor owns instance.json, runtime.env, and service snapshots.
func RecordDaemon(inst *api.Instance, pid int, logPath string) error {
	if _, err := LoadDaemon(inst.Worktree, inst.ID); err != nil {
		return err
	}
	ref := api.DaemonRef{
		PID:       pid,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}
	return jsonutil.WriteFileAtomic(filepath.Join(instancePath(inst.Worktree, inst.ID), "daemon.json"), ref)
}

func ClearDaemon(inst *api.Instance) error {
	path := filepath.Join(instancePath(inst.Worktree, inst.ID), "daemon.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	inst.Daemon = api.DaemonRef{}
	return nil
}

func StopDaemonWork(inst *api.Instance, extra map[string]int, daemonPID int) ([]string, error) {
	refs := map[string]int{}
	for name, ref := range inst.Processes {
		addStopRef(refs, name, ref.PID)
	}
	for name, pid := range extra {
		addStopRef(refs, name, pid)
	}
	// Process and status snapshots can name the same PID. The daemon must stay
	// alive until it has completed cleanup and delivered the stop response.
	for name, pid := range refs {
		if pid == daemonPID {
			delete(refs, name)
		}
	}
	stopped, stopErr := stopNamedProcessGroups(refs, 3*time.Second)
	stoppedPIDs := map[int]bool{}
	for _, name := range stopped {
		if pid := refs[name]; pid > 0 {
			stoppedPIDs[pid] = true
		}
	}
	for name, ref := range inst.Processes {
		if stoppedPIDs[ref.PID] || !ProcessAlive(ref.PID) {
			delete(inst.Processes, name)
		}
	}
	return stopped, errors.Join(stopErr, Save(inst))
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return processAlive(pid)
}

func addStopRef(refs map[string]int, name string, pid int) {
	if name == "" || pid <= 0 {
		return
	}
	if existing, ok := refs[name]; ok && existing > 0 {
		return
	}
	refs[name] = pid
}

func stopNamedProcessGroups(refs map[string]int, grace time.Duration) ([]string, error) {
	orderedNames := make([]string, 0, len(refs))
	for name := range refs {
		orderedNames = append(orderedNames, name)
	}
	sort.Strings(orderedNames)
	live := map[int]bool{}
	pidNames := map[int]string{}
	for _, name := range orderedNames {
		pid := refs[name]
		if pid <= 0 || !ProcessAlive(pid) {
			continue
		}
		// Process and status snapshots can point at the same process.
		// Keep one deterministic resource name in the actual-result set.
		if _, exists := pidNames[pid]; exists {
			continue
		}
		pidNames[pid] = name
		live[pid] = true
	}
	if len(live) == 0 {
		return []string{}, nil
	}
	for pid := range live {
		_ = terminateProcessGroup(pid)
	}
	waitForProcessExit(live, grace)
	for pid := range live {
		if ProcessAlive(pid) {
			_ = killProcessGroup(pid)
		}
	}
	waitForProcessExit(live, 500*time.Millisecond)
	stopped := make([]string, 0, len(live))
	remaining := make([]string, 0)
	for pid, name := range pidNames {
		if ProcessAlive(pid) {
			remaining = append(remaining, fmt.Sprintf("%s (pid %d)", name, pid))
			continue
		}
		stopped = append(stopped, name)
	}
	sort.Strings(stopped)
	sort.Strings(remaining)
	if len(remaining) > 0 {
		return stopped, fmt.Errorf("processes remained alive after stop: %s", strings.Join(remaining, ", "))
	}
	return stopped, nil
}

func waitForProcessExit(pids map[int]bool, timeout time.Duration) {
	if timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		alive := false
		for pid := range pids {
			if ProcessAlive(pid) {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func instanceID(realpath string) string {
	sum := sha1.Sum([]byte(realpath))
	return hex.EncodeToString(sum[:])[:12]
}

func registerIndex(instanceID, worktree string) error {
	root, err := GlobalStateRoot()
	if err != nil {
		return err
	}
	lockFile, err := lock.Acquire(filepath.Join(root, "instance-index.lock"))
	if err != nil {
		return err
	}
	defer lockFile.Release()

	index, err := readIndex()
	if err != nil {
		return err
	}
	index[instanceID] = worktree
	return jsonutil.WriteFileAtomic(filepath.Join(root, "instance-index.json"), index)
}

func readIndex() (map[string]string, error) {
	root, err := GlobalStateRoot()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(root, "instance-index.json")
	index := map[string]string{}
	if err := jsonutil.ReadFile(path, &index); err != nil {
		if os.IsNotExist(err) {
			return index, nil
		}
		return nil, fmt.Errorf("read instance index: %w", err)
	}
	return index, nil
}
