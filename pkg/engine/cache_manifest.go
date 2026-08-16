package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/internal/jsonutil"
	"github.com/benjaco/devflow/internal/version"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/fingerprint"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

const (
	CacheKeyManifestSchemaVersion = 1
	CacheKeyManifestMaxAge        = 15 * time.Minute
	cacheKeyManifestMaxBytes      = 4 << 20
)

type CacheKeyManifestError struct {
	Reason     string
	DurationMs int64
}

func (e *CacheKeyManifestError) Error() string {
	if e == nil || e.Reason == "" {
		return "cache key manifest rejected"
	}
	return "cache key manifest rejected: " + e.Reason
}

type validatedCacheKeyManifest struct {
	manifest api.CacheKeyManifest
	byTask   map[string]api.CacheKeyManifestTask
}

type taskKeyComputation struct {
	key                string
	staticDigest       string
	semanticComponents []api.CacheKeyManifestComponent
	manifestComponents []string
	localInputsChanged bool
}

// WriteCacheKeyManifest atomically writes a manifest with owner-only
// permissions. The manifest is intended for an immediate Run request using
// CacheKeyManifestPath, not for durable or cross-job cache storage.
func WriteCacheKeyManifest(path string, manifest *api.CacheKeyManifest) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("cache key manifest path is required")
	}
	if manifest == nil {
		return fmt.Errorf("cache key manifest is required")
	}
	return jsonutil.WriteFileAtomic(path, manifest)
}

