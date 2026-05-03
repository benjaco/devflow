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

	"github.com/benjaco/devflow/internal/fsutil"
	"github.com/benjaco/devflow/pkg/api"
	"github.com/benjaco/devflow/pkg/cache"
	"github.com/benjaco/devflow/pkg/database"
	"github.com/benjaco/devflow/pkg/graph"
	"github.com/benjaco/devflow/pkg/instance"
	"github.com/benjaco/devflow/pkg/process"
	"github.com/benjaco/devflow/pkg/project"
	"github.com/benjaco/devflow/pkg/watch"
)

type Options struct {
	Worktree   string
	InstanceID string
}

type snapshot struct {
	instance     *api.Instance
	state        *instance.State
	nodes        []api.NodeStatus
	supervisor   *api.SupervisorStatus
	urls         map[string]string
	logTitle     string
	logLines     []string
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

type tuiDatabaseManager interface {
	PreparePrismaMigrationAuthoringDatabase(context.Context, database.PrismaMigrationAuthoringOptions) (*database.PrismaMigrationAuthoringResult, error)
}

var newDatabaseManagerForTUI = func() tuiDatabaseManager {
	return database.New()
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
	showDatabasePanel bool
	selectedName      string
	currentNodes      []api.NodeStatus
	statusMessage     string
	busy              bool
	eventOffset       int64
	activePromptID    string
	activeInput       bool
}

const (
	fallbackRefreshInterval = 2 * time.Second
	databasePanelTitle      = "database / prisma"
	supervisorLogTitle      = "supervisor log"
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
		case tview.MouseScrollUp, tview.MouseScrollDown, tview.MouseLeftClick:
			d.app.SetFocus(d.logs)
		}
		return action, event
	})

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

	d.app.EnableMouse(true)
	d.app.SetRoot(d.pages, true)
	d.app.SetFocus(d.logs)
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
	if d.activeInput {
		return event
	}
	switch event.Key() {
	case tcell.KeyEsc:
		d.app.Stop()
		return nil
	case tcell.KeyHome:
		d.selectIndex(0)
		return nil
	case tcell.KeyEnd:
		d.selectIndex(len(d.currentNodes) - 1)
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
		d.moveSelection(1)
		return nil
	case tcell.KeyUp:
		d.moveSelection(-1)
		return nil
	}
	return event
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
	d.tasks.Select(index+1, 0)
	d.updateLogs()
}

