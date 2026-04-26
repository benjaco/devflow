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
	"syscall"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/internal/lock"
	"github.com/benjaco/devflow/pkg/api"
)

type State struct {
	Target    string                    `json:"target"`
	Mode      api.RunMode               `json:"mode"`
	Nodes     map[string]api.NodeStatus `json:"nodes"`
	UpdatedAt time.Time                 `json:"updatedAt"`
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
		if err := registerIndex(inst.ID, real); err != nil {
			return nil, err
		}
		return &inst, nil
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
	return jsonutil.WriteFileAtomic(filepath.Join(instancePath(worktree, instanceID), "status.json"), state)
}

func LoadStatus(worktree, instanceID string) (*State, error) {
	var state State
	if err := jsonutil.ReadFile(filepath.Join(instancePath(worktree, instanceID), "status.json"), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func Load(worktree, instanceID string) (*api.Instance, error) {
	var inst api.Instance
	if err := jsonutil.ReadFile(filepath.Join(instancePath(worktree, instanceID), "instance.json"), &inst); err != nil {
		return nil, err
	}
	return &inst, nil
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

func InteractionAnswerPath(worktree, instanceID, promptID string) string {
	return filepath.Join(instancePath(worktree, instanceID), "interactions", promptID+".json")
}

func WriteInteractionAnswer(worktree, instanceID, promptID, value string) error {
	path := InteractionAnswerPath(worktree, instanceID, promptID)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return jsonutil.WriteFileAtomic(path, map[string]string{"value": value})
}

func ConsumeInteractionAnswer(worktree, instanceID, promptID string) (string, bool, error) {
	path := InteractionAnswerPath(worktree, instanceID, promptID)
	var payload map[string]string
	if err := jsonutil.ReadFile(path, &payload); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	return payload["value"], true, nil
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
	stopped := make([]string, 0)
	for name, ref := range inst.Processes {
		if task != "" && name != task {
			continue
		}
		if ref.PID <= 0 {
			continue
		}
		if err := syscall.Kill(-ref.PID, syscall.SIGTERM); err != nil {
			if err := syscall.Kill(ref.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
				return stopped, err
			}
		}
		stopped = append(stopped, name)
		delete(inst.Processes, name)
	}
	sort.Strings(stopped)
	return stopped, Save(inst)
}

func RecordDetachedRun(inst *api.Instance, cfg api.RunConfig, supervisorPID int, logPath string) error {
	inst.LastRun = cfg
	inst.Supervisor = api.SupervisorRef{
		PID:       supervisorPID,
		StartedAt: time.Now().UTC(),
		LogPath:   logPath,
	}
	return Save(inst)
}

func ClearSupervisor(inst *api.Instance) error {
	inst.Supervisor = api.SupervisorRef{}
	if inst.Processes == nil {
		inst.Processes = map[string]api.ProcessRef{}
	}
	return Save(inst)
}

func StopSupervisor(inst *api.Instance) error {
	if inst.Supervisor.PID <= 0 {
		return nil
	}
	if err := syscall.Kill(-inst.Supervisor.PID, syscall.SIGTERM); err != nil {
		if err := syscall.Kill(inst.Supervisor.PID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	inst.Supervisor = api.SupervisorRef{}
	inst.Processes = map[string]api.ProcessRef{}
	return Save(inst)
}

func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
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
