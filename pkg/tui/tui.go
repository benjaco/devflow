package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"

	"devflow/internal/fsutil"
	"devflow/pkg/api"
	"devflow/pkg/cache"
	"devflow/pkg/graph"
	"devflow/pkg/instance"
	"devflow/pkg/process"
	"devflow/pkg/project"
	"devflow/pkg/watch"
)

type Options struct {
	Worktree   string
	InstanceID string
}

type snapshot struct {
	instance   *api.Instance
	state      *instance.State
	nodes      []api.NodeStatus
	supervisor *api.SupervisorStatus
	urls       map[string]string
	logTitle   string
	logLines   []string
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
	showSupervisorLog bool
	selectedName      string
	currentNodes      []api.NodeStatus
	statusMessage     string
	busy              bool
	eventOffset       int64
	activePromptID    string
}

const (
	fallbackRefreshInterval = 2 * time.Second
)

func Run(opts Options) error {
	root, id, err := resolveInstance(opts.Worktree, opts.InstanceID)
	if err != nil {
		return err
	}
	d := newDashboard(root, id)
	if err := d.refresh(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.eventLoop(ctx)
	go d.fallbackRefreshLoop(ctx)
	return d.app.Run()
}

func newDashboard(root, instanceID string) *dashboard {
	d := &dashboard{
		root:       root,
		instanceID: instanceID,
		eventsPath: instance.EventsPath(root, instanceID),
		app:        tview.NewApplication(),
		header:     tview.NewTextView(),
		tasks:      tview.NewTable(),
		logs:       tview.NewTextView(),
		footer:     tview.NewTextView(),
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
		d.updateLogs()
	})

	d.logs.
		SetDynamicColors(true).
		SetWrap(false).
		SetBorder(true).
		SetTitle(" Logs ")

	d.footer.
		SetDynamicColors(true).
		SetWrap(false).
		SetBorder(true).
		SetTitle(" Keys ")

	layout := tview.NewFlex().SetDirection(tview.FlexRow).
		AddItem(d.header, 5, 0, false).
		AddItem(d.tasks, 0, 2, true).
		AddItem(d.logs, 0, 3, false).
		AddItem(d.footer, 3, 0, false)
	d.pages = tview.NewPages().
		AddPage("main", layout, true, true)

	d.setStatus("[green]ready")

	d.app.SetRoot(d.pages, true)
	d.app.SetFocus(d.tasks)
	d.app.SetInputCapture(d.handleKeys)
	return d
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
		PollInterval: 40 * time.Millisecond,
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
		case <-errs:
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

func (d *dashboard) handleKeys(event *tcell.EventKey) *tcell.EventKey {
	switch event.Key() {
	case tcell.KeyEsc:
		d.app.Stop()
		return nil
	case tcell.KeyRune:
		switch event.Rune() {
		case 'q':
			d.app.Stop()
			return nil
		case 'j':
			d.moveSelection(1)
			return nil
		case 'k':
			d.moveSelection(-1)
			return nil
		case 'g':
			d.selectIndex(0)
			return nil
		case 'G':
			d.selectIndex(len(d.currentNodes) - 1)
			return nil
		case 'l':
			d.showSupervisorLog = !d.showSupervisorLog
			d.updateLogs()
			return nil
		case 'i':
			d.triggerInvalidateSelected()
			return nil
		case 't':
			d.triggerRetargetSelected()
			return nil
		}
	case tcell.KeyDown:
		d.moveSelection(1)
		return nil
	case tcell.KeyUp:
		d.moveSelection(-1)
		return nil
	}
	return event
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
	d.tasks.Select(index+1, 0)
	d.updateLogs()
}

func (d *dashboard) refresh() error {
	snap, err := loadSnapshot(d.root, d.instanceID, d.showSupervisorLog, d.selectedName)
	if err != nil {
		d.header.SetText(fmt.Sprintf("[red]failed to load instance state: %v", err))
		return err
	}
	d.currentNodes = snap.nodes
	d.header.SetText(strings.Join(renderHeader(snap), "\n"))
	d.renderTasks(snap.nodes)
	d.reconcileSelection()
	d.updateLogsFromSnapshot(snap)
	d.renderFooter()
	return nil
}

func (d *dashboard) renderTasks(nodes []api.NodeStatus) {
	d.tasks.Clear()
	headers := []string{"STATE", "TASK", "KIND"}
	for col, header := range headers {
		d.tasks.SetCell(0, col, tview.NewTableCell(header).
			SetSelectable(false).
			SetAttributes(tcell.AttrBold).
			SetTextColor(tcell.ColorWhite))
	}
	for row, node := range nodes {
		state := stateBadge(node.State)
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
	snap, err := loadSnapshot(d.root, d.instanceID, d.showSupervisorLog, d.selectedName)
	if err != nil {
		d.logs.SetTitle(" Logs ")
		d.logs.SetText(fmt.Sprintf("failed to load logs: %v", err))
		return
	}
	d.updateLogsFromSnapshot(snap)
}

func (d *dashboard) updateLogsFromSnapshot(snap snapshot) {
	d.logs.SetTitle(" " + snap.logTitle + " ")
	lines := renderLogPanel(snap, d.selectedName)
	d.logs.SetText(strings.Join(lines, "\n"))
}

func (d *dashboard) setStatus(msg string) {
	d.statusMessage = msg
	d.renderFooter()
}

func (d *dashboard) renderFooter() {
	status := d.statusMessage
	if status == "" {
		status = "[green]ready"
	}
	d.footer.SetText("q quit  j/k move  g/G top/bottom  l toggle selected/supervisor log  i invalidate+rerun downstream  t retarget to selected task\n" + status)
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
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]invalidating from %s and relaunching target...", node.Name))
	_ = d.refresh()
	selected := node.Name
	go func() {
		err := invalidateAndRerunDownstream(d.root, d.instanceID, selected, func() {
			d.app.QueueUpdateDraw(func() {
				d.setStatus(fmt.Sprintf("[yellow]invalidated downstream from %s, relaunching...", selected))
				_ = d.refresh()
			})
		})
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]invalidate+rerun failed: %v", err))
				return
			}
			d.setStatus(fmt.Sprintf("[green]invalidated downstream from %s and relaunched target", selected))
			_ = d.refresh()
		})
	}()
}

