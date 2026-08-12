package validation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/graph"
)

func (v *Validator) validateOrders(ctx context.Context, req Request, template runtimeTemplate, closure []string, root string) (*api.OrderValidationResult, error) {
	result := &api.OrderValidationResult{
		Success:   true,
		Complete:  true,
		MaxOrders: req.MaxOrders,
		Runs:      []api.ValidationOrderRun{},
	}
	orders, exceeded := enumerateTopologicalOrders(v.graph, closure, req.MaxOrders)
	result.DiscoveredOrders = len(orders)
	if exceeded {
		result.Success = false
		result.Complete = false
		result.Issues = append(result.Issues, errorIssue(
			"order_limit_exceeded",
			"",
			"",
			fmt.Sprintf("target %q has more than %d valid task orders; raise --max-orders to run an exhaustive validation", req.Target, req.MaxOrders),
		))
		return result, nil
	}
	result.TotalOrders = len(orders)

	sandbox := filepath.Join(root, "worktree")
	logsRoot := filepath.Join(root, "logs")
	if err := os.MkdirAll(logsRoot, 0o755); err != nil {
		return nil, fmt.Errorf("prepare order validation logs: %w", err)
	}
	outputs := allOutputSpecs(v.graph, closure)
	var baseline filesystemSnapshot

	for orderIndex, taskOrder := range orders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		runStarted := time.Now()
		run := api.ValidationOrderRun{
			Index:   orderIndex + 1,
			Tasks:   append([]string(nil), taskOrder...),
			Success: true,
		}
		if err := copyWorktreeForOrders(ctx, req.Worktree, sandbox, outputs); err != nil {
			return nil, fmt.Errorf("seed order %d sandbox: %w", orderIndex+1, err)
		}
		if err := materializeInPlaceInputs(ctx, req.Worktree, sandbox, v.graph, closure); err != nil {
			return nil, fmt.Errorf("seed in-place inputs for order %d: %w", orderIndex+1, err)
		}
		execution, err := template.runtime(sandbox, string(api.ValidationModeOrders))
		if err != nil {
			return nil, err
		}
		completed := map[string]bool{}
		for taskIndex, name := range taskOrder {
			task := v.graph.Tasks[name]
			for _, dep := range task.Deps {
				if containsName(closure, dep) && !completed[dep] {
					return nil, fmt.Errorf("internal validation error: order %d scheduled %q before dependency %q", orderIndex+1, name, dep)
				}
			}
			depKeys := make([]string, 0, len(task.Deps))
			for _, dep := range task.Deps {
				depKeys = append(depKeys, "validation:"+dep)
			}
			sort.Strings(depKeys)
			logPath := filepath.Join(logsRoot, fmt.Sprintf("%04d-%04d-%s.log", orderIndex+1, taskIndex+1, safePathPart(name)))
			logText, taskErr := execution.runTask(ctx, task, logPath, depKeys)
			if symlinkErr := validateSandboxSymlinks(sandbox); symlinkErr != nil && taskErr == nil {
				taskErr = symlinkErr
			}
			if taskErr != nil {
				run.Success = false
				run.FailedTask = name
				run.Error = taskErr.Error()
				run.Log = logText
				break
			}
			missing, outputErr := missingDeclaredOutputs(sandbox, task)
			if outputErr != nil {
				return nil, fmt.Errorf("check task %q outputs in order %d: %w", name, orderIndex+1, outputErr)
			}
			if len(missing) > 0 {
				run.Success = false
				run.FailedTask = name
				run.Error = "task did not produce declared outputs: " + strings.Join(missing, ", ")
				run.Log = logText
				break
			}
			completed[name] = true
		}

		if run.Success {
			snapshot, missing, err := outputSnapshot(sandbox, outputs)
			if err != nil {
				return nil, fmt.Errorf("snapshot declared outputs after order %d: %w", orderIndex+1, err)
			}
			if len(missing) > 0 {
				run.Success = false
				run.Error = "declared outputs missing after order: " + strings.Join(missing, ", ")
			} else {
				run.OutputDigest = snapshotDigest(snapshot)
				if baseline == nil {
					baseline = snapshot
					result.BaselineDigest = run.OutputDigest
				} else if differences := snapshotDifferences(baseline, snapshot); len(differences) > 0 {
					run.Success = false
					run.Error = "declared outputs differ from the first successful order"
					run.OutputDifferences = differences
				}
			}
		}
		run.DurationMs = time.Since(runStarted).Milliseconds()
		if !run.Success {
			result.Success = false
			kind := "order_failed"
			if len(run.OutputDifferences) > 0 {
				kind = "order_output_mismatch"
			}
			result.Issues = append(result.Issues, errorIssue(
				kind,
				run.FailedTask,
				"",
				fmt.Sprintf("task order %d [%s] failed: %s", run.Index, strings.Join(run.Tasks, ", "), run.Error),
			))
		}
		result.Runs = append(result.Runs, run)
	}
	return result, nil
}

func enumerateTopologicalOrders(g *graph.Graph, closure []string, limit int) ([][]string, bool) {
	names := append([]string(nil), closure...)
	sort.Strings(names)
	inClosure := map[string]bool{}
	for _, name := range names {
		inClosure[name] = true
	}
	used := map[string]bool{}
	current := make([]string, 0, len(names))
	orders := make([][]string, 0)
	exceeded := false

	var visit func()
	visit = func() {
		if exceeded {
			return
		}
		if len(current) == len(names) {
			orders = append(orders, append([]string(nil), current...))
			if len(orders) > limit {
				exceeded = true
			}
			return
		}
		for _, name := range names {
			if used[name] || !dependenciesUsed(g, name, inClosure, used) {
				continue
			}
			used[name] = true
			current = append(current, name)
			visit()
			current = current[:len(current)-1]
			delete(used, name)
			if exceeded {
				return
			}
		}
	}
	visit()
	return orders, exceeded
}

func dependenciesUsed(g *graph.Graph, name string, inClosure, used map[string]bool) bool {
	for _, dep := range g.Tasks[name].Deps {
		if inClosure[dep] && !used[dep] {
			return false
		}
	}
	return true
}

func containsName(names []string, wanted string) bool {
	for _, name := range names {
		if name == wanted {
			return true
		}
	}
	return false
}
