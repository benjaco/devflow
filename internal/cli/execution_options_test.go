package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
)

type executionOptionsCLIProject struct {
	name         string
	started      chan struct{}
	prompt       bool
	repairMarker string
}

func (p *executionOptionsCLIProject) Name() string { return p.name }
func (*executionOptionsCLIProject) ConfigureInstance(context.Context, string) (project.InstanceConfig, error) {
	return project.InstanceConfig{}, nil
}
func (*executionOptionsCLIProject) Targets() []project.Target {
	return []project.Target{{Name: "verify", RootTasks: []string{"check"}}}
}
func (p *executionOptionsCLIProject) Tasks() []project.Task {
	return []project.Task{{Name: "check", Kind: project.KindOnce, Run: func(ctx context.Context, rt *project.Runtime) error {
		close(p.started)
		if p.repairMarker != "" {
			if err := os.WriteFile(rt.Abs("frontend/app.txt"), []byte("repaired frontend\n"), 0o644); err != nil {
				return err
			}
			return os.WriteFile(p.repairMarker, []byte("ready"), 0o600)
		}
		if p.prompt {
			_, err := rt.OnPrompt(rt.TaskName, process.PromptRequest{Kind: process.PromptConfirm, Prompt: "Apply change?"})
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}}}
}

type executionCLIResult struct {
	result api.RunResult
	err    error
	stdout string
}

func startExecutionOptionsCLI(t *testing.T, p *executionOptionsCLIProject, wt string, flags ...string) <-chan executionCLIResult {
	t.Helper()
	project.Register(p)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan executionCLIResult, 1)
	go func() {
		defer close(done)
		var stdout bytes.Buffer
		app := &App{Context: ctx, Stdout: &stdout, Stderr: io.Discard}
		args := []string{"run", "verify", "--ci", "--json", "--project", p.Name(), "--worktree", wt}
		err := app.Run(append(args, flags...))
		var result api.RunResult
		if decodeErr := json.Unmarshal(stdout.Bytes(), &result); decodeErr != nil {
			err = errors.Join(err, decodeErr)
		}
		done <- executionCLIResult{result, err, stdout.String()}
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("CLI execution did not clean up")
		}
	})
	return done
}

func executionOptionsProject(t *testing.T, prompt bool) *executionOptionsCLIProject {
	t.Helper()
	isolateJSONContractState(t)
	return &executionOptionsCLIProject{name: "cli-execution-options-" + strings.ReplaceAll(t.Name(), "/", "-"), started: make(chan struct{}), prompt: prompt}
}

func awaitExecutionCLI(t *testing.T, done <-chan executionCLIResult) executionCLIResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(10 * time.Second):
		t.Fatal("CLI execution did not complete")
		return executionCLIResult{}
	}
}