func (d *dashboard) triggerRetargetSelected() {
	if d.busy {
		d.setStatus("[yellow]action already running")
		return
	}
	node := findSelectedNode(d.currentNodes, d.selectedName)
	if node == nil {
		d.setStatus("[red]no task selected")
		return
	}
	d.busy = true
	d.setStatus(fmt.Sprintf("[yellow]retargeting detached run to %s...", node.Name))
	_ = d.refresh()
	selected := node.Name
	go func() {
		err := retargetAndRelaunch(d.root, d.instanceID, selected)
		d.app.QueueUpdateDraw(func() {
			d.busy = false
			if err != nil {
				d.setStatus(fmt.Sprintf("[red]retarget failed: %v", err))
				return
			}
			d.setStatus(fmt.Sprintf("[green]retargeted detached run to %s", selected))
			_ = d.refresh()
		})
	}()
}

func loadSnapshot(root, instanceID string, showSupervisor bool, selectedName string) (snapshot, error) {
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return snapshot{}, err
	}
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		return snapshot{}, err
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

	nodes := make([]api.NodeStatus, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		left := taskStatePriority(nodes[i].State)
		right := taskStatePriority(nodes[j].State)
		if left != right {
			return left < right
		}
		return nodes[i].Name < nodes[j].Name
	})

	selected := findSelectedNode(nodes, selectedName)
	logTitle := "selected log"
	logPath := ""
	if showSupervisor {
		logTitle = "supervisor log"
		if supervisor != nil {
			logPath = supervisor.LogPath
		}
	} else if selected != nil {
		logTitle = selected.Name + " log"
		logPath = selected.LogPath
	}
	logLines, _ := readLastLines(logPath, 200)

	return snapshot{
		instance:   inst,
		state:      state,
		nodes:      nodes,
		supervisor: supervisor,
		urls:       instanceURLs(inst),
		logTitle:   logTitle,
		logLines:   logLines,
	}, nil
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
		fmt.Sprintf("[yellow]%s[-]    [yellow]states[-]: RUN=%d WAIT=%d CACHE=%d DONE=%d FAIL=%d STOP=%d",
			supervisorText,
			counts[api.StateRunning],
			counts[api.StatePending]+counts[api.StateReady]+counts[api.StateDirty],
			counts[api.StateCached],
			counts[api.StateDone],
			counts[api.StateFailed],
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
			if evt.State == api.StateFailed && evt.Task != "" {
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
	d.pages.RemovePage("prompt")
	d.app.SetFocus(d.tasks)
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
	lines := []string{}
	if snap.logTitle == "supervisor log" {
		lines = append(lines, "selected: supervisor")
	} else if node := findSelectedNode(snap.nodes, selectedName); node != nil {
		lines = append(lines, fmt.Sprintf("selected: %s    kind=%s    state=%s", node.Name, node.Kind, node.State))
		if node.PID > 0 {
			lines = append(lines, fmt.Sprintf("pid=%d", node.PID))
		}
		if node.LastRunKey != "" {
			lines = append(lines, fmt.Sprintf("key=%s", node.LastRunKey))
		}
		if node.LastError != "" {
			lines = append(lines, fmt.Sprintf("error=%s", node.LastError))
		}
	}
	lines = append(lines, "")
	if len(snap.logLines) == 0 {
		lines = append(lines, "no log lines yet")
		return lines
	}
	lines = append(lines, snap.logLines...)
	return lines
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
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := strings.TrimRight(string(data), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > limit {
		lines = lines[len(lines)-limit:]
	}
	return lines, nil
}

func supervisorStatus(inst *api.Instance) *api.SupervisorStatus {
	if inst == nil || inst.Supervisor.PID <= 0 {
		return nil
	}
	return &api.SupervisorStatus{
		PID:       inst.Supervisor.PID,
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
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return err
	}
	if inst.LastRun.Project == "" || inst.LastRun.Target == "" {
		return fmt.Errorf("instance has no recorded project/target to relaunch")
	}
	g, resolvedTarget, err := executionGraph(inst.LastRun.Project, inst.LastRun.Target)
	if err != nil {
		return err
	}
	toInvalidate, err := downstreamInvalidateTasks(g, resolvedTarget, task)
	if err != nil {
		return err
	}
	if err := writeInvalidateTransition(root, instanceID, resolvedTarget, g, toInvalidate); err != nil {
		return err
	}
	if onTransition != nil {
		onTransition()
	}
	store := cache.New(instance.CacheRoot(root))
	for _, name := range toInvalidate {
		if err := store.Invalidate(name); err != nil {
			return err
		}
	}
	if inst.Supervisor.PID > 0 {
		supervisorPID := inst.Supervisor.PID
		if err := instance.StopSupervisor(inst); err != nil {
			return err
		}
		waitForPIDExit(supervisorPID, 5*time.Second)
	}
	_, err = launchDetached(root, inst, inst.LastRun.Target, inst.LastRun.Project, inst.LastRun.Mode, inst.LastRun.MaxParallel)
	return err
}

func retargetAndRelaunch(root, instanceID, task string) error {
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return err
	}
	if inst.LastRun.Project == "" {
		return fmt.Errorf("instance has no recorded project to relaunch")
	}
	p, err := project.Lookup(inst.LastRun.Project)
	if err != nil {
		return err
	}
	if _, _, err := project.ResolveExecutionProject(p, task); err != nil {
		return err
	}
	if inst.Supervisor.PID > 0 {
		supervisorPID := inst.Supervisor.PID
		if err := instance.StopSupervisor(inst); err != nil {
			return err
		}
		waitForPIDExit(supervisorPID, 5*time.Second)
	}
	_, err = launchDetached(root, inst, task, inst.LastRun.Project, inst.LastRun.Mode, inst.LastRun.MaxParallel)
	return err
}

func downstreamInvalidateTasks(g *graph.Graph, target, selected string) ([]string, error) {
	closure, err := g.TargetClosure(target)
	if err != nil {
		return nil, err
	}
	inClosure := map[string]bool{}
	for _, name := range closure {
		inClosure[name] = true
	}
	selectedTask, ok := g.Tasks[selected]
	if !ok {
		return nil, fmt.Errorf("unknown task %q", selected)
	}
	candidates := []string{}
	if selectedTask.Kind == project.KindGroup {
		candidates = g.Upstream([]string{selected})
	} else {
		candidates = g.Downstream([]string{selected})
	}
	out := collectInvalidateTasks(g, inClosure, candidates)
	sort.Strings(out)
	return out, nil
}

func collectInvalidateTasks(g *graph.Graph, inClosure map[string]bool, names []string) []string {
	out := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if !inClosure[name] || seen[name] {
			continue
		}
		task := g.Tasks[name]
		if task.Kind == project.KindOnce && task.Cache {
			out = append(out, name)
			seen[name] = true
		}
	}
	return out
}

