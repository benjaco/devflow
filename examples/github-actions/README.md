# Automatic GitHub log groups demo

This adapter uses only Go callbacks and local files. Two independent checks share one cached generator. The normal parallel scheduler runs both checks together; GitHub presentation prints their completed retained logs in separate groups.

Completed headers carry the final status and duration, for example `shared:generate | SUCCESS | 12ms | CACHE MISS`. The default grouped output omits separate `done` and cache-miss lines; `--progress states` retains these messages because it has no log groups.

With Devflow available on `PATH`, run from this directory:

```sh
devflow run verify --ci --json
```

On GitHub Actions, the runner's `GITHUB_ACTIONS=true` environment selects the presentation automatically. Preview this unreleased checkout on macOS/Linux using the repository's source launcher:

```sh
GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY="$PWD/summary.md" ../../devflow run verify --ci --json
```

PowerShell:

```powershell
$env:GITHUB_ACTIONS = 'true'
$env:GITHUB_STEP_SUMMARY = Join-Path $PWD 'summary.md'
$env:DEVFLOW_BOOTSTRAP_ROOT = (Resolve-Path ../..).Path
go build -o "$env:TEMP/devflow-demo.exe" ../../cmd/devflow
& "$env:TEMP/devflow-demo.exe" run verify --ci --json
```

The final JSON stays on stdout. Lifecycle messages, groups, and failure annotations go to stderr. The Markdown table is appended to `summary.md`. A second run demonstrates the generator's cache hit. `--progress states` omits full task logs, and `--progress quiet` suppresses progress. On GitHub Actions, `devflow run failure --ci --json` demonstrates a failed group, an intentional error annotation, and a nonzero exit. For the local macOS/Linux preview, repeat the invocation environment: `GITHUB_ACTIONS=true ../../devflow run failure --ci --json`.

For verbose Devflow diagnostics, set the repository secret/variable
`ACTIONS_STEP_DEBUG` to `true`, or choose **Enable debug logging** when rerunning a
job. GitHub supplies `RUNNER_DEBUG=1`; Devflow then adds bootstrap, execution,
state-transition, cache-key/timing and retained-log diagnostics to stderr using
GitHub debug commands. `ACTIONS_RUNNER_DEBUG` alone enables the runner's separate
diagnostic archive. The final JSON and explicit `--progress`/`--details` choices
are preserved; `--progress quiet` also suppresses debug output. See
[GitHub's debug logging guide](https://docs.github.com/en/actions/how-tos/monitor-workflows/enable-debug-logging).

Local macOS/Linux debug preview:

```sh
GITHUB_ACTIONS=true RUNNER_DEBUG=1 ../../devflow run verify --ci --json
```

For the PowerShell preview above, also set `$env:RUNNER_DEBUG = '1'` before invoking
the binary. Debugging selects diagnostic metadata; it does not dump environment
maps or secret prompt answers, or change child-tool flags.

`workflow-smoke.yml.example` is a manual hosted-rendering check, ready to copy into `.github/workflows/` when wanted. It uses the same single command and does not install an execution coordinator or generate workflow steps. `DEVFLOW_BOOTSTRAP_ROOT` binds an unreleased source-built binary to this checkout for adapter compilation; it is unrelated to presentation selection and is unnecessary for released installations. The example is not an active workflow in this repository. Local tests verify output, evidence and parallel execution; an actual hosted run is needed to inspect GitHub's rendered groups and job summary.