func (e *Engine) CacheKeyWithManifest(ctx context.Context, req Request) (*api.CacheKeyResult, *api.CacheKeyManifest, error) {
	inst, _, baseRT, err := e.prepareExecution(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	order, err := e.graph.TargetClosure(req.Target)
	if err != nil {
		return nil, nil, err
	}
	keys := map[string]string{}
	taskKeys := make([]api.TaskCacheKey, 0)
	manifestTasks := make([]api.CacheKeyManifestTask, 0, len(order))
	for _, name := range order {
		task := e.graph.Tasks[name]
		signature, err := fingerprint.TaskSignature(task)
		if err != nil {
			return nil, nil, fmt.Errorf("compute task signature for %q: %w", name, err)
		}
		manifestTask := api.CacheKeyManifestTask{
			Task:               name,
			Cache:              task.Cache,
			Stamp:              task.Stamp,
			TaskSignature:      signature,
			SemanticComponents: []api.CacheKeyManifestComponent{},
		}
		if task.Cache || task.Stamp {
			depKeys := make([]string, 0, len(task.Deps))
			for _, dep := range task.Deps {
				if key := keys[dep]; key != "" {
					depKeys = append(depKeys, key)
				}
			}
			rt := baseRT.WithTask(name, instance.LogPath(req.Worktree, inst.ID, name))
			rt.DepKeys = append([]string(nil), depKeys...)
			computation, err := e.computeTaskKey(ctx, rt, task, depKeys, nil)
			if err != nil {
				return nil, nil, fmt.Errorf("compute cache key for task %q: %w", name, err)
			}
			keys[name] = computation.key
			taskKeys = append(taskKeys, api.TaskCacheKey{Task: name, Key: computation.key, Stamp: task.Stamp})
			manifestTask.StaticInputDigest = computation.staticDigest
			manifestTask.SemanticComponents = computation.semanticComponents
			manifestTask.PreflightKey = computation.key
		}
		manifestTasks = append(manifestTasks, manifestTask)
	}
	aggregateKey := aggregateTargetCacheKey(project.CacheNamespace(e.project), req.Target, taskKeys)
	result := &api.CacheKeyResult{
		Project:    e.project.Name(),
		Target:     req.Target,
		InstanceID: inst.ID,
		Namespace:  project.CacheNamespace(e.project),
		Key:        aggregateKey,
		TaskKeys:   taskKeys,
	}
	createdAt := time.Now().UTC()
	manifest := &api.CacheKeyManifest{
		SchemaVersion:       CacheKeyManifestSchemaVersion,
		Compatibility:       cacheManifestCompatibility(),
		Project:             e.project.Name(),
		Namespace:           project.CacheNamespace(e.project),
		InstanceID:          inst.ID,
		WorktreeDigest:      cacheManifestValueDigest("worktree", cleanManifestWorktree(req.Worktree)),
		Target:              req.Target,
		GraphDigest:         e.cacheManifestGraphDigest(req.Target, order),
		ConfigurationDigest: cacheManifestConfigurationDigest(inst),
		EnvironmentHashes:   cacheManifestEnvironmentHashes(inst.Env),
		Tasks:               manifestTasks,
		AggregateKey:        aggregateKey,
		CreatedAt:           createdAt.Format(time.RFC3339Nano),
		ExpiresAt:           createdAt.Add(CacheKeyManifestMaxAge).Format(time.RFC3339Nano),
	}
	manifest.Integrity = cacheKeyManifestIntegrity(*manifest)
	return result, manifest, nil
}

func aggregateTargetCacheKey(namespace, target string, taskKeys []api.TaskCacheKey) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "devflow-target-cache-v1\x00%s\x00%s\x00", namespace, target)
	for _, item := range taskKeys {
		_, _ = fmt.Fprintf(hash, "%s\x00%s\x00%t\x00", item.Task, item.Key, item.Stamp)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (e *Engine) computeTaskKey(ctx context.Context, rt *project.Runtime, task project.Task, depKeys []string, reuse *validatedCacheKeyManifest) (taskKeyComputation, error) {
	manifestTask, hasManifestTask := api.CacheKeyManifestTask{}, false
	if reuse != nil {
		manifestTask, hasManifestTask = reuse.byTask[task.Name]
		if !hasManifestTask {
			return taskKeyComputation{}, &CacheKeyManifestError{Reason: fmt.Sprintf("manifest is missing task %q", task.Name)}
		}
	}
	if task.CacheKeyOverride != nil {
		var component api.CacheKeyManifestComponent
		fromManifest := false
		if reuse != nil {
			if len(manifestTask.SemanticComponents) != 1 || manifestTask.SemanticComponents[0].Name != "override" {
				return taskKeyComputation{}, &CacheKeyManifestError{Reason: fmt.Sprintf("manifest override component for task %q is invalid", task.Name)}
			}
			component = manifestTask.SemanticComponents[0]
			fromManifest = true
		} else {
			value, err := task.CacheKeyOverride(ctx, rt)
			if err != nil {
				return taskKeyComputation{}, err
			}
			if strings.TrimSpace(value) == "" {
				return taskKeyComputation{}, fmt.Errorf("task %q cache key override returned empty value", task.Name)
			}
			component = api.CacheKeyManifestComponent{Name: "override", Digest: fingerprint.CacheComponentDigest("override", value)}
		}
		key, err := fingerprint.TaskKey(fingerprint.TaskKeyInput{Task: task, OverrideDigest: component.Digest})
		if err != nil {
			return taskKeyComputation{}, err
		}
		computation := taskKeyComputation{key: key, semanticComponents: []api.CacheKeyManifestComponent{component}}
		if fromManifest {
			computation.manifestComponents = []string{"override"}
		}
		return computation, nil
	}

	inputHashes, envValues, err := fingerprint.CollectTaskStaticInputsWithCache(ctx, rt.Worktree, task, rt, e.inputs)
	if err != nil {
		return taskKeyComputation{}, err
	}
	staticDigest := fingerprint.StaticInputDigest(inputHashes, envValues)
	components := make([]api.CacheKeyManifestComponent, 0, len(task.Inputs.Custom))
	manifestComponents := make([]string, 0)
	if reuse != nil {
		if len(manifestTask.SemanticComponents) != len(task.Inputs.Custom) {
			return taskKeyComputation{}, &CacheKeyManifestError{Reason: fmt.Sprintf("manifest fingerprint components for task %q are invalid", task.Name)}
		}
		for index, component := range manifestTask.SemanticComponents {
			if component.Name != fmt.Sprintf("custom:%04d", index) || component.Digest == "" {
				return taskKeyComputation{}, &CacheKeyManifestError{Reason: fmt.Sprintf("manifest fingerprint component for task %q is invalid", task.Name)}
			}
			manifestComponents = append(manifestComponents, component.Name)
		}
		components = append(components, manifestTask.SemanticComponents...)
	}
	if len(components) == 0 && len(task.Inputs.Custom) > 0 {
		values, err := fingerprint.CollectTaskCustomFingerprints(ctx, task, rt)
		if err != nil {
			return taskKeyComputation{}, err
		}
		digests := fingerprint.SemanticFingerprintDigests(values)
		for index, digest := range digests {
			components = append(components, api.CacheKeyManifestComponent{Name: fmt.Sprintf("custom:%04d", index), Digest: digest})
		}
	}
	semanticDigests := make([]string, 0, len(components))
	for _, component := range components {
		semanticDigests = append(semanticDigests, component.Digest)
	}
	key, err := fingerprint.TaskKey(fingerprint.TaskKeyInput{
		Task:                     task,
		DepKeys:                  depKeys,
		StaticInputDigest:        staticDigest,
		CustomFingerprintDigests: semanticDigests,
	})
	if err != nil {
		return taskKeyComputation{}, err
	}
	return taskKeyComputation{
		key:                key,
		staticDigest:       staticDigest,
		semanticComponents: components,
		manifestComponents: manifestComponents,
		localInputsChanged: hasManifestTask && manifestTask.StaticInputDigest != "" && manifestTask.StaticInputDigest != staticDigest,
	}, nil
}

func (e *Engine) loadAndValidateCacheKeyManifest(path string, req Request, inst *api.Instance, order []string) (*validatedCacheKeyManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, &CacheKeyManifestError{Reason: "cannot open manifest"}
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, cacheKeyManifestMaxBytes+1))
	if err != nil {
		return nil, &CacheKeyManifestError{Reason: "cannot read manifest"}
	}
	if len(data) > cacheKeyManifestMaxBytes {
		return nil, &CacheKeyManifestError{Reason: "manifest exceeds the 4 MiB limit"}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var manifest api.CacheKeyManifest
	if err := decoder.Decode(&manifest); err != nil {
		return nil, &CacheKeyManifestError{Reason: "invalid JSON or schema"}
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, &CacheKeyManifestError{Reason: "invalid trailing JSON data"}
	}
	if manifest.SchemaVersion != CacheKeyManifestSchemaVersion {
		return nil, &CacheKeyManifestError{Reason: "unsupported schema version"}
	}
	if manifest.Integrity == "" || manifest.Integrity != cacheKeyManifestIntegrity(manifest) {
		return nil, &CacheKeyManifestError{Reason: "integrity check failed"}
	}
	if !validCacheManifestDigest(manifest.Integrity) || !validCacheManifestDigest(manifest.Compatibility) || !validCacheManifestDigest(manifest.WorktreeDigest) || !validCacheManifestDigest(manifest.GraphDigest) || !validCacheManifestDigest(manifest.ConfigurationDigest) || !validCacheManifestDigest(manifest.AggregateKey) {
		return nil, &CacheKeyManifestError{Reason: "manifest contains an invalid digest"}
	}
	for _, digest := range manifest.EnvironmentHashes {
		if !validCacheManifestDigest(digest) {
			return nil, &CacheKeyManifestError{Reason: "manifest contains an invalid environment digest"}
		}
	}
	createdAt, err := time.Parse(time.RFC3339Nano, manifest.CreatedAt)
	if err != nil {
		return nil, &CacheKeyManifestError{Reason: "invalid creation time"}
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, manifest.ExpiresAt)
	if err != nil || expiresAt.After(createdAt.Add(CacheKeyManifestMaxAge)) {
		return nil, &CacheKeyManifestError{Reason: "invalid expiration time"}
	}
	now := time.Now().UTC()
	if createdAt.After(now.Add(time.Minute)) {
		return nil, &CacheKeyManifestError{Reason: "creation time is in the future"}
	}
	if !expiresAt.After(now) || now.Sub(createdAt) > CacheKeyManifestMaxAge {
		return nil, &CacheKeyManifestError{Reason: "manifest has expired"}
	}
	if manifest.Compatibility != cacheManifestCompatibility() {
		return nil, &CacheKeyManifestError{Reason: "DevFlow build is incompatible"}
	}
	if manifest.Project != e.project.Name() || manifest.Namespace != project.CacheNamespace(e.project) {
		return nil, &CacheKeyManifestError{Reason: "manifest belongs to another project or cache namespace"}
	}
	if manifest.Target != req.Target {
		return nil, &CacheKeyManifestError{Reason: "manifest belongs to another target"}
	}
	if manifest.InstanceID != inst.ID || manifest.WorktreeDigest != cacheManifestValueDigest("worktree", cleanManifestWorktree(req.Worktree)) {
		return nil, &CacheKeyManifestError{Reason: "manifest belongs to another worktree instance"}
	}
	if manifest.GraphDigest != e.cacheManifestGraphDigest(req.Target, order) {
		return nil, &CacheKeyManifestError{Reason: "task graph or configuration changed"}
	}
	if manifest.ConfigurationDigest != cacheManifestConfigurationDigest(inst) {
		return nil, &CacheKeyManifestError{Reason: "instance configuration changed"}
	}
	if !reflect.DeepEqual(manifest.EnvironmentHashes, cacheManifestEnvironmentHashes(inst.Env)) {
		return nil, &CacheKeyManifestError{Reason: "relevant environment changed"}
	}
	if len(manifest.Tasks) != len(order) {
		return nil, &CacheKeyManifestError{Reason: "selected task set changed"}
	}
	byTask := make(map[string]api.CacheKeyManifestTask, len(manifest.Tasks))
	for index, name := range order {
		entry := manifest.Tasks[index]
		task := e.graph.Tasks[name]
		signature, err := fingerprint.TaskSignature(task)
		if err != nil || entry.Task != name || entry.TaskSignature != signature || entry.Cache != task.Cache || entry.Stamp != task.Stamp {
			return nil, &CacheKeyManifestError{Reason: "selected task configuration changed"}
		}
		if _, exists := byTask[name]; exists {
			return nil, &CacheKeyManifestError{Reason: "manifest contains duplicate tasks"}
		}
		if task.Cache || task.Stamp {
			if !validCacheManifestDigest(entry.PreflightKey) {
				return nil, &CacheKeyManifestError{Reason: "manifest contains an invalid task key"}
			}
			if task.CacheKeyOverride != nil {
				if entry.StaticInputDigest != "" || len(entry.SemanticComponents) != 1 || entry.SemanticComponents[0].Name != "override" || !validCacheManifestDigest(entry.SemanticComponents[0].Digest) {
					return nil, &CacheKeyManifestError{Reason: "manifest override component does not match the task"}
				}
			} else {
				if !validCacheManifestDigest(entry.StaticInputDigest) || len(entry.SemanticComponents) != len(task.Inputs.Custom) {
					return nil, &CacheKeyManifestError{Reason: "manifest fingerprint components do not match the task"}
				}
				for componentIndex, component := range entry.SemanticComponents {
					if component.Name != fmt.Sprintf("custom:%04d", componentIndex) || !validCacheManifestDigest(component.Digest) {
						return nil, &CacheKeyManifestError{Reason: "manifest contains an invalid fingerprint component"}
					}
				}
			}
		} else if entry.StaticInputDigest != "" || entry.PreflightKey != "" || len(entry.SemanticComponents) != 0 {
			return nil, &CacheKeyManifestError{Reason: "manifest contains key data for a non-cache task"}
		}
		byTask[name] = entry
	}
	return &validatedCacheKeyManifest{manifest: manifest, byTask: byTask}, nil
}

func validCacheManifestDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("extra JSON value")
		}
		return err
	}
	return nil
}

func cacheKeyManifestIntegrity(manifest api.CacheKeyManifest) string {
	manifest.Integrity = ""
	data, _ := json.Marshal(manifest)
	return cacheManifestValueDigest("integrity", string(data))
}

func cacheManifestCompatibility() string {
	info := version.Current()
	return fingerprint.CacheComponentDigest("compatibility", info.Version+"\x00"+info.VCSRevision)
}

func (e *Engine) cacheManifestGraphDigest(target string, order []string) string {
	type taskDigest struct {
		Name      string `json:"name"`
		Signature string `json:"signature"`
	}
	tasks := make([]taskDigest, 0, len(order))
	for _, name := range order {
		signature, _ := fingerprint.TaskSignature(e.graph.Tasks[name])
		tasks = append(tasks, taskDigest{Name: name, Signature: signature})
	}
	targetDefinition := e.graph.Targets[target]
	payload := struct {
		Target       string       `json:"target"`
		RootTasks    []string     `json:"rootTasks"`
		RequiredCLIs []string     `json:"requiredCLIs"`
		RequiredEnv  []string     `json:"requiredEnv"`
		Tasks        []taskDigest `json:"tasks"`
	}{
		Target:       target,
		RootTasks:    append([]string(nil), targetDefinition.RootTasks...),
		RequiredCLIs: append([]string(nil), targetDefinition.RequiredCLIs...),
		RequiredEnv:  append([]string(nil), targetDefinition.RequiredEnv...),
		Tasks:        tasks,
	}
	sort.Strings(payload.RootTasks)
	sort.Strings(payload.RequiredCLIs)
	sort.Strings(payload.RequiredEnv)
	data, _ := json.Marshal(payload)
	return cacheManifestValueDigest("graph", string(data))
}