func writeInvalidateTransition(root, instanceID, target string, g *graph.Graph, invalidated []string) error {
	state, err := instance.LoadStatus(root, instanceID)
	if err != nil {
		return err
	}
	impacted, err := impactedRerunTasks(g, target, invalidated)
	if err != nil {
		return err
	}
	invalidatedSet := make(map[string]bool, len(invalidated))
	for _, name := range invalidated {
		invalidatedSet[name] = true
	}
	impactedSet := make(map[string]bool, len(impacted))
	for _, name := range impacted {
		impactedSet[name] = true
	}
	for name, node := range state.Nodes {
		if invalidatedSet[name] {
			node.State = api.StateDirty
			node.LastRunKey = ""
			node.LastError = ""
			node.PID = 0
			state.Nodes[name] = node
			continue
		}
		if !impactedSet[name] {
			continue
		}
		node.LastError = ""
		node.PID = 0
		switch node.Kind {
		case string(project.KindService):
			node.State = api.StatePending
		case string(project.KindGroup), string(project.KindWarmup), string(project.KindOnce):
			if node.State != api.StateDirty {
				node.State = api.StatePending
			}
		}
		state.Nodes[name] = node
	}
	return instance.SaveStatus(root, instanceID, state.Target, state.Mode, state.Nodes)
}

