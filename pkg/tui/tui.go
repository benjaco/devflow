package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/daemon"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/watch"
)

type Options struct {
	Worktree         string
	InstanceID       string
	StopDaemonOnExit bool
	Output           io.Writer
}

type snapshot struct {
	instance     *api.Instance
	state        *instance.State
	nodes        []api.NodeStatus
	supervisor   *api.SupervisorStatus
	urls         map[string]string
	logTitle     string
	logPath      string
	logLines     []string
	logStartLine int
	logEndLine   int
	logTruncated bool
	prisma       []prismaSnapshotSummary
	prismaErr    string
	prismaCfg    tuiPrismaConfig
	prismaDev    *database.PrismaDevelopmentStatus
	prismaDevErr string
}

type prismaSnapshotSummary struct {
	Key             string
	CreatedAt       time.Time
	SchemaHash      string
	BaseFingerprint string
	MigrationNames  []string
}

type tuiPrismaConfig struct {
	project.PrismaConfig
	Source    string
	Available bool
}

type dashboard struct {
	root              string
	instanceID        string
	eventsPath        string
	app               *tview.Application
	pages             *tview.Pages
	header            *tview.TextView
	tasks             *tview.Table
	logs              *tview.TextView
	footer            *tview.TextView
	layout            *tview.Flex
	tooSmall          *tview.TextView
	showSupervisorLog bool
	showDatabasePanel bool
	selectedName      string
	allNodes          []api.NodeStatus
	currentNodes      []api.NodeStatus
	attentionOnly     bool
	statusMessage     string
	statusAt          time.Time
	busy              bool
	eventOffset       int64
	activePromptID    string
	activeInput       bool
	lastLogPath       string
	lastLogLines      []string
	lastSnapshot      snapshot
	renderedLogKey    string
	logScrollKey      string
	logScrollRow      int
	logScrollCol      int
	logFollowing      bool
	logLineLimit      int
	helpOpen          bool
	lifecycleModal    bool
	compactLevel      int
}

const (
	fallbackRefreshInterval = 2 * time.Second
	databasePanelTitle      = "database / prisma"
	supervisorLogTitle      = "supervisor log"
)

var callDaemonForTUI = func(ctx context.Context, root string, req daemon.Request, onEvent func(api.Event)) (daemon.Response, error) {
	client, _, err := daemon.Ensure(ctx, root, "")
	if err != nil {
		return daemon.Response{}, err
	}
	return client.Call(ctx, req, onEvent)
}

var stopDaemonForTUI = func(ctx context.Context, client *daemon.Client) error {
	if client == nil {
		return nil
	}
	_, err := client.Call(ctx, daemon.Request{Action: daemon.ActionStop, All: true})
	return err
}

func Run(opts Options) error {
	root, id, err := resolveInstance(opts.Worktree, opts.InstanceID)
	if err != nil {
		return err
	}
	client, started, err := daemon.Ensure(context.Background(), root, "")
	if err != nil {
		return err
	}
	stopDaemonOnExit := opts.StopDaemonOnExit || started
	d := newDashboard(root, id)
	if err := d.refresh(); err != nil {
		_ = maybeStopDaemonForTUI(client, stopDaemonOnExit)
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	go d.daemonEventLoop(ctx, client)
	go d.eventLoop(ctx)
	go d.fallbackRefreshLoop(ctx)
	runErr := d.app.Run()
	cancel()
	_ = maybeStopDaemonForTUI(client, stopDaemonOnExit)
	if !stopDaemonOnExit {
		writeDetachedQuitMessage(opts.Output, root, id)
	}
	return runErr
}

func writeDetachedQuitMessage(output io.Writer, root, instanceID string) {
	inst, err := instance.Load(root, instanceID)
	if err != nil || !inst.LastRun.Detached {
		return
	}
	if output == nil {
		output = os.Stdout
	}
	_, _ = fmt.Fprintf(output, "DevFlow run %s (%s) remains active. Inspect: devflow status --worktree %q --json  Stop: devflow stop --worktree %q --all\n", inst.LastRun.Target, instanceID, root, root)
}

func maybeStopDaemonForTUI(client *daemon.Client, enabled bool) error {
	if !enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return stopDaemonForTUI(ctx, client)
}

func newDashboard(root, instanceID string) *dashboard {
	d := &dashboard{
		root:         root,
		instanceID:   instanceID,
		eventsPath:   instance.EventsPath(root, instanceID),
		app:          tview.NewApplication(),
		header:       tview.NewTextView(),
		tasks:        tview.NewTable(),
		logs:         tview.NewTextView(),
		footer:       tview.NewTextView(),
		tooSmall:     tview.NewTextView(),
		logFollowing: true,
		logLineLimit: 200,
	}

	d.header.
		SetDynamicColors(true).
		SetWrap(false).
		SetBorder(true).
		SetTitle(" Instance ")

	d.tasks.
		SetBorders(false).
		SetSelectable(true, false).
		SetFixed(1, 0).
		SetBorder(true).
		SetTitle(" Tasks ")
	d.tasks.SetSelectionChangedFunc(func(row, _ int) {
		if row <= 0 || row-1 >= len(d.currentNodes) {
			return
		}
		d.selectedName = d.currentNodes[row-1].Name
		d.logFollowing = true
		d.logLineLimit = 200
		d.updateLogs()
	})

	d.logs.
		SetDynamicColors(true).
		SetWrap(false).
		SetBorder(true).
		SetTitle(" Logs ")
	d.logs.SetScrollable(true)

	d.tasks.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseScrollUp:
			d.scrollLogs(-3)
			return tview.MouseConsumed, nil
		case tview.MouseScrollDown:
			d.scrollLogs(3)
			return tview.MouseConsumed, nil
		default:
			return action, event
		}
	})
	d.logs.SetMouseCapture(func(action tview.MouseAction, event *tcell.EventMouse) (tview.MouseAction, *tcell.EventMouse) {
		switch action {
		case tview.MouseScrollUp:
			d.app.SetFocus(d.logs)
			d.logFollowing = false
			d.rememberLogScrollDelta(-1)
		case tview.MouseScrollDown:
			d.app.SetFocus(d.logs)
			d.logFollowing = false
			d.rememberLogScrollDelta(1)
		case tview.MouseLeftClick:
			d.app.SetFocus(d.logs)
		}
		return action, event
	})

	d.footer.
		SetDynamicColors(true).
		SetWrap(false).
		SetBorder(true).
		SetTitle(" Keys ")

	d.layout = tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.header, 7, 0, false).
		AddItem(d.tasks, 0, 2, true).
		AddItem(d.logs, 0, 3, false).
		AddItem(d.footer, 5, 0, false)
	d.tooSmall.SetTextAlign(tview.AlignCenter).SetBorder(true).SetTitle(" DevFlow ")
	d.pages = tview.NewPages().
		AddPage("main", d.layout, true, true).
		AddPage("too_small", d.tooSmall, true, false)

	d.setStatus("[green]ready")

	d.app.EnableMouse(true)
	d.app.SetRoot(d.pages, true)
	d.app.SetFocus(d.logs)
	d.app.SetInputCapture(d.handleKeys)
	d.app.SetBeforeDrawFunc(func(screen tcell.Screen) bool {
		width, height := screen.Size()
		d.applyResponsiveLayout(width, height)
		return false
	})
	d.updateFocusTreatment()
	return d
}

func (d *dashboard) applyResponsiveLayout(width, height int) {
	d.updateFocusTreatment()
	if width < 40 || height < 10 {
		d.tooSmall.SetText(fmt.Sprintf("Terminal too small (%dx%d)\nResize to at least 40x10. Press ? for help or q to quit.", width, height))
		d.pages.ShowPage("too_small")
		return
	}
	d.pages.HidePage("too_small")
	d.pages.ShowPage("main")
	nextCompact := 0
	if width <= 60 || height < 20 {
		nextCompact = 2
	} else if width < 100 || height < 28 {
		nextCompact = 1
	}
	if nextCompact != d.compactLevel {
		d.compactLevel = nextCompact
		d.renderFooter()
	}
	switch {
	case height < 16:
		// At 100x12 this leaves six rows for the bordered task table, so
		// navigation remains useful while the optional log pane is hidden.
		d.layout.ResizeItem(d.header, 3, 0)
		d.layout.ResizeItem(d.tasks, 0, 1)
		d.layout.ResizeItem(d.logs, 0, 0)
		d.layout.ResizeItem(d.footer, 3, 0)
	case height < 24:
		d.layout.ResizeItem(d.header, 3, 0)
		d.layout.ResizeItem(d.tasks, 0, 2)
		d.layout.ResizeItem(d.logs, 0, 2)
		d.layout.ResizeItem(d.footer, 4, 0)
	default:
		d.layout.ResizeItem(d.header, 7, 0)
		d.layout.ResizeItem(d.tasks, 0, 2)
		d.layout.ResizeItem(d.logs, 0, 3)
		d.layout.ResizeItem(d.footer, 5, 0)
	}
}