func (d *dashboard) refresh() error {
	snap, err := loadSnapshot(d.root, d.instanceID, d.showSupervisorLog, d.showDatabasePanel, d.selectedName)
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
	snap, err := loadSnapshot(d.root, d.instanceID, d.showSupervisorLog, d.showDatabasePanel, d.selectedName)
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

func (d *dashboard) scrollLogs(delta int) {
	if delta == 0 {
		return
	}
	row, col := d.logs.GetScrollOffset()
	nextRow := row + delta
	if nextRow < 0 {
		nextRow = 0
	}
	d.logs.ScrollTo(nextRow, col)
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
	d.footer.SetText("q quit  j/k/arrows move  g/G top/bottom  l log  d db  m migration  i rerun  t retarget\n" + status)
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
	if !cfg.Available {
		d.setStatus("[red]no Prisma schema detected or configured")
		return
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
		AddText("Create Prisma Migration", true, tview.AlignCenter, tcell.ColorWhite).
		AddText("Enter creates the migration. Escape cancels.", false, tview.AlignCenter, tcell.ColorGray).
		AddText(fmt.Sprintf("%s -> %s", cfg.SchemaPath, cfg.MigrationsDir), false, tview.AlignCenter, tcell.ColorGray)
	d.activeInput = true
	d.pages.AddPage("prisma_migration", centered(frame, 84, 8), true, true)
	d.app.SetFocus(input)
}

func (d *dashboard) closePrismaMigrationPrompt() {
	d.activeInput = false
	d.pages.RemovePage("prisma_migration")
	d.app.SetFocus(d.logs)
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
	d.setStatus(fmt.Sprintf("[yellow]creating Prisma migration %q...", name))
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
				d.setStatus(fmt.Sprintf("[red]Prisma migration failed: %v", err))
				_ = d.refresh()
				return
			}
			d.setStatus(fmt.Sprintf("[green]created Prisma migration %q", name))
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

func loadSnapshot(root, instanceID string, showSupervisor bool, showDatabase bool, selectedName string) (snapshot, error) {
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
	logLines, _ := readLastLines(logPath, 200)

	return snapshot{
		instance:     inst,
		state:        state,
		nodes:        nodes,
		supervisor:   supervisor,
		urls:         instanceURLs(inst),
		logTitle:     logTitle,
		logLines:     logLines,
		prisma:       prisma,
		prismaErr:    prismaErr,
		prismaCfg:    prismaCfg,
		prismaDev:    prismaDev,
		prismaDevErr: prismaDevErr,
	}, nil
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
		fmt.Sprintf("[yellow]%s[-]    [yellow]states[-]: RUN=%d WAIT=%d CACHE=%d DONE=%d MIGR=%d FAIL=%d CANC=%d STOP=%d",
			supervisorText,
			counts[api.StateRunning],
			counts[api.StatePending]+counts[api.StateReady]+counts[api.StateDirty],
			counts[api.StateCached],
			counts[api.StateDone],
			counts[api.StateMigrationNeeded],
			counts[api.StateFailed],
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
	lines = append(lines, fmt.Sprintf("name=%s host=%s port=%d user=%s image=%s", db.Name, db.Host, db.Port, db.User, db.Image))
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
	reportTUIProgress(progress, "loading Prisma migration configuration...")
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return err
	}
	cfg, err := resolvePrismaConfig(root, inst)
	if err != nil {
		return err
	}
	if !cfg.Available {
		return fmt.Errorf("no Prisma schema detected or configured")
	}
	env := project.MergeEnvMaps(inst.Env, map[string]string{
		"DEVFLOW_MIGRATION_NAME": name,
	})
	if env["DATABASE_URL"] == "" && inst.DB.URL != "" {
		env["DATABASE_URL"] = inst.DB.URL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	logPath := instance.LogPath(root, instanceID, "prisma_migration")
	if err := preparePrismaMigrationAuthoringDatabaseForTUI(ctx, root, inst.DB, cfg, env, logPath, progress); err != nil {
		return err
	}
	reportTUIProgress(progress, "running Prisma migration generation for %q...", name)
	return database.GeneratePrismaMigration(ctx, database.PrismaMigrationGenerateOptions{
		Worktree:   root,
		SchemaPath: cfg.SchemaPath,
		Name:       name,
		CreateOnly: cfg.CreateOnly,
		Env:        env,
		LogPath:    logPath,
		Command:    cfg.Command,
		OnLine: func(stream, line string) {
			line = strings.TrimSpace(line)
			if line == "" {
				return
			}
			reportTUIProgress(progress, "Prisma %s: %s", stream, compactTUIStatus(line, 120))
		},
	})
}

func preparePrismaMigrationAuthoringDatabaseForTUI(ctx context.Context, root string, db api.DBInstance, cfg tuiPrismaConfig, env map[string]string, logPath string, progress func(string)) error {
	if db.ContainerName == "" && db.VolumeName == "" {
		return nil
	}
	manager := newDatabaseManagerForTUI()
	reportTUIProgress(progress, "reconciling Prisma migration database...")
	result, err := manager.PreparePrismaMigrationAuthoringDatabase(ctx, database.PrismaMigrationAuthoringOptions{
		Worktree:      root,
		DB:            db,
		SchemaPath:    cfg.SchemaPath,
		MigrationsDir: cfg.MigrationsDir,
		BasePaths:     cfg.BasePaths,
		Prepare: database.PrepareOptions{
			Worktree: root,
			Env:      env,
			LogPath:  logPath,
			OnLine: func(stream, line string) {
				line = strings.TrimSpace(line)
				if line == "" {
					return
				}
				reportTUIProgress(progress, "Prisma database %s: %s", stream, compactTUIStatus(line, 120))
			},
		},
	})
	if err != nil {
		return fmt.Errorf("prepare Prisma migration database: %w", err)
	}
	summary := database.SummarizePrismaMigrationAuthoring(result)
	reportTUIProgress(progress, "managed database is ready; restored=%v applied=%v prefix=%d", summary.Restored, summary.Applied, summary.PrefixLength)
	return nil
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
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return err
	}
	projectName, p, err := resolveRelaunchProject(root, inst)
	if err != nil {
		return err
	}
	if inst.LastRun.Target == "" {
		return fmt.Errorf("instance has no recorded project/target to relaunch")
	}
	g, resolvedTarget, err := executionGraphForProject(p, inst.LastRun.Target)
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
	store := cache.NewNamespaced(instance.CacheRoot(), project.CacheNamespace(p))
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
	_, err = launchDetached(root, inst, inst.LastRun.Target, projectName, inst.LastRun.Mode, inst.LastRun.MaxParallel)
	return err
}

func retargetAndRelaunch(root, instanceID, task string) error {
	inst, err := instance.Load(root, instanceID)
	if err != nil {
		return err
	}
	projectName, p, err := resolveRelaunchProject(root, inst)
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
	_, err = launchDetached(root, inst, task, projectName, inst.LastRun.Mode, inst.LastRun.MaxParallel)
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
	return executionGraphForProject(p, target)
}

func executionGraphForProject(p project.Project, target string) (*graph.Graph, string, error) {
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
	case api.StateMigrationNeeded:
		return "MIGR"
	case api.StateCanceled:
		return "CANC"
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

func taskStatePriority(state api.NodeState) int {
	switch state {
	case api.StateRunning:
		return 0
	case api.StatePending, api.StateReady, api.StateDirty:
		return 1
	case api.StateMigrationNeeded:
		return 2
	case api.StateFailed:
		return 3
	case api.StateCanceled:
		return 4
	case api.StateCached:
		return 5
	case api.StateDone:
		return 6
	case api.StateStopped:
		return 7
	case api.StateSkipped:
		return 8
	default:
		return 9
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