func impactedRerunTasks(g *graph.Graph, target string, invalidated []string) ([]string, error) {
	closure, err := g.TargetClosure(target)
	if err != nil {
		return nil, err
	}
	inClosure := make(map[string]bool, len(closure))
	for _, name := range closure {
		inClosure[name] = true
	}
	downstream := g.Downstream(invalidated)
	out := make([]string, 0, len(downstream))
	seen := make(map[string]bool, len(downstream))
	for _, name := range downstream {
		if !inClosure[name] || seen[name] {
			continue
		}
		out = append(out, name)
		seen[name] = true
	}
	sort.Strings(out)
	return out, nil
}

func executionGraph(projectName, target string) (*graph.Graph, string, error) {
	p, err := project.Lookup(projectName)
	if err != nil {
		return nil, "", err
	}
	execProject, resolvedTarget, err := project.ResolveExecutionProject(p, target)
	if err != nil {
		return nil, "", err
	}
	g, err := graph.New(execProject.Tasks(), execProject.Targets())
	if err != nil {
		return nil, "", err
	}
	return g, resolvedTarget, nil
}

func launchDetached(root string, inst *api.Instance, target, projectName string, mode api.RunMode, maxParallel int) (int, error) {
	executable, err := detachedExecutable(root)
	if err != nil {
		return 0, err
	}
	logPath := filepath.Join(root, ".devflow", "logs", inst.ID, "supervisor.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return 0, err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer logFile.Close()

	cmd := exec.Command(executable,
		"__internal_supervise",
		"--target", target,
		"--project", projectName,
		"--worktree", root,
		"--mode", string(mode),
		"--log-path", logPath,
	)
	if maxParallel > 0 {
		cmd.Args = append(cmd.Args, "--max-parallel", fmt.Sprintf("%d", maxParallel))
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Dir = root
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if err := instance.RecordDetachedRun(inst, api.RunConfig{
		Project:     projectName,
		Target:      target,
		Mode:        mode,
		MaxParallel: maxParallel,
		Detached:    true,
	}, cmd.Process.Pid, logPath); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func detachedExecutable(worktree string) (string, error) {
	current, err := os.Executable()
	if err != nil {
		return "", err
	}
	target := filepath.Join(worktree, ".devflow", "bin", "devflow-launcher")
	if err := fsutil.CopyFile(current, target); err != nil {
		return "", err
	}
	if err := os.Chmod(target, 0o755); err != nil {
		return "", err
	}
	return target, nil
}

func waitForPIDExit(pid int, timeout time.Duration) {
	if pid <= 0 || timeout <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func stateBadge(state api.NodeState) string {
	switch state {
	case api.StateRunning:
		return "RUN"
	case api.StatePending, api.StateReady, api.StateDirty:
		return "WAIT"
	case api.StateCached:
		return "CACHE"
	case api.StateDone:
		return "DONE"
	case api.StateFailed:
		return "FAIL"
	case api.StateStopped:
		return "STOP"
	case api.StateSkipped:
		return "SKIP"
	default:
		return string(state)
	}
}

func stateColor(state api.NodeState) tcell.Color {
	switch state {
	case api.StateRunning:
		return tcell.ColorLightGreen
	case api.StatePending, api.StateReady, api.StateDirty:
		return tcell.ColorYellow
	case api.StateCached:
		return tcell.ColorLightBlue
	case api.StateDone:
		return tcell.ColorWhite
	case api.StateFailed:
		return tcell.ColorIndianRed
	case api.StateStopped:
		return tcell.ColorGray
	case api.StateSkipped:
		return tcell.ColorDarkGray
	default:
		return tcell.ColorWhite
	}
}

func taskStatePriority(state api.NodeState) int {
	switch state {
	case api.StateRunning:
		return 0
	case api.StatePending, api.StateReady, api.StateDirty:
		return 1
	case api.StateFailed:
		return 2
	case api.StateCached:
		return 3
	case api.StateDone:
		return 4
	case api.StateStopped:
		return 5
	case api.StateSkipped:
		return 6
	default:
		return 7
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
