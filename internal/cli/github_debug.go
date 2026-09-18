package cli

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/benjaco/devflow/pkg/api"
)

func githubWriteDebug(out io.Writer, format string, values ...any) {
	line := githubBoundedText(fmt.Sprintf(format, values...), 2048)
	// Only this deliberate command is active. Metadata may contain historical
	// runner commands too, including the legacy syntax recognized within a line.
	_, _ = fmt.Fprintf(out, "::debug::[devflow] %s\n", githubCommandData(githubSafeLogText(line)))
}

func (p *githubPresenter) debugMessage(format string, values ...any) {
	if p.debug && p.progress != "quiet" {
		p.enqueue(githubProgress{debug: true, line: strings.Clone(githubBoundedText(fmt.Sprintf(format, values...), 2048))})
	}
}

// Called only by the renderer, or after it has been joined during finalization.
func (p *githubPresenter) debugLine(format string, values ...any) {
	if p.debug && p.progress != "quiet" {
		githubWriteDebug(p.out, format, values...)
	}
}

func (p *githubPresenter) observeDebug(evt api.Event) {
	if !p.debug || evt.Type == api.EventLogLine || evt.Type == api.EventTaskAttemptFinished {
		return
	}
	// Select metadata explicitly: whole events can contain secret prompt answers
	// and future payloads. Task output already has its retained replay path.
	p.debugMessage("event=%s time=%s run=%s instance=%s task=%q attempt=%s mode=%s state=%s previous=%s duration_ms=%d pid=%d cache_key=%s prompt_id=%s prompt_kind=%s",
		evt.Type, evt.TS, evt.RunID, evt.InstanceID, evt.Task, evt.AttemptID, evt.Mode, evt.State, evt.PreviousState, evt.DurationMs, evt.PID, evt.CacheKey, evt.PromptID, evt.PromptKind)
}

func (p *githubPresenter) debugResult(record api.RunRecord, result api.RunResult) {
	if !p.debug {
		return
	}
	p.debugLine("result run=%s instance=%s target=%q mode=%s success=%t duration_ms=%d nodes=%d attempts=%d cache_hits=%d cache_misses=%d", result.RunID, result.InstanceID, result.Target, result.Mode, result.Success, result.DurationMs, len(result.Nodes), len(record.Attempts), len(result.CacheHits), len(result.CacheMisses))
	p.debugLine("record state=%s owner_pid=%d graph_digest=%s adapter_version=%s started_at=%s finished_at=%s deadline=%s", record.State, record.OwnerPID, record.GraphDigest, record.AdapterVersion, githubDebugTime(record.StartedAt), githubDebugTime(record.FinishedAt), githubDebugTime(record.Deadline))
	for _, node := range result.Nodes {
		p.debugLine("node task=%q kind=%s state=%s attempt=%s duration_ms=%d ready=%t pid=%d generation=%d last_run_key=%s log=%q", node.Name, node.Kind, node.State, node.AttemptID, node.DurationMs, node.Ready, node.PID, node.Generation, node.LastRunKey, node.LogPath)
		if cache := node.Cache; cache != nil {
			p.debugLine("cache task=%q outcome=%s key_ms=%d read_ms=%d write_ms=%d total_ms=%d manifest_validation_ms=%d manifest_components=%d local_inputs_changed=%t", node.Name, cache.Outcome, cache.KeyDurationMs, cache.ReadDurationMs, cache.WriteDurationMs, cache.TotalDurationMs, cache.ManifestValidationMs, len(cache.ManifestComponents), cache.LocalInputsChangedFromManifest)
		}
	}
	if manifest := result.CacheKeyManifest; manifest != nil {
		p.debugLine("cache_manifest path=%q validated=%t validation_ms=%d reused_tasks=%d reused_components=%d local_input_changed_tasks=%d", manifest.Path, manifest.Validated, manifest.ValidationDurationMs, len(manifest.ReusedTasks), manifest.ReusedComponents, len(manifest.LocalInputChangedTasks))
	}
}

func githubDebugTime(value time.Time) string {
	if value.IsZero() {
		return "unavailable"
	}
	return value.UTC().Format(time.RFC3339Nano)
}