func cacheManifestConfigurationDigest(inst *api.Instance) string {
	if inst == nil {
		return cacheManifestValueDigest("configuration", "nil")
	}
	type databaseBinding struct {
		Name            string `json:"name"`
		URLDigest       string `json:"urlDigest"`
		Host            string `json:"host"`
		Port            int    `json:"port"`
		ContainerPort   int    `json:"containerPort"`
		User            string `json:"user"`
		PasswordDigest  string `json:"passwordDigest"`
		Flavor          string `json:"flavor"`
		PostgresVersion int    `json:"postgresVersion"`
		Image           string `json:"image"`
		SidecarImage    string `json:"sidecarImage"`
	}
	payload := struct {
		Label string          `json:"label"`
		Ports map[string]int  `json:"ports"`
		DB    databaseBinding `json:"db"`
	}{
		Label: inst.Label,
		Ports: inst.Ports,
		DB: databaseBinding{
			Name:            inst.DB.Name,
			URLDigest:       cacheManifestValueDigest("database-url", inst.DB.URL),
			Host:            inst.DB.Host,
			Port:            inst.DB.Port,
			ContainerPort:   inst.DB.ContainerPort,
			User:            inst.DB.User,
			PasswordDigest:  cacheManifestValueDigest("database-password", inst.DB.Password),
			Flavor:          inst.DB.Flavor,
			PostgresVersion: inst.DB.PostgresVersion,
			Image:           inst.DB.Image,
			SidecarImage:    inst.DB.SidecarImage,
		},
	}
	data, _ := json.Marshal(payload)
	return cacheManifestValueDigest("configuration", string(data))
}

func cacheManifestEnvironmentHashes(env map[string]string) map[string]string {
	out := make(map[string]string, len(env))
	for key, value := range env {
		out[key] = cacheManifestValueDigest("environment:"+key, value)
	}
	return out
}

func cacheManifestValueDigest(kind, value string) string {
	return fingerprint.CacheComponentDigest(kind, value)
}

func cleanManifestWorktree(worktree string) string {
	abs, err := filepath.Abs(worktree)
	if err != nil {
		return filepath.Clean(worktree)
	}
	return filepath.Clean(abs)
}
