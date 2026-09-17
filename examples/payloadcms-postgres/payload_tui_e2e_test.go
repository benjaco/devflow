package payloadcmspostgres

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/xpty"

	"github.com/benjaco/devflow/internal/testutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
)

// Launch the public CLI in a real terminal. The test only observes prompts;
// every answer must travel through terminal keys and the actual TUI handlers.
func TestPayloadTUIWorkflowE2E(t *testing.T) {
	if os.Getenv("DEVFLOW_E2E_PAYLOAD") != "1" {
		t.Skip("set DEVFLOW_E2E_PAYLOAD=1 with Node/npm, Go and Docker available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS("testdata/workflow")); err != nil {
		t.Fatal(err)
	}
	// Overlay an ordinary editable collection module onto the pinned Next/Payload
	// fixture. The CLI will compile the adapter and install Node dependencies.
	if err := os.Remove(filepath.Join(root, "payload.config.mjs")); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(root, os.DirFS("testdata/tui")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "devflow.project.go.txt"), filepath.Join(root, "devflow.project.go")); err != nil {
		t.Fatal(err)
	}
	repo := testutil.RepoRoot(t)
	binary := filepath.Join(t.TempDir(), "devflow"+testutil.ExeSuffix())
	if _, err := process.Run(ctx, process.CommandSpec{Name: "go", Args: []string{"build", "-o", binary, "./cmd/devflow"}, Dir: repo}); err != nil {
		t.Fatal(err)
	}
	// Share the compiler/module caches while keeping Devflow's runtime cache and
	// instance index isolated. A private module cache also leaves read-only files.
	cacheJSON, err := exec.CommandContext(ctx, "go", "env", "-json", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	if err := json.Unmarshal(cacheJSON, &env); err != nil {
		t.Fatal(err)
	}
	npmCache, err := exec.CommandContext(ctx, "npm", "config", "get", "cache").Output()
	if err != nil {
		t.Fatal(err)
	}
	env["npm_config_cache"] = strings.TrimSpace(string(npmCache))
	isolatePayloadUserCache(t)
	mgr := database.New()
	db := mgr.Desired(fmt.Sprintf("payload-tui-%d", time.Now().UnixNano()), database.Config{HostPort: payloadFreePort(t), Database: "payload_tui", User: "devflow", Password: "devflow"})
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := mgr.DestroyRuntime(cleanup, db, true); err != nil {
			t.Error(err)
		}
	})
	if err := mgr.EnsureRuntime(ctx, db); err != nil {
		t.Fatal(err)
	}
	if err := mgr.WaitReady(ctx, db, 45*time.Second); err != nil {
		t.Fatal(err)
	}
	id, root, err := instance.IDForWorktree(root)
	if err != nil {
		t.Fatal(err)
	}
	env["DEVFLOW_BOOTSTRAP_ENTRY"], env["DEVFLOW_BOOTSTRAP_ROOT"] = "1", repo
	env["DATABASE_URL"], env["TERM"] = db.URL, "xterm-256color"
	terminalLog := filepath.Join(t.TempDir(), "terminal.log")
	t.Log("bootstrap fresh Next/Payload project through bare devflow in an owned terminal")
	tui := startPayloadTUITerminal(t, ctx, binary, root, terminalLog, env)
	t.Cleanup(func() {
		if t.Failed() {
			for _, path := range []string{terminalLog, instance.LogPath(root, id, "daemon"), instance.LogPath(root, id, "tui")} {
				data, _ := os.ReadFile(path)
				t.Logf("%s:\n%s", filepath.Base(path), data[max(0, len(data)-(24<<10)):])
			}
		}
		// Also runs on assertion failure, when a modal may still own the keyboard.
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var pids []int
		if daemon, err := instance.LoadDaemon(root, id); err == nil && daemon.PID > 0 {
			pids = append(pids, daemon.PID)
		}
		if state, err := instance.LoadStatus(root, id); err == nil {
			for _, node := range state.Nodes {
				if node.PID > 0 {
					pids = append(pids, node.PID)
				}
			}
		}
		if _, err := process.Run(cleanup, process.CommandSpec{Name: binary, Args: []string{"stop", "--all", "--json"}, Dir: root, Env: env}); err != nil {
			t.Errorf("stop fixture: %v", err)
		}
		tui.stop(t)
		// A stop acknowledgment can precede the daemon's final log write.
		deadline := time.Now().Add(5 * time.Second)
		for _, pid := range pids {
			for instance.ProcessAlive(pid) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if instance.ProcessAlive(pid) {
				t.Errorf("fixture process %d survived stop", pid)
			}
		}
	})

	var before api.NodeStatus
	waitPayloadTUI(t, ctx, tui, terminalLog, "initial table and watcher readiness", 3*time.Minute, func() bool {
		state, err := instance.LoadStatus(root, id)
		if err != nil {
			return false
		}
		before = state.Nodes["app"]
		_, err = os.Stat(instance.FlushWatchReadyPath(root, id))
		return err == nil && before.Ready && before.State == api.StateRunning && state.Nodes["install"].State == api.StateDone
	})
	if _, err := os.Stat(filepath.Join(root, ".devflow", "bin", "devflow-local"+testutil.ExeSuffix())); err != nil {
		t.Fatalf("CLI did not bootstrap the local adapter: %v", err)
	}
	if _, err := mgr.ExecSQL(ctx, db, `INSERT INTO posts (title,legacy) VALUES ('preserve this title','remove this legacy value');`); err != nil {
		t.Fatal(err)
	}
	daemon, err := instance.LoadDaemon(root, id)
	if err != nil || daemon.PID == 0 {
		t.Fatalf("missing real daemon: %+v %v", daemon, err)
	}
	servicePIDs := []int{before.PID}

	t.Log("rename Posts.title, wait for the rendered TUI menu, choose rename with Down/Enter")
	source, err := os.ReadFile(filepath.Join(root, "collections", "Posts.mjs"))
	if err != nil {
		t.Fatal(err)
	}
	renamed := strings.Replace(string(source), "name: 'title'", "name: 'headline'", 1)
	mark := payloadTUILogSize(t, terminalLog)
	writePayloadFile(t, root, "collections/Posts.mjs", renamed)
	prompt := waitPayloadTUIPrompt(t, ctx, tui, terminalLog, root, id, "select")
	if len(prompt.Choices) != 2 || !strings.Contains(prompt.Choices[1], "title › headline rename column") {
		t.Fatalf("unexpected rename choices: %+v", prompt)
	}
	waitPayloadTUI(t, ctx, tui, terminalLog, "visible rename menu", time.Minute, func() bool {
		return payloadTUIContains(terminalLog, mark, "Choose an option", "rename column")
	})
	assertPayloadSQL(t, ctx, mgr, db, `SELECT title FROM posts;`, "preserve this title")
	if err := tui.WriteString("\x1b[B\r"); err != nil {
		t.Fatal(err)
	}
	afterRename := waitPayloadTUIReadyAfter(t, ctx, tui, terminalLog, root, id, prompt, before.AttemptID)
	servicePIDs = append(servicePIDs, afterRename.PID)
	assertPayloadSQL(t, ctx, mgr, db, `SELECT headline,legacy FROM posts;`, "preserve this title", "remove this legacy value")
	assertPayloadSQL(t, ctx, mgr, db, `SELECT CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='posts' AND column_name='title') THEN 'old column remains' ELSE 'old column removed' END;`, "old column removed")

	t.Log("remove populated Posts.legacy and accept the rendered warning with Left/Enter")
	mark = payloadTUILogSize(t, terminalLog)
	writePayloadFile(t, root, "collections/Posts.mjs", strings.Replace(renamed, "    { name: 'legacy', type: 'text' },\n", "", 1))
	prompt = waitPayloadTUIPrompt(t, ctx, tui, terminalLog, root, id, "confirm")
	if !strings.Contains(prompt.Message, "legacy") || !strings.Contains(prompt.Message, "DATA LOSS") {
		t.Fatalf("warning lost context: %+v", prompt)
	}
	waitPayloadTUI(t, ctx, tui, terminalLog, "visible confirmation buttons", time.Minute, func() bool {
		return payloadTUIContains(terminalLog, mark, "Yes", "No", "Accept warnings")
	})
	assertPayloadSQL(t, ctx, mgr, db, `SELECT legacy FROM posts;`, "remove this legacy value")
	// The confirmation starts on No. Move explicitly to Yes before submitting.
	if err := tui.WriteString("\x1b[D\r"); err != nil {
		t.Fatal(err)
	}
	afterDrop := waitPayloadTUIReadyAfter(t, ctx, tui, terminalLog, root, id, prompt, afterRename.AttemptID)
	servicePIDs = append(servicePIDs, afterDrop.PID)
	assertPayloadSQL(t, ctx, mgr, db, `SELECT headline FROM posts;`, "preserve this title")
	assertPayloadSQL(t, ctx, mgr, db, `SELECT CASE WHEN EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='posts' AND column_name='legacy') THEN 'legacy remains' ELSE 'legacy removed' END;`, "legacy removed")

	t.Log("quit the TUI and require its owned daemon and every Next service to exit")
	if err := tui.WriteString("q"); err != nil {
		t.Fatal(err)
	}
	waitPayloadTUI(t, ctx, nil, terminalLog, "TUI and owned processes shutdown", 20*time.Second, func() bool {
		if tui.Alive() || instance.ProcessAlive(daemon.PID) {
			return false
		}
		for _, pid := range servicePIDs {
			if instance.ProcessAlive(pid) {
				return false
			}
		}
		return true
	})
	if err := tui.Wait(); err != nil {
		t.Fatalf("TUI exit: %v", err)
	}
}