func TestDirectCLIExecutionDeadlineIsRetained(t *testing.T) {
	wt := t.TempDir()
	p := executionOptionsProject(t, false)
	done := startExecutionOptionsCLI(t, p, wt, "--timeout", "2s")
	select {
	case <-p.started:
	case result := <-done:
		t.Fatalf("deadline prevented task from starting: %s (%v)", result.stdout, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("task did not start")
	}
	out := awaitExecutionCLI(t, done)
	if out.err == nil || out.result.Success || out.result.Error == nil || out.result.Error.Code != "deadline_exceeded" {
		t.Fatalf("wrong deadline result: %s (%v)", out.stdout, out.err)
	}
	record, err := instance.LoadRun(wt, out.result.InstanceID, out.result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != api.RunCanceled || record.Deadline.IsZero() || record.Result == nil || record.Result.Error == nil || record.Result.Error.Code != "deadline_exceeded" {
		t.Fatalf("deadline evidence lost: %+v", record)
	}
}

func TestDirectCLIDefaultHeadlessPolicyFailsWithClosedPrompt(t *testing.T) {
	wt := t.TempDir()
	p := executionOptionsProject(t, true)
	out := awaitExecutionCLI(t, startExecutionOptionsCLI(t, p, wt))
	if out.err == nil || out.result.Success || out.result.Error == nil || out.result.Error.Code != "interaction_required" {
		t.Fatalf("default policy hung or misclassified prompt: %s (%v)", out.stdout, out.err)
	}
	prompts, err := instance.ListPrompts(context.Background(), wt, out.result.InstanceID, out.result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(prompts) != 1 || prompts[0].State != api.PromptCancelled {
		t.Fatalf("diagnostic prompt still answerable: %+v", prompts)
	}
}

func TestDirectCLIHeadlessWaitSupportsPublicResponseAndCancellation(t *testing.T) {
	for _, action := range []string{"respond", "cancel"} {
		t.Run(action, func(t *testing.T) {
			wt := t.TempDir()
			p := executionOptionsProject(t, true)
			done := startExecutionOptionsCLI(t, p, wt, "--headless", "wait", "--timeout", "8s")
			id, _, err := instance.IDForWorktree(wt)
			if err != nil {
				t.Fatal(err)
			}
			var pending api.Prompt
			deadline := time.Now().Add(5 * time.Second)
			for pending.ID == "" && time.Now().Before(deadline) {
				result, _, err := runEvidenceCLI(t, "", "runs", "list", "--worktree", wt, "--json")
				if err != nil {
					t.Fatal(err)
				}
				var runs []runSummary
				if err := json.Unmarshal(result["runs"], &runs); err != nil {
					t.Fatal(err)
				}
				if len(runs) > 0 {
					result, _, err = runEvidenceCLI(t, "", "prompts", "list", "--run", runs[0].RunID, "--worktree", wt, "--json")
					if err != nil {
						t.Fatal(err)
					}
					var prompts []api.Prompt
					if err := json.Unmarshal(result["prompts"], &prompts); err != nil {
						t.Fatal(err)
					}
					if len(prompts) > 0 && prompts[0].State == api.PromptPending {
						pending = prompts[0]
					}
				}
				if pending.ID == "" {
					time.Sleep(10 * time.Millisecond)
				}
			}
			if pending.ID == "" {
				t.Fatal("waiting prompt could not be discovered through public commands")
			}
			args := []string{"runs", "cancel", pending.RunID, "--worktree", wt, "--json"}
			if action == "respond" {
				args = []string{"prompts", "respond", pending.ID, "--run", pending.RunID, "--task", pending.Task, "--attempt", pending.AttemptID, "--confirm", "true", "--worktree", wt, "--json"}
			}
			if _, stderr, err := runEvidenceCLI(t, "", args...); err != nil || stderr != "" {
				t.Fatalf("public control failed: %v stderr=%q", err, stderr)
			}
			out := awaitExecutionCLI(t, done)
			if action == "respond" {
				if out.err != nil || !out.result.Success {
					t.Fatalf("response did not complete run: %s (%v)", out.stdout, out.err)
				}
			} else if out.err == nil || out.result.Error == nil || out.result.Error.Code != "operation_cancelled" {
				t.Fatalf("scoped cancellation lost: %s (%v)", out.stdout, out.err)
			}
			record, err := instance.LoadRun(wt, id, pending.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !record.State.Terminal() || record.Result == nil || record.Result.RunID != pending.RunID {
				t.Fatalf("terminal evidence missing: %+v", record)
			}
		})
	}
}

func TestDirectCLICancellationContinuesThroughRepositoryRepair(t *testing.T) {
	p := executionOptionsProject(t, false)
	wt := initRepositoryRepairGitWorktree(t)
	baseline := repairGitText(t, wt, "rev-parse", "HEAD")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	control := t.TempDir()
	p.repairMarker = filepath.Join(control, "dag-done")
	paused := filepath.Join(control, "git-paused")
	source := filepath.Join(control, "git-wrapper.go")
	if err := os.WriteFile(source, []byte(repairCancellationGitWrapper), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(control, "git"+testExeSuffix())
	build := exec.Command("go", "build", "-o", binary, source)
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Git wrapper: %v\n%s", err, out)
	}
	t.Setenv("DEVFLOW_TEST_REAL_GIT", realGit)
	t.Setenv("DEVFLOW_TEST_REPAIR_MARKER", p.repairMarker)
	t.Setenv("DEVFLOW_TEST_GIT_PAUSED", paused)
	t.Setenv("PATH", control+string(os.PathListSeparator)+os.Getenv("PATH"))
	done := startExecutionOptionsCLI(t, p, wt, "--commit-changes", "--commit-path", "frontend", "--commit-message", "must not commit canceled repair", "--push")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(paused); err == nil {
			break
		}
		select {
		case result := <-done:
			t.Fatalf("CLI ended before Git finalization: %s (%v)", result.stdout, result.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("Git did not pause during repository finalization")
		}
		time.Sleep(10 * time.Millisecond)
	}
	payload, _, err := runEvidenceCLI(t, "", "runs", "list", "--worktree", wt, "--json")
	if err != nil {
		t.Fatal(err)
	}
	var runs []runSummary
	if err := json.Unmarshal(payload["runs"], &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].State.Terminal() {
		t.Fatalf("run finalized before repository repair completed: %+v", runs)
	}
	runID := runs[0].RunID
	if _, stderr, err := runEvidenceCLI(t, "", "runs", "cancel", runID, "--worktree", wt, "--json"); err != nil || stderr != "" {
		t.Fatalf("cancel repair: %v stderr=%q", err, stderr)
	}
	var out executionCLIResult
	select {
	case out = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("run cancellation observer ended with DAG; repository Git remained blocked")
	}
	if out.err == nil || out.result.Success || out.result.RunID != runID || out.result.Error == nil || out.result.Error.Code != "operation_cancelled" {
		t.Fatalf("repair cancellation lost: %s (%v)", out.stdout, out.err)
	}
	repository := out.result.RepositoryChanges
	if repository == nil || repository.CommitSHA != "" || repository.PushAttempted {
		t.Fatalf("canceled finalization committed or pushed: %+v", repository)
	}
	// Inspect with the real executable so the paused-wrapper trigger cannot affect assertions.
	cmd := exec.Command(realGit, "rev-parse", "HEAD")
	cmd.Dir = wt
	head, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(head)) != baseline {
		t.Fatalf("canceled repair changed HEAD: %s", head)
	}
	pidBytes, err := os.ReadFile(paused)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil {
		t.Fatal(err)
	}
	if instance.ProcessAlive(pid) {
		t.Fatalf("canceled Git helper remains alive: %d", pid)
	}
	id, _, err := instance.IDForWorktree(wt)
	if err != nil {
		t.Fatal(err)
	}
	record, err := instance.LoadRun(wt, id, runID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != api.RunCanceled || record.Result == nil || record.Result.RunID != runID || record.Result.Error == nil || record.Result.Error.Code != "operation_cancelled" {
		t.Fatalf("repair terminal evidence missing: %+v", record)
	}
}

const repairCancellationGitWrapper = `package main
import("errors";"os";"os/exec";"strconv";"time")
func main(){
 args:=os.Args[1:]
 if len(args)==3 && args[0]=="rev-parse" && args[1]=="--verify" && args[2]=="HEAD^{commit}" {
  if _,err:=os.Stat(os.Getenv("DEVFLOW_TEST_REPAIR_MARKER"));err==nil {
   if err:=os.WriteFile(os.Getenv("DEVFLOW_TEST_GIT_PAUSED"),[]byte(strconv.Itoa(os.Getpid())),0600);err!=nil {os.Exit(92)}
   time.Sleep(20*time.Second)
   os.Exit(93)
  }
 }
 cmd:=exec.Command(os.Getenv("DEVFLOW_TEST_REAL_GIT"),args...)
 cmd.Stdin=os.Stdin;cmd.Stdout=os.Stdout;cmd.Stderr=os.Stderr
 if err:=cmd.Run();err!=nil {var exit *exec.ExitError;if errors.As(err,&exit){os.Exit(exit.ExitCode())};os.Exit(94)}
}
`
