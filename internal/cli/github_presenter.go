package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

const githubProgressCapacity = 128

type githubProgress struct {
	line    string
	runID   string
	attempt *api.TaskAttempt
}

type githubAttemptKey struct{ run, task, attempt string }

// The collector never waits for the output writer. A full queue defers evidence
// to final reconciliation, using the run store rather than another copy of logs.
type githubPresenter struct {
	out         io.Writer
	progress    string
	summaryPath string
	pending     chan githubProgress
	done        chan struct{}
	deferred    atomic.Uint64
	shown       map[githubAttemptKey]bool // Owned only by the renderer, then finish.
}

func newGitHubPresenter(out io.Writer, progress, summaryPath string) *githubPresenter {
	p := &githubPresenter{out: out, progress: progress, summaryPath: summaryPath,
		pending: make(chan githubProgress, githubProgressCapacity), done: make(chan struct{}), shown: map[githubAttemptKey]bool{}}
	go func() {
		defer close(p.done)
		for item := range p.pending {
			if item.attempt != nil {
				p.renderAttempt(item.runID, *item.attempt)
			} else {
				p.line(item.line)
			}
		}
	}()
	return p
}

func (p *githubPresenter) enqueue(item githubProgress) {
	if p.progress == "quiet" {
		return
	}
	select {
	case p.pending <- item:
	default:
		p.deferred.Add(1)
	}
}

func (p *githubPresenter) observe(evt api.Event) {
	if p.progress == "quiet" {
		return
	}
	switch evt.Type {
	case api.EventTaskAttemptFinished:
		if evt.Attempt != nil && evt.Attempt.LogsComplete {
			attempt := *evt.Attempt
			attempt.FailureExcerpts = nil
			attempt.CacheKey = ""
			attempt.LastError = strings.Clone(githubBoundedText(attempt.LastError, 2048))
			p.enqueue(githubProgress{runID: evt.RunID, attempt: &attempt})
		}
	case api.EventRunStarted:
		p.message("run %s started", evt.Target)
	case api.EventTaskState:
		state := string(evt.State)
		if evt.State == api.StateRunning && evt.PreviousState != api.StateStarting {
			state = "started"
		}
		if evt.DurationMs > 0 {
			p.message("%s: %s (%s)", evt.Task, state, (time.Duration(evt.DurationMs) * time.Millisecond).String())
		} else {
			p.message("%s: %s", evt.Task, state)
		}
	case api.EventCacheHit:
		p.message("%s: cache hit", evt.Task)
	case api.EventCacheMiss:
		p.message("%s: cache miss", evt.Task)
	case api.EventLogLine:
		// Attempt output is replayed exactly once from its retained file.
		// Setup/run-level messages have no such file and remain live progress.
		if evt.RunID == "" || evt.AttemptID == "" || evt.Task == "" {
			if evt.Task != "" {
				p.message("%s: %s", evt.Task, evt.Line)
			} else {
				p.message("%s", evt.Line)
			}
		}
	}
}

func (p *githubPresenter) message(format string, values ...any) {
	line := fmt.Sprintf(format, values...)
	if len(line) > 2048 {
		line = strings.Clone(line[:2048]) + "…"
	}
	p.enqueue(githubProgress{line: line})
}

// Write is for enclosing run-level progress, such as repository repair. It uses
// the same serialized destination as groups without blocking that operation.
func (p *githubPresenter) Write(data []byte) (int, error) {
	n := len(data)
	for len(data) > 0 {
		end := bytes.IndexByte(data, '\n')
		if end < 0 {
			end = len(data)
		}
		if end > 0 {
			line := bytes.TrimPrefix(data[:end], []byte("[devflow] "))
			p.message("%s", line[:min(len(line), 2048)])
		}
		data = data[min(end+1, len(data)):]
	}
	return n, nil
}

func (p *githubPresenter) line(line string) {
	if len(line) > 2048 {
		line = line[:2048] + "…"
	}
	_, _ = fmt.Fprintf(p.out, "[devflow] %s\n", githubSafeLogText(line))
}

func (p *githubPresenter) diagnostic(err error) {
	if err != nil {
		p.line("presentation: " + err.Error())
	}
}

func (p *githubPresenter) renderAttempt(runID string, attempt api.TaskAttempt) {
	key := githubAttemptKey{run: runID, task: attempt.Task, attempt: attempt.AttemptID}
	if p.progress == "quiet" || key.run == "" || key.task == "" || key.attempt == "" || p.shown[key] {
		return
	}
	p.shown[key] = true
	if p.progress != "states" {
		// Cancellation belongs to execution; its retained output must still drain.
		p.diagnostic(githubWriteAttemptGroup(context.Background(), p.out, attempt))
	}
	p.diagnostic(githubWriteFailureAnnotation(p.out, attempt))
}

// finish follows engine cleanup, collector shutdown and enclosing finalization.
// Only this method writes after the renderer exits, so all groups remain whole.
func (p *githubPresenter) finish(record api.RunRecord, result api.RunResult) {
	close(p.pending)
	<-p.done
	if p.progress != "quiet" {
		if n := p.deferred.Load(); n > 0 {
			p.line(fmt.Sprintf("%d live updates deferred while output was busy; reconciling retained attempts", n))
		}
		for _, attempt := range record.Attempts {
			p.renderAttempt(record.RunID, attempt)
		}
		// Failed evidence reads still leave the returned node snapshot usable.
		// It cannot prove writer completion or supply missing attempt timings.
		for _, node := range result.Nodes {
			p.renderAttempt(result.RunID, api.TaskAttempt{Task: node.Name, AttemptID: node.AttemptID, State: node.State, LogPath: node.LogPath, LastError: node.LastError})
		}
		p.line(fmt.Sprintf("run %s finished success=%t", result.Target, result.Success))
		p.diagnostic(githubWriteSummary(p.out, record, result))
	}
	// Quiet affects progress, not the final summary artifact or execution result.
	p.diagnostic(githubAppendSummary(p.summaryPath, record, result))
}