func waitPayloadTUI(t *testing.T, ctx context.Context, tui *payloadTUITerminal, log, description string, timeout time.Duration, ready func() bool) {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if ready() {
			return
		}
		if tui != nil && !tui.Alive() {
			t.Fatalf("TUI exited while waiting for %s: %v\n%s", description, tui.Wait(), payloadTUITail(log))
		}
		select {
		case <-wait.Done():
			t.Fatalf("waiting for %s: %v\n%s", description, wait.Err(), payloadTUITail(log))
		case <-ticker.C:
		}
	}
}

func waitPayloadTUIPrompt(t *testing.T, ctx context.Context, tui *payloadTUITerminal, log, root, id, kind string) api.Prompt {
	t.Helper()
	var found api.Prompt
	waitPayloadTUI(t, ctx, tui, log, "pending "+kind+" prompt", time.Minute, func() bool {
		state, err := instance.LoadStatus(root, id)
		if err != nil || state.Nodes["app"].RunID == "" {
			return false
		}
		prompts, err := instance.ListPrompts(ctx, root, id, state.Nodes["app"].RunID)
		if err != nil {
			return false
		}
		for _, prompt := range prompts {
			if prompt.Task == "app" && prompt.Kind == kind && prompt.State == api.PromptPending {
				found = prompt
				return true
			}
		}
		return false
	})
	return found
}