func (d *dashboard) updateFocusTreatment() {
	focus := d.app.GetFocus()
	if focus == d.tasks {
		d.tasks.SetBorderColor(tcell.ColorLightBlue).SetTitle(" Tasks [FOCUSED] ")
	} else {
		d.tasks.SetBorderColor(tcell.ColorGray).SetTitle(" Tasks ")
	}
	logTitle := strings.TrimSpace(d.logs.GetTitle())
	logTitle = strings.TrimSuffix(logTitle, "[FOCUSED]")
	logTitle = strings.TrimSpace(logTitle)
	if focus == d.logs {
		d.logs.SetBorderColor(tcell.ColorLightBlue).SetTitle(" " + logTitle + " [FOCUSED] ")
	} else {
		d.logs.SetBorderColor(tcell.ColorGray).SetTitle(" " + logTitle + " ")
	}
}

func (d *dashboard) fallbackRefreshLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(fallbackRefreshInterval):
			d.app.QueueUpdateDraw(func() {
				_ = d.refresh()
			})
		}
	}
}

func (d *dashboard) eventLoop(ctx context.Context) {
	dir := filepath.Dir(d.eventsPath)
	runner, err := watch.New(watch.Options{
		Root:         dir,
		Debounce:     40 * time.Millisecond,
		PollInterval: 250 * time.Millisecond,
		WatchPaths:   []string{"events.jsonl"},
		WatchOnly:    true,
	})
	if err != nil {
		return
	}
	batches, errs, err := runner.Start(ctx)
	if err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-errs:
			if !ok {
				return
			}
		case batch, ok := <-batches:
			if !ok {
				return
			}
			if !stringSliceContains(batch.Files, "events.jsonl") {
				continue
			}
			events := d.readNewEvents()
			d.app.QueueUpdateDraw(func() {
				d.applyEvents(events)
				_ = d.refresh()
			})
		}
	}
}

func (d *dashboard) daemonEventLoop(ctx context.Context, client *daemon.Client) {
	if client == nil {
		return
	}
	_ = client.Subscribe(ctx, func(evt api.Event) {
		d.app.QueueUpdateDraw(func() {
			d.applyEvents([]api.Event{evt})
			_ = d.refresh()
		})
	})
}

func (d *dashboard) handleKeys(event *tcell.EventKey) *tcell.EventKey {
	if d.helpOpen {
		if event.Key() == tcell.KeyEsc || (event.Key() == tcell.KeyRune && (event.Rune() == '?' || event.Rune() == 'q')) {
			d.closeHelp()
			return nil
		}
		return event
	}
	if d.lifecycleModal && event.Key() == tcell.KeyEsc {
		d.closeLifecyclePlan("[yellow]lifecycle action canceled")
		return nil
	}
	if d.activeInput {
		return event
	}
	logsFocused := d.app.GetFocus() == d.logs
	switch event.Key() {
	case tcell.KeyEsc:
		d.app.Stop()
		return nil
	case tcell.KeyTAB, tcell.KeyBacktab:
		if logsFocused {
			d.app.SetFocus(d.tasks)
		} else {
			d.app.SetFocus(d.logs)
		}
		d.updateFocusTreatment()
		d.renderFooter()
		return nil
	case tcell.KeyHome:
		if logsFocused {
			d.logFollowing = false
			d.logs.ScrollToBeginning()
			d.rememberLogScroll(d.renderedLogKey, 0, 0)
			d.updateLogs()
		} else {
			d.selectIndex(0)
		}
		return nil
	case tcell.KeyEnd:
		if logsFocused {
			d.resumeLogFollowing()
		} else {
			d.selectIndex(len(d.currentNodes) - 1)
		}
		return nil
	case tcell.KeyPgUp:
		d.pauseAndScrollLogs(-10)
		return nil
	case tcell.KeyPgDn:
		d.scrollLogs(10)
		return nil
	case tcell.KeyF2:
		d.toggleDatabasePanel()
		return nil
	case tcell.KeyF3:
		d.toggleSupervisorLog()
		return nil
	case tcell.KeyF4:
		d.openPrismaMigrationPrompt()
		return nil
	case tcell.KeyF5:
		d.triggerInvalidateSelected()
		return nil
	case tcell.KeyF6:
		d.triggerRetargetSelected()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case '?':
			d.openHelp()
			return nil
		case 'q':
			d.app.Stop()
			return nil
		case 'j':
			if logsFocused {
				d.scrollLogs(1)
			} else {
				d.moveSelection(1)
			}
			return nil
		case 'k':
			if logsFocused {
				d.pauseAndScrollLogs(-1)
			} else {
				d.moveSelection(-1)
			}
			return nil
		case 'g':
			if logsFocused {
				d.logFollowing = false
				d.logs.ScrollToBeginning()
				d.rememberLogScroll(d.renderedLogKey, 0, 0)
				d.updateLogs()
			} else {
				d.selectIndex(0)
			}
			return nil
		case 'G':
			if logsFocused {
				d.resumeLogFollowing()
			} else {
				d.selectIndex(len(d.currentNodes) - 1)
			}
			return nil
		case 'f':
			d.resumeLogFollowing()
			return nil
		case 'o':
			if logsFocused {
				d.loadOlderLogContent()
			}
			return nil
		case 'a':
			d.attentionOnly = !d.attentionOnly
			_ = d.refresh()
			return nil
		case 'l':
			d.toggleSupervisorLog()
			return nil
		case 'd':
			d.toggleDatabasePanel()
			return nil
		case 'm':
			d.openPrismaMigrationPrompt()
			return nil
		case 'i':
			d.triggerInvalidateSelected()
			return nil
		case 't':
			d.triggerRetargetSelected()
			return nil
		}
	case tcell.KeyDown:
		if logsFocused {
			d.scrollLogs(1)
		} else {
			d.moveSelection(1)
		}
		return nil
	case tcell.KeyUp:
		if logsFocused {
			d.pauseAndScrollLogs(-1)
		} else {
			d.moveSelection(-1)
		}
		return nil
	}
	return event
}

func (d *dashboard) openHelp() {
	if d.activeInput || d.helpOpen {
		return
	}
	help := tview.NewTextView().
		SetDynamicColors(true).
		SetText(strings.Join([]string{
			"[yellow]Navigation[-]  Tab changes pane; j/k or arrows move in the focused pane.",
			"[yellow]Logs[-]        running logs open at the tail; Page Up/up pauses; End/f resumes; o loads older retained lines.",
			"[yellow]Actions[-]     i previews rerun scope; t previews retarget scope; m creates a migration.",
			"[yellow]Views[-]       l switches task/supervisor log; d opens database details.",
			"[yellow]Quit[-]        q or Escape closes the UI; a pre-existing detached run remains active.",
			"",
			"Press Escape, ? or q to close help.",
		}, "\n"))
	help.SetBorder(true).SetTitle(" Contextual Help ")
	d.helpOpen = true
	d.pages.AddPage("help", centered(help, 88, 13), true, true)
	d.app.SetFocus(help)
	d.renderFooter()
}

func (d *dashboard) closeHelp() {
	d.helpOpen = false
	d.pages.RemovePage("help")
	d.app.SetFocus(d.logs)
	d.updateFocusTreatment()
	d.renderFooter()
}

func (d *dashboard) toggleSupervisorLog() {
	d.showSupervisorLog = !d.showSupervisorLog
	if d.showSupervisorLog {
		d.showDatabasePanel = false
	}
	d.updateLogs()
}

func (d *dashboard) toggleDatabasePanel() {
	d.showDatabasePanel = !d.showDatabasePanel
	if d.showDatabasePanel {
		d.showSupervisorLog = false
	}
	d.updateLogs()
}

func (d *dashboard) moveSelection(delta int) {
	if len(d.currentNodes) == 0 {
		return
	}
	row, _ := d.tasks.GetSelection()
	index := max(0, min(len(d.currentNodes)-1, row-1+delta))
	d.selectIndex(index)
}

func (d *dashboard) selectIndex(index int) {
	if len(d.currentNodes) == 0 {
		return
	}
	index = max(0, min(len(d.currentNodes)-1, index))
	d.selectedName = d.currentNodes[index].Name
	d.logFollowing = true
	d.logLineLimit = 200
	d.tasks.Select(index+1, 0)
	d.updateLogs()
}

