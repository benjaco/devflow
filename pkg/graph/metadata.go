package graph

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"maps"
	"slices"
	"time"

	"github.com/benjaco/devflow/pkg/project"
)

// Metadata contains declarations only. Its digest identifies this projection,
// not executable behavior, current input contents, or successful verification.
type Metadata struct {
	Tasks   []TaskMetadata   `json:"tasks"`
	Targets []TargetMetadata `json:"targets"`
	Digest  string           `json:"digest,omitempty"`
}

type TaskMetadata struct {
	Name                      string                `json:"name"`
	Kind                      project.Kind          `json:"kind"`
	Deps                      []string              `json:"deps,omitempty"`
	RequiredCLIs              []string              `json:"requiredCLIs,omitempty"`
	RequiredEnv               []string              `json:"requiredEnv,omitempty"`
	Inputs                    InputMetadata         `json:"inputs"`
	Outputs                   OutputMetadata        `json:"outputs"`
	Purposes                  []project.Purpose     `json:"purposes,omitempty"`
	Effects                   *project.Effects      `json:"effects"`
	Cache                     bool                  `json:"cache"`
	Stamp                     bool                  `json:"stamp"`
	Restart                   project.RestartPolicy `json:"restart"`
	WatchRestartOnServiceDeps bool                  `json:"watchRestartOnServiceDeps"`
	AllowInWatch              bool                  `json:"allowInWatch"`
	Tags                      []string              `json:"tags,omitempty"`
	Description               string                `json:"description,omitempty"`
	ReadyTimeout              time.Duration         `json:"readyTimeout"`
	HasBeforeRun              bool                  `json:"hasBeforeRun"`
	HasRun                    bool                  `json:"hasRun"`
	HasReady                  bool                  `json:"hasReady"`
	HasAfterReady             bool                  `json:"hasAfterReady"`
	HasCacheKeyOverride       bool                  `json:"hasCacheKeyOverride"`
	HasDebug                  bool                  `json:"hasDebug"`
}

type InputMetadata struct {
	Paths                  []string                `json:"paths,omitempty"`
	Files                  []string                `json:"files,omitempty"`
	Dirs                   []string                `json:"dirs,omitempty"`
	Globs                  []string                `json:"globs,omitempty"`
	Filtered               []FilteredInputMetadata `json:"filtered,omitempty"`
	Env                    []string                `json:"env,omitempty"`
	Ignore                 []string                `json:"ignore,omitempty"`
	CustomFingerprintCount int                     `json:"customFingerprintCount"`
}

type FilteredInputMetadata struct {
	Path            string `json:"path"`
	FilterSignature string `json:"filterSignature"`
}

type OutputMetadata struct {
	Paths []string `json:"paths,omitempty"`
	Files []string `json:"files,omitempty"`
	Dirs  []string `json:"dirs,omitempty"`
}

type TargetMetadata struct {
	Name         string   `json:"name"`
	RootTasks    []string `json:"rootTasks"`
	RequiredCLIs []string `json:"requiredCLIs,omitempty"`
	RequiredEnv  []string `json:"requiredEnv,omitempty"`
	Description  string   `json:"description,omitempty"`
	Verification bool     `json:"verification"`
}

func (g *Graph) Metadata() Metadata {
	return g.metadata(slices.Sorted(maps.Keys(g.Tasks)), slices.Sorted(maps.Keys(g.Targets)))
}

func (g *Graph) MetadataForTarget(target string) (Metadata, error) {
	tasks, err := g.TargetClosure(target)
	if err != nil {
		return Metadata{}, err
	}
	slices.Sort(tasks)
	return g.metadata(tasks, []string{target}), nil
}

func (g *Graph) metadata(tasks, targets []string) Metadata {
	metadata := Metadata{Tasks: make([]TaskMetadata, 0, len(tasks)), Targets: make([]TargetMetadata, 0, len(targets))}
	for _, name := range tasks {
		metadata.Tasks = append(metadata.Tasks, taskMetadata(g.Tasks[name]))
	}
	for _, name := range targets {
		target := g.Targets[name]
		metadata.Targets = append(metadata.Targets, TargetMetadata{
			Name: target.Name, RootTasks: slices.Clone(target.RootTasks),
			RequiredCLIs: slices.Clone(target.RequiredCLIs), RequiredEnv: slices.Clone(target.RequiredEnv),
			Description: target.Description, Verification: target.Verification,
		})
	}
	// All fields have JSON-safe types, and the empty digest is omitted.
	data, _ := json.Marshal(metadata)
	digest := sha256.Sum256(data)
	metadata.Digest = hex.EncodeToString(digest[:])
	return metadata
}

func taskMetadata(task project.Task) TaskMetadata {
	metadata := TaskMetadata{
		Name: task.Name, Kind: task.Kind, Deps: slices.Clone(task.Deps),
		RequiredCLIs: slices.Clone(task.RequiredCLIs), RequiredEnv: slices.Clone(task.RequiredEnv),
		Inputs: InputMetadata{
			Paths: slices.Clone(task.Inputs.Paths), Files: slices.Clone(task.Inputs.Files), Dirs: slices.Clone(task.Inputs.Dirs),
			Globs: slices.Clone(task.Inputs.Globs), Env: slices.Clone(task.Inputs.Env), Ignore: slices.Clone(task.Inputs.Ignore),
			CustomFingerprintCount: len(task.Inputs.Custom),
		},
		Outputs:  OutputMetadata{Paths: slices.Clone(task.Outputs.Paths), Files: slices.Clone(task.Outputs.Files), Dirs: slices.Clone(task.Outputs.Dirs)},
		Purposes: slices.Clone(task.Purposes), Cache: task.Cache, Stamp: task.Stamp, Restart: task.Restart,
		WatchRestartOnServiceDeps: task.WatchRestartOnServiceDeps, AllowInWatch: task.AllowInWatch,
		Tags: slices.Clone(task.Tags), Description: task.Description, ReadyTimeout: task.ReadyTimeout,
		HasBeforeRun: task.BeforeRun != nil, HasRun: task.Run != nil, HasReady: task.Ready != nil,
		HasAfterReady: task.AfterReady != nil, HasCacheKeyOverride: task.CacheKeyOverride != nil, HasDebug: task.Debug != nil,
	}
	for _, input := range task.Inputs.Filtered {
		metadata.Inputs.Filtered = append(metadata.Inputs.Filtered, FilteredInputMetadata{Path: input.Path, FilterSignature: input.Filter.Signature})
	}
	if task.Effects != nil {
		metadata.Effects = &project.Effects{
			Writes: slices.Clone(task.Effects.Writes), Touches: slices.Clone(task.Effects.Touches),
			Invalidates: slices.Clone(task.Effects.Invalidates), Resources: slices.Clone(task.Effects.Resources),
		}
	}
	return metadata
}