func waitPayloadTUIReadyAfter(t *testing.T, ctx context.Context, tui *payloadTUITerminal, log, root, id string, prompt api.Prompt, previous string) api.NodeStatus {
	t.Helper()
	var node api.NodeStatus
	waitPayloadTUI(t, ctx, tui, log, "answered prompt and restarted Payload readiness", time.Minute, func() bool {
		state, err := instance.LoadStatus(root, id)
		if err != nil {
			return false
		}
		node = state.Nodes["app"]
		if !node.Ready || node.State != api.StateRunning || node.AttemptID == previous || node.AttemptID != prompt.AttemptID {
			return false
		}
		prompts, err := instance.ListPrompts(ctx, root, id, prompt.RunID)
		if err != nil {
			return false
		}
		for _, item := range prompts {
			if item.ID == prompt.ID {
				return item.State == api.PromptAnswered
			}
		}
		return false
	})
	return node
}

var payloadTUIEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()][A-Z0-9])`)

func payloadTUIOutput(path string, offset int) string {
	data, _ := os.ReadFile(path)
	return payloadTUIEscape.ReplaceAllString(string(data[min(offset, len(data)):]), "")
}

func payloadTUITail(path string) string {
	text := payloadTUIOutput(path, 0)
	return text[max(0, len(text)-(24<<10)):]
}

func payloadTUIContains(path string, offset int, labels ...string) bool {
	// tcell can paint spaces with cursor movement. Match the emitted glyphs;
	// the prompt store and SQL assertions establish identity and the outcome.
	text := strings.Join(strings.Fields(payloadTUIOutput(path, offset)), "")
	for _, label := range labels {
		if !strings.Contains(text, strings.Join(strings.Fields(label), "")) {
			return false
		}
	}
	return true
}

func payloadTUILogSize(t *testing.T, path string) int {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return int(info.Size())
}

type payloadTUITerminal struct {
	pty  xpty.Pty
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func startPayloadTUITerminal(t *testing.T, ctx context.Context, binary, root, log string, env map[string]string) *payloadTUITerminal {
	t.Helper()
	pty, err := xpty.NewPty(120, 30)
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(log)
	if err != nil {
		_ = pty.Close()
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, binary)
	cmd.Dir = root
	merged := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		merged[key] = value
	}
	for key, value := range env {
		merged[key] = value
	}
	for key, value := range merged {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	// A full-screen TUI needs a controlling terminal, as provided by a shell.
	// Merely connecting its stdin/stdout to a PTY does not supply /dev/tty.
	preparePayloadTUITerminal(cmd)
	if err := pty.Start(cmd); err != nil {
		_ = pty.Close()
		_ = out.Close()
		t.Fatal(err)
	}
	if unix, ok := pty.(*xpty.UnixPty); ok {
		_ = unix.Slave().Close()
	}
	terminal := &payloadTUITerminal{pty: pty, cmd: cmd, done: make(chan struct{})}
	readDone := make(chan struct{})
	go func() {
		// A Unix PTY ends with EIO after the child closes its slave.
		_, _ = io.Copy(out, pty)
		close(readDone)
	}()
	go func() {
		terminal.err = xpty.WaitProcess(ctx, cmd)
		if _, unix := pty.(*xpty.UnixPty); !unix {
			_ = pty.Close()
		}
		<-readDone
		_ = pty.Close()
		_ = out.Close()
		close(terminal.done)
	}()
	return terminal
}

func (p *payloadTUITerminal) WriteString(keys string) error {
	_, err := io.WriteString(p.pty, keys)
	return err
}

func (p *payloadTUITerminal) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

func (p *payloadTUITerminal) Wait() error {
	<-p.done
	return p.err
}

func (p *payloadTUITerminal) stop(t *testing.T) {
	t.Helper()
	if p.Alive() {
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(10 * time.Second):
		t.Error("terminal process/output did not join")
	}
}
