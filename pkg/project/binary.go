package project

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benjaco/devflow/pkg/process"
)

type BinaryTool struct {
	TaskName    string
	Description string
	Deps        []string
	Inputs      Inputs
	Output      string
	Build       process.CommandSpec
	Signature   string
	Tags        []string
}

type BinaryExecSpec struct {
	Args  []string
	Dir   string
	Env   map[string]string
	Grace time.Duration
}

func (t BinaryTool) BuildTask() Task {
	taskName := t.TaskName
	if taskName == "" {
		taskName = "build_" + filepath.Base(t.Output)
	}
	description := t.Description
	if description == "" {
		description = fmt.Sprintf("Build binary %s", t.Output)
	}
	signature := t.Signature
	if signature == "" {
		signature = binaryBuildSignature(t.Build)
	}
	return Task{
		Name:        taskName,
		Kind:        KindOnce,
		Deps:        append([]string(nil), t.Deps...),
		Inputs:      t.Inputs,
		Outputs:     Outputs{Files: []string{t.Output}},
		Cache:       true,
		Tags:        append([]string(nil), t.Tags...),
		Description: description,
		Signature:   signature,
		Run: func(ctx context.Context, rt *Runtime) error {
			if t.Output == "" {
				return fmt.Errorf("binary tool output path is required")
			}
			if filepath.IsAbs(t.Output) {
				return fmt.Errorf("binary tool output must be worktree-relative: %s", t.Output)
			}
			if t.Build.Name == "" {
				return fmt.Errorf("binary tool build command is required")
			}
			if err := os.MkdirAll(filepath.Dir(rt.Abs(t.Output)), 0o755); err != nil {
				return err
			}
			spec := t.Build
			if spec.Dir == "" {
				spec.Dir = rt.Worktree
			}
			spec.Env = mergeEnvMaps(rt.Env, spec.Env)
			if err := rt.RunCmdSpec(ctx, spec); err != nil {
				return err
			}
			if _, err := os.Stat(rt.Abs(t.Output)); err != nil {
				return fmt.Errorf("binary build did not produce %q: %w", t.Output, err)
			}
			return nil
		},
	}
}

func (t BinaryTool) Path(rt *Runtime) string {
	return resolveBinaryPath(rt, t.Output)
}

func (t BinaryTool) Run(ctx context.Context, rt *Runtime, args ...string) error {
	return t.RunSpec(ctx, rt, BinaryExecSpec{Args: args})
}

func (t BinaryTool) RunSpec(ctx context.Context, rt *Runtime, spec BinaryExecSpec) error {
	path, err := t.executablePath(rt)
	if err != nil {
		return err
	}
	return rt.RunCmdSpec(ctx, process.CommandSpec{
		Name: path,
		Args: append([]string(nil), spec.Args...),
		Dir:  execDir(rt, spec.Dir),
		Env:  mergeEnvMaps(rt.Env, spec.Env),
	})
}

func (t BinaryTool) Start(ctx context.Context, rt *Runtime, args ...string) (*process.Handle, error) {
	return t.StartSpec(ctx, rt, BinaryExecSpec{Args: args})
}

func (t BinaryTool) StartSpec(ctx context.Context, rt *Runtime, spec BinaryExecSpec) (*process.Handle, error) {
	path, err := t.executablePath(rt)
	if err != nil {
		return nil, err
	}
	return rt.StartServiceSpec(ctx, process.CommandSpec{
		Name:  path,
		Args:  append([]string(nil), spec.Args...),
		Dir:   execDir(rt, spec.Dir),
		Env:   mergeEnvMaps(rt.Env, spec.Env),
		Grace: spec.Grace,
	})
}

func (t BinaryTool) executablePath(rt *Runtime) (string, error) {
	path := resolveBinaryPath(rt, t.Output)
	if path == "" {
		return "", fmt.Errorf("binary tool output path is required")
	}
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("binary %q is not available: %w", path, err)
	}
	return path, nil
}

func resolveBinaryPath(rt *Runtime, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return rt.Abs(path)
}

func execDir(rt *Runtime, dir string) string {
	if dir == "" {
		return rt.Worktree
	}
	if filepath.IsAbs(dir) {
		return dir
	}
	return rt.Abs(dir)
}

func mergeEnvMaps(base, overlay map[string]string) map[string]string {
	if len(base) == 0 && len(overlay) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(overlay))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range overlay {
		out[key] = value
	}
	return out
}

func binaryBuildSignature(spec process.CommandSpec) string {
	payload := struct {
		Name string   `json:"name"`
		Args []string `json:"args"`
		Dir  string   `json:"dir,omitempty"`
		Env  []string `json:"env,omitempty"`
	}{
		Name: spec.Name,
		Args: append([]string(nil), spec.Args...),
		Dir:  spec.Dir,
		Env:  envPairs(spec.Env),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return spec.Name
	}
	return string(data)
}

func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, key+"="+env[key])
	}
	return pairs
}