func (d *dashboard) refresh() error {
	requestedSelection := d.selectedName
	snap, err := loadSnapshotWithLogLimit(d.root, d.instanceID, d.showSupervisorLog, d.showDatabasePanel, d.selectedName, d.logLineLimit)
	if err != nil {
		d.header.SetText(fmt.Sprintf("[red]failed to load instance state: %v", err))
		return err
	}
	d.allNodes = append([]api.NodeStatus(nil), snap.nodes...)
	d.currentNodes = filterAttentionNodes(snap.nodes, d.attentionOnly)
	snap.nodes = d.currentNodes
	d.header.SetText(strings.Join(renderHeader(snap), "\n"))
	d.renderTasks(snap.nodes)
	d.reconcileSelection()
	if !d.showSupervisorLog && !d.showDatabasePanel && d.selectedName != requestedSelection {
		selectedSnap, reloadErr := loadSnapshotWithLogLimit(d.root, d.instanceID, false, false, d.selectedName, d.logLineLimit)
		if reloadErr == nil {
			selectedSnap.nodes = d.currentNodes
			snap = selectedSnap
		}
	}
	d.updateLogsFromSnapshot(snap)
	d.renderFooter()
	return nil
}

func filterAttentionNodes(nodes []api.NodeStatus, enabled bool) []api.NodeStatus {
	if !enabled {
		return append([]api.NodeStatus(nil), nodes...)
	}
	out := make([]api.NodeStatus, 0, len(nodes))
	for _, node := range nodes {
		switch node.State {
		case api.StateStarting, api.StateReady, api.StateRunning, api.StateRestarting, api.StateFailed, api.StateBlocked, api.StateDegraded, api.StateMigrationNeeded:
			out = append(out, node)
		}
	}
	return out
}

func (d *dashboard) renderTasks(nodes []api.NodeStatus) {
	rowOffset, columnOffset := d.tasks.GetOffset()
	d.tasks.Clear()
	headers := []string{"STATE", "TASK", "KIND", "REASON"}
	for col, header := range headers {
		d.tasks.SetCell(0, col, tview.NewTableCell(header).
			SetSelectable(false).
			SetAttributes(tcell.AttrBold).
			SetTextColor(tcell.ColorWhite))
	}
	for row, node := range nodes {
		state := stateBadge(node.State)
		if node.Ready && node.State == api.StateRunning && (node.Kind == string(project.KindService) || node.Kind == string(project.KindDebugService)) {
			state = "READY"
		}
		color := stateColor(node.State)
		d.tasks.SetCell(row+1, 0, tview.NewTableCell(state).
			SetTextColor(color).
			SetSelectable(true))
		d.tasks.SetCell(row+1, 1, tview.NewTableCell(node.Name).
			SetTextColor(color).
			SetSelectable(true).SetExpansion(1))
		d.tasks.SetCell(row+1, 2, tview.NewTableCell(node.Kind).
			SetTextColor(tcell.ColorGray).
			SetSelectable(true))
		d.tasks.SetCell(row+1, 3, tview.NewTableCell(nodeStateReason(node)).
			SetTextColor(color).
			SetSelectable(true).
			SetMaxWidth(56))
	}
	d.tasks.SetOffset(rowOffset, columnOffset)
}

func nodeStateReason(node api.NodeStatus) string {
	switch node.State {
	case api.StateFailed, api.StateCanceled, api.StateBlocked, api.StateDegraded, api.StateMigrationNeeded:
		reason := strings.TrimSpace(node.LastError)
		if reason == "" {
			return string(node.State)
		}
		if len(reason) > 120 {
			return reason[:117] + "..."
		}
		return reason
	default:
		return ""
	}
}

func (d *dashboard) reconcileSelection() {
	if len(d.currentNodes) == 0 {
		d.selectedName = ""
		return
	}
	if d.selectedName != "" {
		for i, node := range d.currentNodes {
			if node.Name == d.selectedName {
				d.tasks.Select(i+1, 0)
				return
			}
		}
	}
	d.selectedName = d.currentNodes[0].Name
	d.tasks.Select(1, 0)
}

func (d *dashboard) updateLogs() {
	snap, err := loadSnapshotWithLogLimit(d.root, d.instanceID, d.showSupervisorLog, d.showDatabasePanel, d.selectedName, d.logLineLimit)
	if err != nil {
		d.logs.SetTitle(" Logs ")
		d.logs.SetText(fmt.Sprintf("failed to load logs: %v", err))
		return
	}
	d.updateLogsFromSnapshot(snap)
}

func (d *dashboard) updateLogsFromSnapshot(snap snapshot) {
	d.lastSnapshot = snap
	nextKey := logViewKey(snap, d.selectedName)
	prevKey := d.renderedLogKey
	if nextKey != prevKey {
		d.logFollowing = true
	}
	row, col := 0, 0
	if nextKey != "" && nextKey == prevKey {
		row, col = d.desiredLogScroll(nextKey)
	}
	if snap.logPath != "" {
		if len(snap.logLines) > 0 {
			d.lastLogPath = snap.logPath
			d.lastLogLines = append([]string(nil), snap.logLines...)
		} else if snap.logPath == d.lastLogPath && len(d.lastLogLines) > 0 && transientEmptyLogAllowed(snap, d.selectedName) {
			snap.logLines = append([]string(nil), d.lastLogLines...)
		}
	} else {
		d.lastLogPath = ""
		d.lastLogLines = nil
	}
	followState := "PAUSED"
	if d.logFollowing {
		followState = "FOLLOWING"
	}
	d.logs.SetTitle(" " + snap.logTitle + " · " + followState + " ")
	lines := renderLogPanel(snap, d.selectedName)
	d.logs.SetText(strings.Join(lines, "\n"))
	if d.logFollowing {
		if nextKey != prevKey {
			d.logs.ScrollToBeginning()
		}
		d.logs.ScrollToEnd()
		row, col = d.logs.GetScrollOffset()
		d.rememberLogScroll(nextKey, row, col)
	} else if nextKey != "" && nextKey == prevKey {
		d.logs.ScrollTo(row, col)
		d.rememberLogScroll(nextKey, row, col)
	} else {
		d.logs.ScrollToBeginning()
		d.rememberLogScroll(nextKey, 0, 0)
	}
	d.renderedLogKey = nextKey
	d.updateFocusTreatment()
}

func logViewKey(snap snapshot, selectedName string) string {
	if snap.logTitle == databasePanelTitle || snap.logTitle == supervisorLogTitle {
		return snap.logTitle + "\x00" + snap.logPath
	}
	if snap.logPath != "" {
		return snap.logTitle + "\x00" + snap.logPath
	}
	return snap.logTitle + "\x00" + selectedName
}

func transientEmptyLogAllowed(snap snapshot, selectedName string) bool {
	if snap.logTitle == supervisorLogTitle || snap.logTitle == databasePanelTitle {
		return false
	}
	node := findSelectedNode(snap.nodes, selectedName)
	if node == nil {
		return false
	}
	switch node.State {
	case api.StatePending, api.StateReady, api.StateRunning, api.StateDirty:
		return true
	default:
		return false
	}
}

func (d *dashboard) scrollLogs(delta int) {
	if delta == 0 {
		return
	}
	d.logFollowing = false
	row, col := d.desiredLogScroll(d.renderedLogKey)
	nextRow := row + delta
	if nextRow < 0 {
		nextRow = 0
	}
	d.logs.ScrollTo(nextRow, col)
	d.rememberLogScroll(d.renderedLogKey, nextRow, col)
}

func (d *dashboard) pauseAndScrollLogs(delta int) {
	d.logFollowing = false
	d.scrollLogs(delta)
	d.updateLogs()
}

func (d *dashboard) resumeLogFollowing() {
	d.logFollowing = true
	if d.lastSnapshot.state != nil || d.lastSnapshot.logTitle != "" {
		d.updateLogsFromSnapshot(d.lastSnapshot)
	} else {
		d.updateLogs()
	}
	d.logs.ScrollToEnd()
	row, col := d.logs.GetScrollOffset()
	d.rememberLogScroll(d.renderedLogKey, row, col)
}

func (d *dashboard) loadOlderLogContent() {
	if !d.lastSnapshot.logTruncated {
		d.setStatus("[yellow]the complete retained log is already loaded")
		return
	}
	d.logFollowing = false
	d.logLineLimit = min(1000, d.logLineLimit+200)
	d.setStatus(fmt.Sprintf("[yellow]loading up to %d retained log lines...", d.logLineLimit))
	d.updateLogs()
}

