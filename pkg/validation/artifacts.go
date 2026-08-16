package validation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

func (v *Validator) validateArtifacts(ctx context.Context, req Request, template runtimeTemplate, order []string, root string, budget *validationBudget, progress *validationProgress) (*api.ArtifactValidationResult, error) {
	result := &api.ArtifactValidationResult{Success: true, Tasks: []api.ArtifactTaskValidation{}}
	sandbox := filepath.Join(root, "worktree")
	archivesRoot := filepath.Join(root, "outputs")
	logsRoot := filepath.Join(root, "logs")
	for _, dir := range []string{sandbox, archivesRoot, logsRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("prepare artifact validation directory: %w", err)
		}
	}
	execution, err := template.runtime(sandbox, string(api.ValidationModeArtifacts))
	if err != nil {
		return nil, err
	}
	archiveByTask := map[string]string{}
	completed := map[string]bool{}

	for taskIndex, name := range order {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		taskStarted := time.Now()
		task := v.graph.Tasks[name]
		taskResult := api.ArtifactTaskValidation{
			Task:            name,
			Kind:            string(task.Kind),
			InputCheck:      "passed",
			OutputCheck:     "passed",
			DeclaredInputs:  declaredInputStrings(task),
			DeclaredOutputs: declaredOutputStrings(task),
		}
		if task.Kind == project.KindGroup {
			taskResult.InputCheck = "not_applicable"
		}

		if err := progress.start(ctx, phaseCopying, true); err != nil {
			return nil, err
		}
		if err := clearSandbox(sandbox); err != nil {
			return nil, fmt.Errorf("reset artifact sandbox for task %q: %w", name, err)
		}
		if err := budget.refreshTemporary(ctx); err != nil {
			return nil, err
		}

		dependencyFiles := map[string]bool{}
		checkedOut := make([]string, 0)
		excludedInputs := make([]outputSpec, 0)
		upstream := map[string]bool{}
		for _, dep := range v.graph.Upstream(task.Deps) {
			upstream[dep] = true
		}
		for _, candidate := range order {
			if !upstream[candidate] || !completed[candidate] {
				continue
			}
			archive := archiveByTask[candidate]
			if archive == "" {
				continue
			}
			candidateTask := v.graph.Tasks[candidate]
			files, moveErr := transferDeclaredOutputs(ctx, archive, sandbox, candidateTask, budget, progress)
			if moveErr != nil {
				return nil, fmt.Errorf("materialize dependency outputs from task %q for %q: %w", candidate, name, moveErr)
			}
			checkedOut = append(checkedOut, candidate)
			excludedInputs = append(excludedInputs, taskOutputSpecs(candidateTask)...)
			for _, file := range files {
				dependencyFiles[file] = true
			}
		}
		taskResult.DependencyOutputs = sortedSet(dependencyFiles)
		if err := progress.start(ctx, phaseProjecting, true); err != nil {
			return nil, err
		}
		inputs, err := materializeTaskInputs(ctx, req.Worktree, sandbox, task, excludedInputs, budget, progress)
		if err != nil {
			return nil, fmt.Errorf("materialize inputs for task %q: %w", name, err)
		}
		taskResult.MaterializedInputs = inputs

		if err := progress.start(ctx, phaseCapturing, false); err != nil {
			return nil, err
		}
		before, err := snapshotFilesystem(ctx, sandbox, true, budget, progress)
		if err != nil {
			return nil, fmt.Errorf("snapshot task %q inputs: %w", name, err)
		}
		depKeys := make([]string, 0, len(task.Deps))
		for _, dep := range task.Deps {
			depKeys = append(depKeys, "validation:"+dep)
		}
		sort.Strings(depKeys)
		logPath := filepath.Join(logsRoot, fmt.Sprintf("%04d-%s.log", taskIndex+1, safePathPart(name)))
		if err := progress.start(ctx, phaseRunning, false); err != nil {
			return nil, err
		}
		logText, runErr := execution.runTask(ctx, task, logPath, depKeys)
		if symlinkErr := validateSandboxSymlinks(sandbox); symlinkErr != nil && runErr == nil {
			runErr = symlinkErr
		}
		if err := budget.refreshTemporary(ctx); err != nil {
			return nil, err
		}
		if err := progress.start(ctx, phaseCapturing, false); err != nil {
			return nil, err
		}
		after, err := snapshotFilesystem(ctx, sandbox, true, budget, progress)
		if err != nil {
			return nil, fmt.Errorf("snapshot task %q outputs: %w", name, err)
		}
		if err := progress.start(ctx, phaseAnalyzing, false); err != nil {
			return nil, err
		}
		taskResult.ObservedWrites = changedFiles(before, after)
		specs := taskOutputSpecs(task)
		taskResult.ProducedOutputs = matchingSnapshotPaths(after, specs)
		for _, changed := range taskResult.ObservedWrites {
			if !anyOutputRelated(specs, changed) {
				taskResult.UndeclaredWrites = append(taskResult.UndeclaredWrites, changed)
			}
		}
		missing, err := missingDeclaredOutputs(sandbox, task)
		if err != nil {
			return nil, fmt.Errorf("check task %q outputs: %w", name, err)
		}
		taskResult.MissingOutputs = missing

		if runErr != nil {
			taskResult.InputCheck = "failed"
			taskResult.OutputCheck = "failed"
			taskResult.Error = runErr.Error()
			taskResult.Log = logText
			taskResult.Issues = append(taskResult.Issues, errorIssue(
				"task_failed_with_projected_inputs",
				name,
				"",
				fmt.Sprintf("task %q could not run with only its declared worktree inputs and upstream declared outputs: %v", name, runErr),
			))
		}
		for _, changed := range taskResult.UndeclaredWrites {
			taskResult.OutputCheck = "failed"
			taskResult.Issues = append(taskResult.Issues, errorIssue(
				"undeclared_output",
				name,
				changed,
				fmt.Sprintf("task %q changed %q outside its declared outputs", name, changed),
			))
		}
		for _, missingOutput := range taskResult.MissingOutputs {
			taskResult.OutputCheck = "failed"
			taskResult.Issues = append(taskResult.Issues, errorIssue(
				"missing_output",
				name,
				missingOutput,
				fmt.Sprintf("task %q did not produce declared output %q", name, missingOutput),
			))
		}

		taskResult.Success = runErr == nil && !hasErrorIssues(taskResult.Issues)
		taskResult.DurationMs = time.Since(taskStarted).Milliseconds()
		if !taskResult.Success {
			result.Success = false
		}
		result.Issues = append(result.Issues, taskResult.Issues...)
		result.Tasks = append(result.Tasks, taskResult)
		progress.setIssues(progress.issueCount + len(taskResult.Issues))

		if !taskResult.Success {
			break
		}
		if err := progress.start(ctx, phaseArchiving, false); err != nil {
			return nil, err
		}
		for _, candidate := range checkedOut {
			archive := archiveByTask[candidate]
			if _, err := transferDeclaredOutputs(ctx, sandbox, archive, v.graph.Tasks[candidate], budget, progress); err != nil {
				return nil, fmt.Errorf("return dependency outputs from task %q after %q: %w", candidate, name, err)
			}
		}
		archive := filepath.Join(archivesRoot, fmt.Sprintf("%04d", taskIndex+1))
		if err := fsutil.RemoveAllWritable(archive); err != nil {
			return nil, fmt.Errorf("reset output archive for task %q: %w", name, err)
		}
		if _, err := transferDeclaredOutputs(ctx, sandbox, archive, task, budget, progress); err != nil {
			return nil, fmt.Errorf("archive outputs for task %q: %w", name, err)
		}
		archiveByTask[name] = archive
		completed[name] = true
	}
	return result, nil
}

func safePathPart(value string) string {
	out := make([]rune, 0, len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "task"
	}
	return string(out)
}
