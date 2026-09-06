package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/benjaco/devflow/internal/clierror"
	"github.com/benjaco/devflow/internal/executionconflict"
	"github.com/benjaco/devflow/pkg/api"
)

var errFlagsDiscovered = errors.New("flags discovered")

type presentedError struct{ error }

func (presentedError) Presented() bool { return true }
func (e presentedError) Unwrap() error { return e.error }

// ReportError is shared by installed and generated entrypoints. A failed child
// or a JSON result already owns its diagnostics; printing again loses that boundary.
func ReportError(w io.Writer, err error) {
	var presented interface{ Presented() bool }
	if err != nil && !(errors.As(err, &presented) && presented.Presented()) {
		_, _ = fmt.Fprintln(w, err)
	}
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 1
}

func (a *App) context() context.Context {
	if a.Context != nil {
		return a.Context
	}
	return context.Background()
}

func (a *App) Run(args []string) error {
	call := *a
	ctx, stop := signal.NotifyContext(a.context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	call.Context = ctx
	// Discover the real command flag set without running resolution or callbacks.
	// This keeps early JSON recognition in sync with the command's value flags.
	probe := call
	probe.discoverFlags = true
	probe.Stdout, probe.Stderr = io.Discard, io.Discard
	var probeErr error
	if len(args) != 0 {
		probeErr = probe.dispatch(args)
	}
	call.jsonOutput = jsonRequested(args, probe.flagSet)
	var outputOptionsErr error
	if errors.Is(probeErr, errFlagsDiscovered) {
		outputOptionsErr = call.configureResultOutput(probe.flagSet)
	}
	if probe.flagSet != nil {
		switch probe.flagSet.Name() {
		case "validate":
			call.localChildOwnsExecution = true
		case "run":
			if ci := probe.flagSet.Lookup("ci"); ci != nil {
				call.localChildOwnsExecution = ci.Value.String() == "true"
			}
		}
	}
	worktreeFlag := ""
	if probe.flagSet != nil {
		if f := probe.flagSet.Lookup("worktree"); f != nil {
			worktreeFlag = f.Value.String()
		}
	}
	var worktree string
	var worktreeErr error
	if !isProjectlessCommand(args) {
		worktree, worktreeErr = resolveWorktree(worktreeFlag)
	}
	var err error
	switch {
	case probeErr != nil && !errors.Is(probeErr, errFlagsDiscovered):
		// Reparse for ordinary help/usage output only in the text path.
		if !call.jsonOutput {
			return call.dispatch(args)
		}
		err = clierror.Wrap(probeErr, "invalid_arguments", "parsing")
	case ctx.Err() != nil:
		err = ctx.Err()
	case outputOptionsErr != nil:
		err = outputOptionsErr
	case worktreeErr != nil:
		err = clierror.Wrap(worktreeErr, "invalid_worktree", "resolution")
	case shouldExecLocalProject(args, worktree):
		err = call.execLocalProject(args, worktree)
	default:
		err = call.dispatch(args)
	}
	if !call.jsonOutput {
		return err
	}
	var presented interface{ Presented() bool }
	if errors.As(err, &presented) && presented.Presented() {
		return err
	}
	if call.outputFailed {
		return err
	}
	call.result = call.resultView(call.result)
	if err != nil {
		payload := map[string]json.RawMessage{}
		if call.result != nil {
			data, marshalErr := json.Marshal(call.result)
			if marshalErr != nil {
				return marshalErr
			}
			if unmarshalErr := json.Unmarshal(data, &payload); unmarshalErr != nil {
				return unmarshalErr
			}
		}
		if payload == nil {
			payload = map[string]json.RawMessage{}
		}
		payload["success"] = json.RawMessage("false")
		failure := clierror.Describe(err, "operation_failed", "execution")
		truncated := api.ExecutionTruncation{}
		if view, ok := call.result.(*api.ExecutionView); ok {
			truncated = view.Truncated
		}
		if call.compactOutput() {
			sample := viewSampler{remaining: 2048, truncated: &truncated.Text}
			failure.Message = sample.text(failure.Message)
			payload["details"], _ = json.Marshal(call.details)
		}
		payload["error"], _ = json.Marshal(failure)
		if detail := executionconflict.Details(err); detail != nil {
			if call.compactOutput() {
				copy := *detail
				sample := viewSampler{remaining: 2048, truncated: &truncated.Text}
				copy.Worktree, copy.Target = sample.identity(copy.Worktree), sample.identity(copy.Target)
				detail = &copy
			}
			payload["resourceConflict"], _ = json.Marshal(detail)
		}
		if call.compactOutput() {
			payload["truncated"], _ = json.Marshal(truncated)
		}
		if call.jsonStream {
			if writeErr := writeJSONLine(call.Stdout, payload); writeErr != nil {
				return writeErr
			}
		} else if writeErr := writeJSON(call.Stdout, payload); writeErr != nil {
			return writeErr
		}
		return presentedError{err}
	}
	if call.result != nil {
		return writeJSON(call.Stdout, call.result)
	}
	return nil
}

// Finite command results are held as values until the command returns, allowing
// one error document to retain partial evidence without buffering log streams.
func (a *App) writeResult(value any) error {
	if a.jsonOutput {
		a.result = value
		return nil
	}
	return writeJSON(a.Stdout, value)
}

func (a *App) parseFlags(fs *flag.FlagSet, args []string) error {
	a.flagSet = fs
	if a.jsonOutput {
		fs.SetOutput(io.Discard)
	}
	flags, positional := splitFlags(args, fs)
	if err := fs.Parse(append(flags, positional...)); err != nil {
		return clierror.Wrap(err, "invalid_arguments", "parsing")
	}
	maxArgs := 0
	switch fs.Name() {
	case "run", "watch", "flush", "restart", "logs", "validate", "graph show", "migration create", "runs show", "runs cancel", "prompts respond":
		maxArgs = 1
	case "action run":
		maxArgs = 2
	}
	if fs.NArg() > maxArgs {
		return clierror.Wrap(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")), "invalid_arguments", "parsing")
	}
	if a.discoverFlags {
		return errFlagsDiscovered
	}
	return nil
}

func valueFlag(fs *flag.FlagSet, name string) bool {
	if fs == nil {
		return false
	}
	f := fs.Lookup(name)
	if f == nil {
		return false
	}
	boolean, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolean.IsBoolFlag()
}

func jsonRequested(args []string, fs *flag.FlagSet) bool {
	enabled := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if name == "json" {
			enabled = true
			if hasValue {
				if parsed, err := strconv.ParseBool(value); err == nil {
					enabled = parsed
				}
			}
		} else if !hasValue && valueFlag(fs, name) {
			i++
		}
	}
	return enabled
}

// Go's flag parser stops at a positional argument. Separate known value flags
// first so JSON can appear on either side of a target, while -- remains literal.
func splitFlags(args []string, fs *flag.FlagSet) (flags, positional []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positional = append(positional, args[i:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positional = append(positional, arg)
			continue
		}
		flags = append(flags, arg)
		name, _, hasValue := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if !hasValue && valueFlag(fs, name) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	// The delimiter also protects ordinary positional values beginning with '-'.
	if len(positional) > 0 && positional[0] != "--" {
		positional = append([]string{"--"}, positional...)
	}
	return flags, positional
}