func (d *dashboard) rememberLogScrollDelta(delta int) {
	if delta == 0 {
		return
	}
	row, col := d.desiredLogScroll(d.renderedLogKey)
	nextRow := row + delta
	if nextRow < 0 {
		nextRow = 0
	}
	d.rememberLogScroll(d.renderedLogKey, nextRow, col)
}

func (d *dashboard) desiredLogScroll(key string) (int, int) {
	if key != "" && d.logScrollKey == key {
		return d.logScrollRow, d.logScrollCol
	}
	return d.logs.GetScrollOffset()
}

func (d *dashboard) rememberLogScroll(key string, row, col int) {
	if key == "" {
		return
	}
	if row < 0 {
		row = 0
	}
	if col < 0 {
		col = 0
	}
	d.logScrollKey = key
	d.logScrollRow = row
	d.logScrollCol = col
}

func (d *dashboard) setStatus(msg string) {
	d.statusMessage = truncateStatusMessage(msg, 240)
	d.statusAt = time.Now()
	d.renderFooter()
}

func truncateStatusMessage(message string, limit int) string {
	if limit <= 0 || len(message) <= limit {
		return message
	}
	const suffix = "… (? help; task log has details)"
	return truncateTUILogLine(message, limit-len(suffix)) + suffix
}

func (d *dashboard) renderFooter() {
	status := d.statusMessage
	if status == "" {
		status = "[green]ready"
	}
	timestamp := ""
	if !d.statusAt.IsZero() {
		timestamp = d.statusAt.Format("15:04:05") + "  "
	}
	if d.helpOpen {
		d.footer.SetText("Escape/? close help\n" + timestamp + status)
		return
	}
	if d.activeInput {
		d.footer.SetText("Enter accept  Escape cancel  editing keys available\n" + timestamp + status)
		return
	}
	if d.compactLevel >= 2 {
		d.footer.SetText("? help  Tab focus  q quit  f follow  o older  a attention\n" + timestamp + status)
		return
	}
	d.footer.SetText("? help  Tab focus  q quit  j/k/arrows move  Home/End contextual  f follow  o older\n" +
		"l task/supervisor log  d db  a attention  m migration  i rerun  t retarget\n" + timestamp + status)
}

func (d *dashboard) openPrismaMigrationPrompt() {
	if d.busy {
		d.setStatus("[yellow]action already running")
		return
	}
	inst, err := instance.Load(d.root, d.instanceID)
	if err != nil {
		d.setStatus(fmt.Sprintf("[red]failed to load instance: %v", err))
		return
	}
	cfg, err := resolvePrismaConfig(d.root, inst)
	if err != nil {
		d.setStatus(fmt.Sprintf("[red]failed to resolve prisma config: %v", err))
		return
	}
	title := "Create Migration"
	detail := "devflow.database.migration.create action"
	if cfg.Available {
		title = "Create Prisma Migration"
		detail = fmt.Sprintf("%s -> %s", cfg.SchemaPath, cfg.MigrationsDir)
	}
	var input *tview.InputField
	input = tview.NewInputField().
		SetLabel("Migration name ").
		SetFieldWidth(48).
		SetDoneFunc(func(key tcell.Key) {
			switch key {
			case tcell.KeyEscape:
				d.closePrismaMigrationPrompt()
				return
			case tcell.KeyEnter:
			default:
				return
			}
			name := strings.TrimSpace(input.GetText())
			if name == "" {
				d.setStatus("[red]migration name is required")
				return
			}
			d.closePrismaMigrationPrompt()
			d.triggerGeneratePrismaMigration(name)
		})
	frame := tview.NewFrame(input).
		SetBorders(1, 1, 1, 1, 1, 1).
		AddText(title, true, tview.AlignCenter, tcell.ColorWhite).
		AddText("Enter creates the migration. Escape cancels.", false, tview.AlignCenter, tcell.ColorGray).
		AddText(detail, false, tview.AlignCenter, tcell.ColorGray)
	d.activeInput = true
	d.pages.AddPage("prisma_migration", centered(frame, 84, 8), true, true)
	d.app.SetFocus(input)
	d.renderFooter()
}

func (d *dashboard) closePrismaMigrationPrompt() {
	d.activeInput = false
	d.pages.RemovePage("prisma_migration")
	d.app.SetFocus(d.logs)
	d.updateFocusTreatment()
	d.renderFooter()
}

func (d *dashboard) triggerGeneratePrismaMigration(name string) {
	if d.busy {
		d.setStatus("[yellow]action already running")
		return
	}
	if strings.TrimSpace(name) == "" {
		d.setStatus("[red]migration name is required")
		return
	}
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]creating migration %q...", name))
	go func() {
		progress := func(message string) {
			message = strings.TrimSpace(message)
			if message == "" {
				return
			}
			d.app.QueueUpdateDraw(func() {
				if d.busy {
					d.setStatus("[yellow]" + message)
				}
			})
		}
		err := generatePrismaMigrationFromTUI(d.root, d.instanceID, name, progress)
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			d.showDatabasePanel = true
			d.showSupervisorLog = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]migration failed: %v", err))
				_ = d.refresh()
				return
			}
			d.setStatus(fmt.Sprintf("[green]created migration %q", name))
			_ = d.refresh()
		})
	}()
}

func (d *dashboard) triggerInvalidateSelected() {
	if d.busy {
		d.setStatus("[yellow]action already running")
		return
	}
	node := findSelectedNode(d.currentNodes, d.selectedName)
	if node == nil {
		d.setStatus("[red]no task selected")
		return
	}
	selected := node.Name
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]rerun request accepted for %s; planning scope...", selected))
	go func() {
		plan, err := previewLifecycleForTUI(d.root, daemon.Request{Action: daemon.ActionInvalidate, Task: selected})
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]rerun planning failed: %v", err))
				return
			}
			d.openLifecyclePlan(plan, func() { d.executeInvalidate(selected) })
		})
	}()
}

func (d *dashboard) executeInvalidate(selected string) {
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]rerun running for %s...", selected))
	go func() {
		err := invalidateAndRerunDownstream(d.root, d.instanceID, selected, func() {
			d.app.QueueUpdateDraw(func() {
				d.setStatus(fmt.Sprintf("[yellow]rerun running: invalidated downstream from %s...", selected))
			})
		})
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]rerun failed: %v", err))
				return
			}
			d.setStatus(fmt.Sprintf("[green]rerun successful for %s", selected))
			_ = d.refresh()
		})
	}()
}

func (d *dashboard) triggerRetargetSelected() {
	if d.busy {
		d.setStatus("[yellow]action already running")
		return
	}
	inst, err := instance.Load(d.root, d.instanceID)
	if err != nil {
		d.setStatus(fmt.Sprintf("[red]retarget failed to load instance: %v", err))
		return
	}
	_, p, err := resolveRelaunchProject(d.root, inst)
	if err != nil {
		d.setStatus(fmt.Sprintf("[red]retarget failed to resolve project: %v", err))
		return
	}
	targets := make([]string, 0)
	for _, target := range p.Targets() {
		if target.Name != inst.LastRun.Target {
			targets = append(targets, target.Name)
		}
	}
	sort.Strings(targets)
	if len(targets) == 0 {
		d.setStatus("[yellow]no alternate target is available")
		return
	}
	buttons := append(append([]string(nil), targets...), "Cancel")
	modal := tview.NewModal().
		SetText(fmt.Sprintf("Current target: %s\nChoose a target to preview.", inst.LastRun.Target)).
		AddButtons(buttons).
		SetDoneFunc(func(_ int, label string) {
			if label == "Cancel" || label == "" {
				d.closeLifecyclePlan("[yellow]retarget canceled")
				return
			}
			d.closeLifecyclePlan("")
			d.planRetarget(label)
		})
	d.lifecycleModal = true
	d.activeInput = true
	d.pages.AddPage("lifecycle_plan", modal, true, true)
	d.app.SetFocus(modal)
	d.setStatus("[yellow]retarget chooser open; Enter selects, Escape cancels")
}

func (d *dashboard) planRetarget(selected string) {
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]retarget request accepted for %s; planning scope...", selected))
	go func() {
		plan, err := previewLifecycleForTUI(d.root, daemon.Request{Action: daemon.ActionRetarget, Target: selected})
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]retarget planning failed: %v", err))
				return
			}
			d.openLifecyclePlan(plan, func() { d.executeRetarget(selected) })
		})
	}()
}

func (d *dashboard) executeRetarget(selected string) {
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]retarget running for %s...", selected))
	go func() {
		err := retargetAndRelaunch(d.root, d.instanceID, selected)
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]retarget failed: %v", err))
				return
			}
			d.setStatus(fmt.Sprintf("[green]retarget successful for %s", selected))
			_ = d.refresh()
		})
	}()
}

