# Exit diagnostic verification

DevFlow records observations at its existing TUI, bootstrap and daemon
boundaries. It uses stock tcell/tview for terminal input, decoding and lifecycle.
The existing `tui.log` and `daemon.log` remain the evidence sources:

```sh
devflow logs tui --tail 200 --json
devflow logs daemon --tail 200 --json
```

These commands bypass adapter compilation, but still select the project's CLI
version. If version selection itself fails, read the retained files directly at
`.devflow/logs/<instance-id>/tui.log` and `daemon.log`.

## Checks

Run `go test ./...`, `go test -race ./...` and `go vet ./...`. The existing
Windows CI job executes native child-process tests; cross-compilation alone
does not verify Windows exit status or console interruption.

Focused regressions should establish:

- Decoded Escape/Ctrl+C and the handler's decision are separate observations.
  Modal-consumed Escape does not become an application exit. Printable input
  and prompt answers are excluded; `q` is recorded only as an executed binding.
- Backend identity comes from public tcell APIs. Application-loop return is
  recorded before owned cleanup, with no claim about user intent.
- The initiating failure survives later cleanup. Existing panic/runtime crash
  output remains available in the TUI log.
- Windows bootstrap retains the actual child status, including unsigned decimal
  and hex, and separately records parent cancellation/control/termination.
  Output ownership and native exit behavior remain unchanged.
- The TUI's stop request ID connects to the daemon's caller/request record before
  blocking cleanup; cleanup and shutdown outcomes retain that ID.
- Diagnostic-log inspection changes no services or retained evidence and keeps
  the existing JSONL output contract.

For an interactive smoke, use the harmless
[existing demo](../examples/github-actions/README.md) in a disposable directory.
Launch bare `devflow` there. Open help, press Escape and verify the TUI remains
active, then quit with `q`.
Inspect both logs and confirm only the fixture-owned daemon stops. Compare IDE
terminals with Windows Terminal/PowerShell when investigating native behavior.

## Limits

A decoded key does not prove a physical keypress or user intent. Native Windows
key records, internal parser decisions/timeouts and reader failures hidden by
tcell are unavailable through these hooks. Deeper observation needs upstream
support rather than a DevFlow-maintained terminal implementation.

If the process and its observer both disappear, these logs cannot supply an
unknown exit status, stack or cause. There is no session index or automatic
incomplete-session diagnosis. Logs append across restarts without new retention
machinery. Raw crash output can contain sensitive data; review it before sharing.
Keep personal paths, consumer-project details and copied incident identifiers
out of contributor documentation.
