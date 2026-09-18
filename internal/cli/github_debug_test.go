package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/project"
)

func TestGitHubDebugUsesRunnerSignal(t *testing.T) {
	for _, tc := range []struct {
		name, actions, runner, step, diagnostic string
		wantDebug                               bool
	}{
		{name: "enabled", actions: "true", runner: "1", wantDebug: true},
		{name: "unset", actions: "true", runner: "<unset>"},
		{name: "empty", actions: "true"},
		{name: "zero", actions: "true", runner: "0"},
		{name: "false", actions: "true", runner: "false"},
		{name: "true-is-not-one", actions: "true", runner: "true"},
		{name: "whitespace", actions: "true", runner: " 1 "},
		{name: "outside-actions", actions: "false", runner: "1"},
		{name: "actions-unset", runner: "1"},
		{name: "runner-diagnostics-only", actions: "true", diagnostic: "true"},
		{name: "raw-settings-do-not-override-runner", actions: "true", runner: "0", step: "true", diagnostic: "true"},
		{name: "runner-is-authoritative", actions: "true", runner: "1", step: "false", diagnostic: "false", wantDebug: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", tc.actions)
			t.Setenv("RUNNER_DEBUG", tc.runner)
			if tc.runner == "<unset>" {
				if err := os.Unsetenv("RUNNER_DEBUG"); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("ACTIONS_STEP_DEBUG", tc.step)
			t.Setenv("ACTIONS_RUNNER_DEBUG", tc.diagnostic)
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
				rt.EmitLogLine("stdout", "debug-selection-output")
				return nil
			}}}, "check")
			// Conflicting task environment must not select CLI presentation.
			p.runnerDebugEnvironment = "1"
			if tc.wantDebug {
				p.runnerDebugEnvironment = "0"
			}
			var stderr bytes.Buffer
			result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json")
			if err != nil || !result.Success || result.Mode != api.ModeCI || len(result.Nodes) != 1 {
				t.Fatalf("debug changed execution/JSON: %v stdout=%s stderr=%s", err, stdout, &stderr)
			}
			if got := strings.Contains(stderr.String(), "::debug::[devflow]"); got != tc.wantDebug {
				t.Fatalf("debug=%t want=%t: %s", got, tc.wantDebug, &stderr)
			}
			if strings.Contains(stdout, "::debug::") {
				t.Fatalf("debug polluted JSON stdout: %s", stdout)
			}
		})
	}
}