func (d *dashboard) openLifecyclePlan(plan api.LifecyclePlan, execute func()) {
	text := strings.Join([]string{
		fmt.Sprintf("Action: %s", plan.RequestedAction),
		fmt.Sprintf("Selected: %s%s", plan.SelectedTask, plan.SelectedTarget),
		fmt.Sprintf("Invalidate: %s", lifecycleList(plan.TasksToInvalidate)),
		fmt.Sprintf("Stop: %s", lifecycleList(plan.ProcessesToStop)),
		fmt.Sprintf("Execute: %s", lifecycleList(plan.TasksToExecute)),
		fmt.Sprintf("Preserve: %s", lifecycleList(plan.ServicesToPreserve)),
		fmt.Sprintf("Restart/start: %s", lifecycleList(plan.ServicesToRestart)),
	}, "\n")
	modal := tview.NewModal().
		SetText(text).
		AddButtons([]string{"Execute", "Cancel"}).
		SetDoneFunc(func(_ int, label string) {
			if label != "Execute" {
				d.closeLifecyclePlan("[yellow]lifecycle action canceled")
				return
			}
			d.closeLifecyclePlan("")
			execute()
		})
	d.lifecycleModal = true
	d.activeInput = true
	d.pages.AddPage("lifecycle_plan", modal, true, true)
	d.app.SetFocus(modal)
	d.setStatus("[yellow]lifecycle plan ready; Enter executes, Escape cancels")
}

func (d *dashboard) closeLifecyclePlan(status string) {
	d.lifecycleModal = false
	d.activeInput = false
	d.pages.RemovePage("lifecycle_plan")
	d.app.SetFocus(d.tasks)
	d.updateFocusTreatment()
	if status != "" {
		d.setStatus(status)
	} else {
		d.renderFooter()
	}
}

func lifecycleList(values []string) string {
	if len(values) == 0 {
		return "none"
	}
	return strings.Join(values, ", ")
}

func loadSnapshot(root, instanceID string, showSupervisor bool, showDatabase bool, selectedName string) (snapshot, error) {
	return loadSnapshotWithLogLimit(root, instanceID, showSupervisor, showDatabase, selectedName, 200)
}

func loadSnapshotWithLogLimit(root, instanceID string, showSupervisor bool, showDatabase bool, selectedName string, logLineLimit int) (snapshot, error) {
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return snapshot{}, err
	}
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		if !os.IsNotExist(err) {
			return snapshot{}, err
		}
		state = &instance.State{
			Target: inst.LastRun.Target,
			Mode:   inst.LastRun.Mode,
			Nodes:  map[string]api.NodeStatus{},
		}
	}
	if state.Target == "" && inst.LastRun.Target != "" {
		state.Target = inst.LastRun.Target
	}
	if state.Mode == "" && inst.LastRun.Mode != "" {
		state.Mode = inst.LastRun.Mode
	}
	supervisor := supervisorStatus(inst)
	if supervisor != nil && !supervisor.Alive {
		if err := instance.ClearSupervisor(inst); err == nil {
			_ = markAllStoppedNodes(root, instanceID)
			inst, _ = instance.Load(root, instanceID)
			state, _ = instance.LoadStatus(root, instanceID)
			supervisor = supervisorStatus(inst)
		}
	}

	var prisma []prismaSnapshotSummary
	var prismaErr string
	var prismaCfg tuiPrismaConfig
	var prismaDev *database.PrismaDevelopmentStatus
	var prismaDevErr string
	if showDatabase {
		prismaCfg, err = resolvePrismaConfig(root, inst)
		if err != nil {
			prismaDevErr = err.Error()
		}
		if prismaCfg.Available {
			prismaDev, err = database.InspectPrismaDevelopmentStatus(root, prismaCfg.SchemaPath, prismaCfg.MigrationsDir, prismaCfg.BasePaths, inst.DB.SnapshotRoot)
			if err != nil {
				prismaDevErr = err.Error()
			}
		}
		prisma, err = loadPrismaSnapshotSummaries(inst.DB.SnapshotRoot, 10)
		if err != nil {
			prismaErr = err.Error()
		}
	}

	nodes := make([]api.NodeStatus, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, normalizeTUIState(node, prismaDev))
	}
	orderNodesForTUI(nodes, inst, state.Target)

	selected := findSelectedNode(nodes, selectedName)
	logTitle := "selected log"
	logPath := ""
	if showDatabase {
		logTitle = databasePanelTitle
	} else if showSupervisor {
		logTitle = supervisorLogTitle
		if supervisor != nil {
			logPath = supervisor.LogPath
		}
	} else if selected != nil {
		logTitle = selected.Name + " log"
		logPath = selected.LogPath
	}
	if logLineLimit <= 0 {
		logLineLimit = 200
	}
	logInfo, _ := readLastLinesInfo(logPath, logLineLimit)

	return snapshot{
		instance:     inst,
		state:        state,
		nodes:        nodes,
		supervisor:   supervisor,
		urls:         instanceURLs(inst),
		logTitle:     logTitle,
		logPath:      logPath,
		logLines:     logInfo.lines,
		logStartLine: logInfo.startLine,
		logEndLine:   logInfo.endLine,
		logTruncated: logInfo.truncated,
		prisma:       prisma,
		prismaErr:    prismaErr,
		prismaCfg:    prismaCfg,
		prismaDev:    prismaDev,
		prismaDevErr: prismaDevErr,
	}, nil
}

func orderNodesForTUI(nodes []api.NodeStatus, inst *api.Instance, target string) {
	positions := map[string]int{}
	if inst != nil && strings.TrimSpace(inst.LastRun.Project) != "" && strings.TrimSpace(target) != "" {
		if p, err := project.Lookup(inst.LastRun.Project); err == nil {
			if execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target); err == nil {
				if g, err := graph.New(execProject.Tasks(), execProject.Targets()); err == nil {
					if closure, err := g.TargetClosure(resolvedTarget); err == nil {
						for index, name := range closure {
							positions[name] = index + 1
						}
					}
				}
			}
		}
	}
	sort.SliceStable(nodes, func(i, j int) bool {
		left, leftOK := positions[nodes[i].Name]
		right, rightOK := positions[nodes[j].Name]
		if leftOK != rightOK {
			return leftOK
		}
		if leftOK && left != right {
			return left < right
		}
		return nodes[i].Name < nodes[j].Name
	})
}

func normalizeTUIState(node api.NodeStatus, prismaDev *database.PrismaDevelopmentStatus) api.NodeStatus {
	if node.State == api.StateFailed && looksLikeMigrationNeededStatus(node.LastError) {
		node.State = api.StateMigrationNeeded
	}
	if node.State == api.StateFailed && prismaDev != nil && prismaDev.NeedsNewMigration && node.Name == "db_prepare" {
		node.State = api.StateMigrationNeeded
	}
	return node
}

func renderHeader(snap snapshot) []string {
	urlParts := make([]string, 0, len(snap.urls))
	for name, url := range snap.urls {
		urlParts = append(urlParts, fmt.Sprintf("%s=%s", name, url))
	}
	sort.Strings(urlParts)
	if len(urlParts) == 0 {
		urlParts = append(urlParts, "no urls")
	}

	supervisorText := "supervisor: stopped"
	if snap.supervisor != nil {
		state := "stopped"
		if snap.supervisor.Alive {
			state = "running"
		}
		supervisorText = fmt.Sprintf("supervisor: %s pid=%d", state, snap.supervisor.PID)
	}

	counts := map[api.NodeState]int{}
	for _, node := range snap.nodes {
		counts[node.State]++
	}

	return []string{
		fmt.Sprintf("[yellow]instance[-]: %s    [yellow]target[-]: %s    [yellow]mode[-]: %s", snap.instance.ID, snap.state.Target, snap.state.Mode),
		fmt.Sprintf("[yellow]worktree[-]: %s", snap.instance.Worktree),
		fmt.Sprintf("[yellow]db[-]: %s host=%s port=%d container=%s", snap.instance.DB.Name, snap.instance.DB.Host, snap.instance.DB.Port, snap.instance.DB.ContainerName),
		fmt.Sprintf("[yellow]urls[-]: %s", strings.Join(urlParts, "    ")),
		fmt.Sprintf("[yellow]%s[-]    [yellow]states[-]: WAIT=%d START=%d RUN=%d READY=%d RSTR=%d CACHE=%d DONE=%d MIGR=%d FAIL=%d BLOCK=%d DEGR=%d CANC=%d STOP=%d",
			supervisorText,
			counts[api.StatePending]+counts[api.StateDirty],
			counts[api.StateStarting],
			counts[api.StateRunning],
			counts[api.StateReady],
			counts[api.StateRestarting],
			counts[api.StateCached],
			counts[api.StateDone],
			counts[api.StateMigrationNeeded],
			counts[api.StateFailed],
			counts[api.StateBlocked],
			counts[api.StateDegraded],
			counts[api.StateCanceled],
			counts[api.StateStopped],
		),
	}
}

