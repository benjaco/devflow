package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/project"
)

func TestGitHubCICompletedHeadersReplaceMissAndDoneLines(t *testing.T) {
	for _, mode := range []struct{ environment, progress string }{
		{"true", "logs"}, {"true", "states"}, {"true", "quiet"}, {"false", "logs"},
	} {
		t.Run(mode.environment+"/"+mode.progress, func(t *testing.T) {
			t.Setenv("GITHUB_ACTIONS", mode.environment)
			t.Setenv("GITHUB_STEP_SUMMARY", "")
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
			t.Setenv("LOCALAPPDATA", filepath.Join(home, "LocalAppData"))
			p, root := githubCLIFixture(t, []project.Task{
				{Name: "generate", Kind: project.KindOnce, Cache: true, Outputs: project.Outputs{Files: []string{"artifact.txt"}}, Run: func(_ context.Context, rt *project.Runtime) error {
					rt.EmitLogLine("stdout", "generated-output-marker")
					return os.WriteFile(rt.Abs("artifact.txt"), []byte("artifact"), 0o600)
				}},
				{Name: "setup", Kind: project.KindOnce, Stamp: true, Run: func(_ context.Context, rt *project.Runtime) error {
					rt.EmitLogLine("stdout", "setup-output-marker")
					return nil
				}},
				{Name: "all", Kind: project.KindGroup, Deps: []string{"generate", "setup"}},
			}, "all")
			for attempt := range 2 {
				var stderr bytes.Buffer
				result, stdout, err := githubCLIRun(context.Background(), p, root, &stderr, "--json", "--progress", mode.progress)
				if err != nil || !result.Success {
					t.Fatalf("run: %v result=%s progress=%s", err, stdout, &stderr)
				}
				output := stderr.String()
				grouped := mode.environment == "true" && mode.progress == "logs"
				if grouped {
					if strings.Contains(output, ": cache miss") || strings.Contains(output, ": done") {
						t.Errorf("redundant miss/done progress outside completed headers:\n%s", output)
					}
					groups := githubCLIParseGroups(t, output)
					if len(groups) != 2 {
						t.Fatalf("want two real attempts, got %+v", groups)
					}
					for _, group := range groups {
						wantMiss := attempt == 0 && strings.HasPrefix(group.title, "generate |")
						if strings.Contains(group.title, " | CACHE MISS") != wantMiss {
							t.Errorf("cache decision missing or falsely attributed: %q", group.title)
						}
					}
					if attempt == 1 && !strings.Contains(output, "::group::generate | CACHED |") {
						t.Errorf("cache hit lost its outcome: %s", output)
					}
				} else if mode.progress == "quiet" {
					if output != "" {
						t.Errorf("quiet emitted progress: %s", output)
					}
				} else if !strings.Contains(output, "setup: done") || attempt == 0 && !strings.Contains(output, "cache miss") && !strings.Contains(output, "cache generate: miss") {
					t.Errorf("ungrouped lifecycle evidence disappeared: %s", output)
				}
			}
		})
	}
}

func TestGitHubHeaderCacheMissSurvivesCompletionAndReconciliation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "attempt.log")
	if err := os.WriteFile(path, []byte("available failure output\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC()
	attempt := api.TaskAttempt{Task: "build", AttemptID: "attempt", State: api.StateFailed, CacheOutcome: "miss", LogPath: path, LastError: "build failed", LogsComplete: true, StartedAt: start, FinishedAt: start.Add(time.Second)}
	for _, source := range []string{"event", "record", "result"} {
		t.Run(source, func(t *testing.T) {
			var output bytes.Buffer
			p := newGitHubPresenter(&output, "logs", "")
			record := api.RunRecord{RunID: "run", Attempts: []api.TaskAttempt{attempt}}
			result := api.RunResult{RunID: "run", Target: "verify"}
			switch source {
			case "event":
				p.observe(api.Event{Type: api.EventTaskAttemptFinished, RunID: "run", Attempt: &attempt})
			case "result":
				record = api.RunRecord{}
				result.Nodes = []api.NodeStatus{{Name: "build", AttemptID: "attempt", State: api.StateFailed, LogPath: path, LastError: "build failed", Cache: &api.CacheTiming{Outcome: "miss"}}}
			}
			p.finish(record, result)
			if strings.Count(output.String(), "::group::build | FAILED |") != 1 || strings.Count(output.String(), "::endgroup::") != 1 || strings.Count(output.String(), " | CACHE MISS") != 1 {
				t.Fatalf("completion lost its recorded failure/cache outcome: %s", &output)
			}
			if strings.Count(output.String(), "::error title=build::") != 1 {
				t.Fatalf("failure annotation missing or duplicated: %s", &output)
			}
		})
	}
}