func TestGitHubDebugReportsRetainedCacheEvidence(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_DEBUG", "1")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
	t.Setenv("UNRELATED_SECRET", "must-not-dump-the-environment")
	executions := 0
	p, root := githubCLIFixture(t, []project.Task{{Name: "build", Kind: project.KindOnce, Cache: true, Outputs: project.Outputs{Files: []string{"artifact.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
		executions++
		rt.EmitLogLine("stdout", "debug-retained-output")
		return os.WriteFile(rt.Abs("artifact.txt"), []byte("artifact"), 0o600)
	}}}, "build")
	for _, outcome := range []string{"miss", "hit"} {
		var stderr bytes.Buffer
		result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json", "--max-parallel", "1")
		if err != nil || !result.Success || executions != 1 {
			t.Fatalf("debug changed cached execution: count=%d err=%v stdout=%s stderr=%s", executions, err, stdout, &stderr)
		}
		record, err := instance.LoadRun(root, result.InstanceID, result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if len(record.Attempts) != 1 || record.Attempts[0].CacheKey == "" {
			t.Fatalf("missing retained evidence: %+v", record)
		}
		attempt := record.Attempts[0]
		for _, marker := range []string{
			"debug logging enabled (RUNNER_DEBUG=1)", "max_parallel=1",
			"event=run_started", "event=task_state_changed", "event=cache_" + outcome,
			"run=" + result.RunID, "instance=" + result.InstanceID,
			"attempt=" + attempt.AttemptID, "cache_key=" + attempt.CacheKey,
			"logs_complete=true", "log=" + fmt.Sprintf("%q", attempt.LogPath),
			"cache task=\"build\" outcome=" + outcome, "key_ms=", "read_ms=", "write_ms=",
		} {
			if !strings.Contains(stderr.String(), marker) {
				t.Errorf("missing diagnostic %q: %s", marker, &stderr)
			}
		}
		if strings.Contains(stderr.String(), "must-not-dump-the-environment") {
			t.Fatal("debug exposed an environment value")
		}
		groups := githubCLIParseGroups(t, stderr.String())
		if len(groups) != 1 || (outcome == "miss" && strings.Count(stderr.String(), "debug-retained-output") != 1) || (outcome == "hit" && strings.Contains(stderr.String(), "debug-retained-output")) {
			t.Fatalf("debug duplicated output or interleaved groups: %s", &stderr)
		}
	}
}

func TestGitHubDebugPreservesExplicitOutputControls(t *testing.T) {
	t.Setenv("GITHUB_ACTIONS", "true")
	t.Setenv("RUNNER_DEBUG", "1")
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	for _, progress := range []string{"logs", "states", "quiet"} {
		for _, jsonOut := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/json=%t", progress, jsonOut), func(t *testing.T) {
				p, root := githubCLIFixture(t, []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(_ context.Context, rt *project.Runtime) error {
					rt.EmitLogLine("stdout", "debug-control-output")
					return nil
				}}}, "check")
				args := []string{"--progress", progress, "--details", "issues"}
				if jsonOut {
					args = append(args, "--json")
				}
				var stderr bytes.Buffer
				_, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, args...)
				if err != nil {
					t.Fatalf("run: %v stdout=%s stderr=%s", err, stdout, &stderr)
				}
				if jsonOut {
					var view api.ExecutionView
					if err := json.Unmarshal([]byte(stdout), &view); err != nil || view.Details != "issues" || view.Counts.Nodes != 1 {
						t.Fatalf("debug changed compact JSON: %v %s", err, stdout)
					}
				} else if !strings.Contains(stdout, "details=issues") {
					t.Fatalf("debug changed compact text: %s", stdout)
				}
				if strings.Contains(stderr.String(), "::debug::[devflow]") != (progress != "quiet") || (progress == "quiet" && stderr.Len() != 0) {
					t.Fatalf("debug ignored progress control: %s", &stderr)
				}
				if strings.Contains(stderr.String(), "debug-control-output") != (progress == "logs") {
					t.Fatalf("debug ignored log control: %s", &stderr)
				}
			})
		}
	}
}

func TestGitHubDebugMetadataCannotEmitCommandsOrPromptSecrets(t *testing.T) {
	var output bytes.Buffer
	p := newGitHubPresenter(&output, "logs", "", true)
	p.observe(api.Event{
		Type: api.EventInteractionAck, Task: "unsafe\n::error::child ##[endgroup] %marker",
		RunID: "run", AttemptID: "attempt", PromptID: "prompt", PromptKind: "secret", PromptSecret: true,
		Line: "secret-answer", Error: "secret-error", Prompt: "secret-question", PromptChoices: []string{"secret-choice"},
	})
	p.observe(api.Event{Type: api.EventTaskState, Task: strings.Repeat("🙂", 10000), State: api.StateStarting})
	p.finish(api.RunRecord{}, api.RunResult{})
	debugLines := 0
	for _, line := range strings.Split(output.String(), "\n") {
		if !utf8.ValidString(line) {
			t.Fatalf("debug metadata was truncated within a rune: %q", line)
		}
		if !strings.HasPrefix(line, "::debug::") {
			continue
		}
		debugLines++
		if len(line) > 16*1024 || strings.Contains(strings.TrimPrefix(line, "::debug::"), "::") || strings.Contains(line, "##[") {
			t.Fatalf("unbounded or active command in debug data: %q", line)
		}
	}
	if debugLines == 0 || !strings.Contains(output.String(), "prompt_id=prompt prompt_kind=secret") || !strings.Contains(output.String(), "%25marker") {
		t.Fatalf("missing escaped debug evidence: %s", &output)
	}
	for _, secret := range []string{"secret-answer", "secret-error", "secret-question", "secret-choice"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("debug exposed %q", secret)
		}
	}
}