func (d *dashboard) readNewEvents() []api.Event {
	info, err := os.Stat(d.eventsPath)
	if err != nil {
		return nil
	}
	if info.Size() < d.eventOffset {
		d.eventOffset = 0
	}
	file, err := os.Open(d.eventsPath)
	if err != nil {
		return nil
	}
	defer file.Close()
	if _, err := file.Seek(d.eventOffset, 0); err != nil {
		return nil
	}
	reader := bufio.NewReader(file)
	out := make([]api.Event, 0, 8)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			d.eventOffset += int64(len(line))
			var evt api.Event
			if jsonErr := json.Unmarshal(line, &evt); jsonErr == nil {
				out = append(out, evt)
			}
		}
		if err != nil {
			break
		}
	}
	return out
}

func (d *dashboard) applyEvents(events []api.Event) {
	if len(events) == 0 {
		return
	}
	for _, evt := range events {
		switch evt.Type {
		case api.EventWatchCycleStart:
			d.setStatus(fmt.Sprintf("[yellow]watch: files=%s affected=%s", strings.Join(evt.Files, ","), strings.Join(evt.AffectedTasks, ",")))
		case api.EventWatchCycleDone:
			if evt.Success != nil && *evt.Success {
				d.setStatus(fmt.Sprintf("[green]watch complete: files=%s", strings.Join(evt.Files, ",")))
			} else {
				d.setStatus(fmt.Sprintf("[red]watch failed: files=%s", strings.Join(evt.Files, ",")))
			}
		case api.EventRunStarted:
			d.setStatus(fmt.Sprintf("[yellow]run started: %s", evt.Target))
		case api.EventRunFinished:
			if evt.Success != nil && *evt.Success {
				d.setStatus(fmt.Sprintf("[green]run finished: %s", evt.Target))
			} else {
				d.setStatus(fmt.Sprintf("[red]run failed: %s", evt.Error))
			}
		case api.EventTaskState:
			if evt.Task != "" && (evt.State == api.StateMigrationNeeded || looksLikeMigrationNeededStatus(evt.Error)) {
				d.setStatus(fmt.Sprintf("[yellow]%s needs a migration: %s", evt.Task, evt.Error))
			} else if evt.State == api.StateFailed && evt.Task != "" {
				d.setStatus(fmt.Sprintf("[red]%s failed: %s", evt.Task, evt.Error))
			}
		case api.EventInteractionReq:
			d.openPrompt(evt)
		case api.EventInteractionAck, api.EventInteractionStop:
			if evt.PromptID == d.activePromptID {
				d.closePrompt()
			}
		}
	}
}

func (d *dashboard) openPrompt(evt api.Event) {
	if evt.PromptID == "" || evt.PromptID == d.activePromptID {
		return
	}
	d.activePromptID = evt.PromptID
	d.activeInput = true
	d.renderFooter()
	switch evt.PromptKind {
	case string(process.PromptConfirm):
		modal := tview.NewModal().
			SetText(evt.Prompt).
			AddButtons([]string{"Yes", "No"}).
			SetDoneFunc(func(_ int, label string) {
				answer := "n"
				if label == "Yes" {
					answer = "y"
				}
				if err := instance.WriteInteractionAnswer(d.root, d.instanceID, evt.PromptID, answer); err != nil {
					d.setStatus(fmt.Sprintf("[red]failed to answer prompt: %v", err))
					return
				}
				d.setStatus(fmt.Sprintf("[yellow]answered %s with %s", evt.Task, answer))
				d.closePrompt()
			})
		d.pages.AddPage("prompt", modal, true, true)
		d.app.SetFocus(modal)
	default:
		var input *tview.InputField
		input = tview.NewInputField().
			SetLabel(evt.Prompt + " ").
			SetDoneFunc(func(key tcell.Key) {
				if key != tcell.KeyEnter {
					return
				}
				value := input.GetText()
				if err := instance.WriteInteractionAnswer(d.root, d.instanceID, evt.PromptID, value); err != nil {
					d.setStatus(fmt.Sprintf("[red]failed to answer prompt: %v", err))
					return
				}
				d.setStatus(fmt.Sprintf("[yellow]answered %s prompt", evt.Task))
				d.closePrompt()
			})
		frame := tview.NewFrame(input).
			SetBorders(1, 1, 1, 1, 1, 1).
			AddText("Interactive Prompt", true, tview.AlignCenter, tcell.ColorWhite)
		d.pages.AddPage("prompt", centered(frame, 80, 7), true, true)
		d.app.SetFocus(input)
	}
}

func (d *dashboard) closePrompt() {
	d.activePromptID = ""
	d.activeInput = false
	d.pages.RemovePage("prompt")
	d.app.SetFocus(d.tasks)
	d.updateFocusTreatment()
	d.renderFooter()
}

func centered(p tview.Primitive, width, height int) tview.Primitive {
	return tview.NewFlex().
		AddItem(nil, 0, 1, false).
		AddItem(tview.NewFlex().SetDirection(tview.FlexRow).
			AddItem(nil, 0, 1, false).
			AddItem(p, height, 1, true).
			AddItem(nil, 0, 1, false), width, 1, true).
		AddItem(nil, 0, 1, false)
}

func renderLogPanel(snap snapshot, selectedName string) []string {
	if snap.logTitle == databasePanelTitle {
		return renderDatabasePanel(snap)
	}
	lines := []string{}
	if snap.logTitle == supervisorLogTitle {
		lines = append(lines, "selected: supervisor")
	} else if node := findSelectedNode(snap.nodes, selectedName); node != nil {
		lines = append(lines, fmt.Sprintf("selected: %s    kind=%s    state=%s", node.Name, node.Kind, node.State))
		if node.PID > 0 {
			lines = append(lines, fmt.Sprintf("pid=%d generation=%d attempt=%d", node.PID, node.Generation, node.Attempt))
		}
		if node.Debug != nil {
			lines = append(lines, fmt.Sprintf("debug=%s://%s:%d port=%s package=%s", node.Debug.Protocol, node.Debug.Host, node.Debug.Port, node.Debug.PortName, node.Debug.Package))
		}
		if node.LastRunKey != "" {
			lines = append(lines, fmt.Sprintf("key=%s", node.LastRunKey))
		}
		if node.LastError != "" {
			lines = append(lines, fmt.Sprintf("error=%s", node.LastError))
		}
		for _, excerpt := range node.FailureExcerpts {
			lines = append(lines, fmt.Sprintf("failure context (%s lines %d-%d):", excerpt.Reason, excerpt.StartLine, excerpt.EndLine))
			lines = append(lines, excerpt.Lines...)
		}
	}
	if snap.logEndLine > 0 {
		retention := fmt.Sprintf("retained log lines %d-%d", snap.logStartLine, snap.logEndLine)
		if snap.logTruncated {
			retention += " (earlier content truncated; task failure excerpts remain available)"
		}
		lines = append(lines, retention)
	}
	lines = append(lines, "")
	if len(snap.logLines) == 0 {
		lines = append(lines, "no log lines yet")
		return lines
	}
	lines = append(lines, snap.logLines...)
	return lines
}

func renderDatabasePanel(snap snapshot) []string {
	lines := []string{"selected: database / prisma", ""}
	if snap.instance == nil {
		lines = append(lines, "managed postgres: not configured")
		return lines
	}
	db := snap.instance.DB
	if db.Name == "" && db.ContainerName == "" && db.SnapshotRoot == "" {
		lines = append(lines, "managed postgres: not configured")
		return lines
	}
	lines = append(lines, "managed postgres:")
	flavor := db.Flavor
	if flavor == "" {
		flavor = database.FlavorPostgres
	}
	image := db.Image
	if image == "" {
		image = "auto"
	}
	databaseRuntime := fmt.Sprintf("flavor=%s", flavor)
	if db.PostgresVersion > 0 {
		databaseRuntime += fmt.Sprintf(" postgres=%d", db.PostgresVersion)
	}
	lines = append(lines, fmt.Sprintf("name=%s host=%s port=%d user=%s %s image=%s", db.Name, db.Host, db.Port, db.User, databaseRuntime, image))
	lines = append(lines, fmt.Sprintf("container=%s volume=%s", db.ContainerName, db.VolumeName))
	if db.SnapshotRoot != "" {
		lines = append(lines, fmt.Sprintf("snapshotRoot=%s", db.SnapshotRoot))
	} else {
		lines = append(lines, "snapshotRoot=not configured")
	}
	lines = append(lines, "")
	if snap.prismaCfg.Available {
		lines = append(lines, fmt.Sprintf("prisma config: schema=%s migrations=%s source=%s", snap.prismaCfg.SchemaPath, snap.prismaCfg.MigrationsDir, snap.prismaCfg.Source))
		if snap.prismaDevErr != "" {
			lines = append(lines, "[red]migration folder/state: error[-] "+snap.prismaDevErr)
		} else if snap.prismaDev != nil && snap.prismaDev.NeedsNewMigration {
			lines = append(lines, fmt.Sprintf("[red]migration folder/state: needs new migration[-] reason=%s", snap.prismaDev.Reason))
			if snap.prismaDev.Message != "" {
				lines = append(lines, snap.prismaDev.Message)
			}
			lines = append(lines, "press m to create a Prisma migration (F4 also works)")
		} else if snap.prismaDev != nil {
			lines = append(lines, formatPrismaDevelopmentStatus(snap.prismaDev))
		}
	} else {
		lines = append(lines, "prisma config: not detected")
	}
	lines = append(lines, "")
	if snap.prismaErr != "" {
		lines = append(lines, "prisma snapshots: error="+snap.prismaErr)
		return lines
	}
	if len(snap.prisma) == 0 {
		lines = append(lines, "prisma snapshots: none")
		return lines
	}
	latest := snap.prisma[0]
	lines = append(lines, fmt.Sprintf("prisma snapshots: %d cached migration-prefix states", len(snap.prisma)))
	lines = append(lines, fmt.Sprintf("latest=%s migrations=%d created=%s", latest.Key, len(latest.MigrationNames), formatSnapshotTime(latest.CreatedAt)))
	lines = append(lines, "")
	lines = append(lines, "recent prisma snapshots:")
	for _, item := range snap.prisma {
		lines = append(lines, fmt.Sprintf("%s  migrations=%d  latest=%s", item.Key, len(item.MigrationNames), latestMigrationName(item.MigrationNames)))
		lines = append(lines, "  "+formatMigrationNames(item.MigrationNames))
	}
	return lines
}

func formatPrismaDevelopmentStatus(status *database.PrismaDevelopmentStatus) string {
	if status == nil || status.State == nil {
		return "migration folder/state: unknown"
	}
	migrationCount := len(status.State.Migrations)
	switch {
	case status.Plan.ExactMatch:
		return fmt.Sprintf("[green]migration folder/state: in sync[-] migrations=%d snapshot=%s", migrationCount, status.Plan.SnapshotKey)
	case status.Plan.SnapshotKey != "":
		return fmt.Sprintf("[yellow]migration folder/state: pending apply[-] migrations=%d nearest=%s prefix=%d", migrationCount, status.Plan.SnapshotKey, status.Plan.PrefixLength)
	default:
		return fmt.Sprintf("[yellow]migration folder/state: no compatible snapshot[-] migrations=%d", migrationCount)
	}
}

func loadPrismaSnapshotSummaries(root string, limit int) ([]prismaSnapshotSummary, error) {
	if root == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]prismaSnapshotSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		meta, err := database.LoadPrismaSnapshot(root, entry.Name())
		if err != nil {
			continue
		}
		snapshotKey := meta.Key
		if snapshotKey == "" {
			snapshotKey = entry.Name()
		}
		names := make([]string, 0, len(meta.Migrations))
		for _, migration := range meta.Migrations {
			names = append(names, migration.Name)
		}
		out = append(out, prismaSnapshotSummary{
			Key:             snapshotKey,
			CreatedAt:       meta.CreatedAt,
			SchemaHash:      meta.SchemaHash,
			BaseFingerprint: meta.BaseFingerprint,
			MigrationNames:  names,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].Key > out[j].Key
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func formatSnapshotTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}

func latestMigrationName(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return names[len(names)-1]
}

func formatMigrationNames(names []string) string {
	if len(names) == 0 {
		return "migrations=none"
	}
	if len(names) <= 4 {
		return "migrations=" + strings.Join(names, ",")
	}
	return "migrations=" + strings.Join(names[:2], ",") + ",...," + names[len(names)-1]
}

func resolvePrismaConfig(worktree string, inst *api.Instance) (tuiPrismaConfig, error) {
	if _, p, err := resolveRelaunchProject(worktree, inst); err == nil {
		if provider, ok := p.(project.PrismaConfigProvider); ok {
			cfg := normalizePrismaConfig(provider.PrismaConfig())
			if cfg.SchemaPath != "" {
				return tuiPrismaConfig{PrismaConfig: cfg, Source: "adapter", Available: true}, nil
			}
		}
	}
	for _, cfg := range []project.PrismaConfig{
		{SchemaPath: "prisma/schema.prisma", MigrationsDir: "prisma/migrations", CreateOnly: true},
		{SchemaPath: "db/schema.prisma", MigrationsDir: "db/migrations", CreateOnly: true},
	} {
		if _, err := os.Stat(filepath.Join(worktree, cfg.SchemaPath)); err == nil {
			return tuiPrismaConfig{PrismaConfig: normalizePrismaConfig(cfg), Source: "detected", Available: true}, nil
		}
	}
	return tuiPrismaConfig{}, nil
}

func normalizePrismaConfig(cfg project.PrismaConfig) project.PrismaConfig {
	cfg.SchemaPath = filepath.ToSlash(strings.TrimSpace(cfg.SchemaPath))
	cfg.MigrationsDir = filepath.ToSlash(strings.TrimSpace(cfg.MigrationsDir))
	if cfg.SchemaPath != "" && cfg.MigrationsDir == "" {
		cfg.MigrationsDir = filepath.ToSlash(filepath.Join(filepath.Dir(cfg.SchemaPath), "migrations"))
	}
	if cfg.Command.Name == "" {
		cfg.CreateOnly = true
	}
	return cfg
}

func generatePrismaMigrationFromTUI(root, instanceID, name string, progressFns ...func(string)) error {
	progress := firstTUIProgress(progressFns)
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("migration name is required")
	}
	_ = instanceID
	reportTUIProgress(progress, "connecting to daemon...")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	_, err := callDaemonForTUI(ctx, root, daemon.Request{
		Action:       daemon.ActionRunAction,
		ActionKind:   database.ActionMigrationCreate,
		StreamEvents: true,
		Inputs: map[string]string{
			"name": name,
		},
	}, func(evt api.Event) {
		reportDaemonEventForTUI(evt, progress)
	})
	return err
}

func reportDaemonEventForTUI(evt api.Event, progress func(string)) {
	switch evt.Type {
	case api.EventLogLine:
		if evt.Task == "daemon" && evt.Line != "" {
			reportTUIProgress(progress, "%s", compactTUIStatus(evt.Line, 120))
			return
		}
		if evt.Task != "" && evt.Line != "" {
			reportTUIProgress(progress, "%s %s: %s", evt.Task, evt.Stream, compactTUIStatus(evt.Line, 120))
		}
	case api.EventRunStarted:
		reportTUIProgress(progress, "run started: %s", evt.Target)
	case api.EventRunFinished:
		if evt.Success != nil && *evt.Success {
			reportTUIProgress(progress, "run finished: %s", evt.Target)
		} else if evt.Error != "" {
			reportTUIProgress(progress, "run failed: %s", compactTUIStatus(evt.Error, 120))
		}
	case api.EventTaskState:
		if evt.Task != "" && evt.State != "" {
			reportTUIProgress(progress, "%s: %s", evt.Task, evt.State)
		}
	}
}

func firstTUIProgress(progressFns []func(string)) func(string) {
	for _, progress := range progressFns {
		if progress != nil {
			return progress
		}
	}
	return nil
}

func reportTUIProgress(progress func(string), format string, args ...any) {
	if progress == nil {
		return
	}
	progress(fmt.Sprintf(format, args...))
}

func compactTUIStatus(text string, maxLen int) string {
	text = strings.NewReplacer("[", "(", "]", ")").Replace(strings.Join(strings.Fields(text), " "))
	if maxLen <= 0 || len(text) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return text[:maxLen]
	}
	return text[:maxLen-3] + "..."
}

func looksLikeMigrationNeededStatus(text string) bool {
	text = strings.ToLower(strings.TrimSpace(text))
	return strings.Contains(text, "generate one with generateprismamigration") ||
		strings.Contains(text, "generate a migration") ||
		strings.Contains(text, "needs new migration") ||
		strings.Contains(text, "migration_needed")
}

func findSelectedNode(nodes []api.NodeStatus, selectedName string) *api.NodeStatus {
	if len(nodes) == 0 {
		return nil
	}
	if selectedName == "" {
		return &nodes[0]
	}
	for i := range nodes {
		if nodes[i].Name == selectedName {
			return &nodes[i]
		}
	}
	return &nodes[0]
}

func resolveInstance(worktreeFlag, instanceID string) (string, string, error) {
	if instanceID != "" {
		items, err := instance.List()
		if err != nil {
			return "", "", err
		}
		for _, item := range items {
			if item.ID == instanceID {
				return item.Worktree, item.ID, nil
			}
		}
		return "", "", fmt.Errorf("unknown instance %q", instanceID)
	}
	worktree, err := resolveWorktree(worktreeFlag)
	if err != nil {
		return "", "", err
	}
	id, real, err := instance.IDForWorktree(worktree)
	if err != nil {
		return "", "", err
	}
	return real, id, nil
}

func resolveWorktree(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	return os.Getwd()
}

func readLastLines(path string, limit int) ([]string, error) {
	info, err := readLastLinesInfo(path, limit)
	return info.lines, err
}

type logTailInfo struct {
	lines     []string
	startLine int
	endLine   int
	truncated bool
}

func readLastLinesInfo(path string, limit int) (logTailInfo, error) {
	if path == "" {
		return logTailInfo{}, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return logTailInfo{}, err
	}
	defer file.Close()
	if limit <= 0 {
		return logTailInfo{}, nil
	}
	// Stream the file through a fixed-size reader and a fixed-count ring. The
	// full scan supplies exact retained line numbers without ever loading a
	// large task log (or one pathological line) into memory.
	reader := bufio.NewReaderSize(file, 64*1024)
	ring := make([]string, 0, limit)
	total := 0
	for {
		line, readErr := readBoundedTUILogLine(reader, 16*1024)
		if readErr == io.EOF && line == "" {
			break
		}
		if readErr != nil && readErr != io.EOF {
			return logTailInfo{}, readErr
		}
		total++
		if len(ring) == limit {
			copy(ring, ring[1:])
			ring[len(ring)-1] = line
		} else {
			ring = append(ring, line)
		}
		if readErr == io.EOF {
			break
		}
	}
	start := 0
	if len(ring) > 0 {
		start = total - len(ring) + 1
	}
	return logTailInfo{lines: ring, startLine: start, endLine: total, truncated: total > len(ring)}, nil
}

func readBoundedTUILogLine(reader *bufio.Reader, maxBytes int) (string, error) {
	const suffix = "… [line truncated]"
	kept := make([]byte, 0, min(maxBytes, 1024))
	truncated := false
	sawData := false
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > 0 {
			sawData = true
			if fragment[len(fragment)-1] == '\n' {
				fragment = fragment[:len(fragment)-1]
			}
			if len(kept) < maxBytes {
				count := min(len(fragment), maxBytes-len(kept))
				kept = append(kept, fragment[:count]...)
				truncated = truncated || count < len(fragment)
			} else if len(fragment) > 0 {
				truncated = true
			}
		}
		switch err {
		case nil:
			text := strings.TrimSuffix(string(kept), "\r")
			if truncated {
				text = truncateTUILogLine(text, maxBytes-len(suffix)) + suffix
			}
			return text, nil
		case bufio.ErrBufferFull:
			continue
		case io.EOF:
			if !sawData {
				return "", io.EOF
			}
			text := strings.TrimSuffix(string(kept), "\r")
			if truncated {
				text = truncateTUILogLine(text, maxBytes-len(suffix)) + suffix
			}
			return text, io.EOF
		default:
			return "", err
		}
	}
}

func truncateTUILogLine(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	value = value[:maxBytes]
	for value != "" && !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func supervisorStatus(inst *api.Instance) *api.SupervisorStatus {
	if inst == nil || inst.Supervisor.PID <= 0 {
		return nil
	}
	return &api.SupervisorStatus{
		PID:       inst.Supervisor.PID,
		ExecPID:   inst.Supervisor.ExecPID,
		Alive:     instance.ProcessAlive(inst.Supervisor.PID),
		StartedAt: inst.Supervisor.StartedAt,
		LogPath:   inst.Supervisor.LogPath,
	}
}

func instanceURLs(inst *api.Instance) map[string]string {
	if inst == nil {
		return nil
	}
	urls := map[string]string{}
	if port := inst.Ports["backend"]; port > 0 {
		urls["backend"] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	if port := inst.Ports["frontend"]; port > 0 {
		urls["frontend"] = fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	return urls
}

func stringSliceContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func markAllStoppedNodes(worktree, instanceID string) error {
	state, err := instance.LoadStatus(worktree, instanceID)
	if err != nil {
		return nil
	}
	for name, node := range state.Nodes {
		switch node.State {
		case api.StatePending, api.StateReady, api.StateRunning, api.StateDirty:
			node.State = api.StateStopped
			node.PID = 0
			state.Nodes[name] = node
		}
	}
	return instance.SaveStatus(worktree, instanceID, state.Target, state.Mode, state.Nodes)
}

func invalidateAndRerunDownstream(root, instanceID, task string, onTransition func()) error {
	_ = instanceID
	if onTransition != nil {
		onTransition()
	}
	_, err := callDaemonForTUI(context.Background(), root, daemon.Request{Action: daemon.ActionInvalidate, Task: task}, nil)
	return err
}

func previewLifecycleForTUI(root string, req daemon.Request) (api.LifecyclePlan, error) {
	req.Preview = true
	resp, err := callDaemonForTUI(context.Background(), root, req, nil)
	if err != nil {
		return api.LifecyclePlan{}, err
	}
	if resp.Lifecycle == nil {
		return api.LifecyclePlan{}, fmt.Errorf("daemon returned no lifecycle plan")
	}
	return resp.Lifecycle.Plan, nil
}

func retargetAndRelaunch(root, instanceID, task string) error {
	_ = instanceID
	_, err := callDaemonForTUI(context.Background(), root, daemon.Request{Action: daemon.ActionRetarget, Target: task}, nil)
	return err
}

func resolveRelaunchProject(worktree string, inst *api.Instance) (string, project.Project, error) {
	if inst != nil {
		name := strings.TrimSpace(inst.LastRun.Project)
		if name != "" {
			if p, err := project.Lookup(name); err == nil {
				return name, p, nil
			}
		}
	}
	names := project.Names()
	if len(names) == 1 {
		p, err := project.Lookup(names[0])
		if err != nil {
			return "", nil, err
		}
		return names[0], p, nil
	}
	if strings.TrimSpace(worktree) != "" {
		p, err := project.Detect(worktree)
		if err == nil {
			return p.Name(), p, nil
		}
	}
	if inst != nil && strings.TrimSpace(inst.LastRun.Project) != "" {
		return "", nil, fmt.Errorf("instance recorded project %q is no longer registered", inst.LastRun.Project)
	}
	return "", nil, fmt.Errorf("unable to resolve project for relaunch")
}

func stateBadge(state api.NodeState) string {
	switch state {
	case api.StatePending:
		return "WAIT"
	case api.StateStarting:
		return "START"
	case api.StateReady:
		return "READY"
	case api.StateRunning:
		return "RUN"
	case api.StateRestarting:
		return "RSTR"
	case api.StateDirty:
		return "DIRTY"
	case api.StateCached:
		return "CACHE"
	case api.StateDone:
		return "DONE"
	case api.StateFailed:
		return "FAIL"
	case api.StateMigrationNeeded:
		return "MIGR"
	case api.StateCanceled:
		return "CANC"
	case api.StateStopped:
		return "STOP"
	case api.StateBlocked:
		return "BLOCK"
	case api.StateDegraded:
		return "DEGR"
	case api.StateSkipped:
		return "SKIP"
	default:
		return string(state)
	}
}

func stateColor(state api.NodeState) tcell.Color {
	switch state {
	case api.StateRunning, api.StateReady:
		return tcell.ColorLightGreen
	case api.StateStarting, api.StateRestarting:
		return tcell.ColorLightCyan
	case api.StatePending, api.StateDirty:
		return tcell.ColorYellow
	case api.StateCached:
		return tcell.ColorLightBlue
	case api.StateDone:
		return tcell.ColorWhite
	case api.StateFailed, api.StateDegraded, api.StateBlocked:
		return tcell.ColorIndianRed
	case api.StateMigrationNeeded:
		return tcell.ColorLightYellow
	case api.StateCanceled:
		return tcell.ColorOrange
	case api.StateStopped:
		return tcell.ColorGray
	case api.StateSkipped:
		return tcell.ColorDarkGray
	default:
		return tcell.ColorWhite
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
