// Package tui is the Bubble Tea dashboard and wizard.
package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/engine"
	"github.com/chriha/tines/internal/runner"
	"github.com/chriha/tines/internal/state"
)

type screen int

const (
	scDash screen = iota
	scWizard
	scPlan
	scTyped
	scEdit
	scMenu
	scInfo
	scHelp
	scTargets
	scTgWizard
	scPreview
)

type (
	tickMsg     time.Time
	pingTickMsg time.Time
	connMsg     struct {
		key string
		c   conn
	}
	prepMsg struct {
		sid string
		ps  []*runner.Prepared
	}
	progMsg struct {
		tid string
		p   engine.Progress
	}
	doneMsg struct {
		tid string
		res runner.Result
	}
	finishedMsg struct{ sid string }
	checkMsg    struct {
		tid  string
		err  error
		plan engine.Plan
	}
	verifyMsg struct {
		tid, name string
		v         engine.VerifyResult
		err       error
	}
)

type model struct {
	env      *runner.Env
	w, h     int
	frame    int
	sel      int
	tsel     int
	screen   screen
	conns    map[string]conn
	inflight map[string]bool
	runs     map[string]*runView
	checking map[string]bool
	autoChk  map[string]bool
	capTried map[string]time.Time // last capacity read attempt per target
	forceCap map[string]bool      // read capacity on the next ping regardless of age
	running  map[string]context.CancelFunc
	ch       chan tea.Msg

	msg     string
	msgErr  bool
	quitArm bool

	plan          *planView
	typ           *typed
	edit          *form
	menu          *menuView
	wiz           *wizard
	tv            targetsView
	tw            *tgWizard
	pvw           *previewView
	info          []string
	verifyPending int
	verifyOut     []string
}

type planView struct {
	stream    config.Stream
	preparing bool
	ps        []*runner.Prepared
	queue     []needRef
	skipped   map[int]bool
}

type needRef struct{ pi, ni int }

type menuView struct {
	title string
	pick  picker
	acts  []func(m *model) tea.Cmd
}

// Run starts the TUI.
func Run(env *runner.Env, wizard bool) error {
	m := &model{
		env: env, conns: map[string]conn{}, inflight: map[string]bool{}, runs: map[string]*runView{},
		checking: map[string]bool{}, autoChk: map[string]bool{}, capTried: map[string]time.Time{}, forceCap: map[string]bool{}, running: map[string]context.CancelFunc{},
		ch: make(chan tea.Msg, 512), tsel: -1,
	}
	if wizard || len(env.Cfg.Streams) == 0 {
		m.startWizard(nil)
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	for _, c := range m.running {
		c()
	}
	return err
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(tick(), pingTick(), m.listen(), m.pingAll(), m.autoCheck(), textinputBlink())
}

func tick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}
func pingTick() tea.Cmd {
	return tea.Tick(5*time.Second, func(t time.Time) tea.Msg { return pingTickMsg(t) })
}
func (m *model) listen() tea.Cmd { return func() tea.Msg { return <-m.ch } }

func (m *model) stream() *config.Stream {
	if m.sel < 0 || m.sel >= len(m.env.Cfg.Streams) {
		return nil
	}
	return &m.env.Cfg.Streams[m.sel]
}

func (m *model) say(s string)  { m.msg, m.msgErr = s, false }
func (m *model) fail(s string) { m.msg, m.msgErr = s, true }

func (m *model) save() bool {
	if err := m.env.Cfg.Save(m.env.CfgPath); err != nil {
		m.fail("could not save config: " + err.Error())
		return false
	}
	return true
}

// ---- background checks ------------------------------------------------

// capacityMaxAge is how old the stored capacity may get while a target stays
// connected. It is also refreshed after every run and on demand (r).
const capacityMaxAge = 30 * time.Second

func (m *model) pingCmd(key string, t *config.Target, spec string) tea.Cmd {
	if m.inflight[key] {
		return nil
	}
	m.inflight[key] = true
	readCap := false
	if t != nil {
		last := m.env.State.Device(t.ID).CapacityAt
		if tried := m.capTried[t.ID]; tried.After(last) {
			last = tried
		}
		if readCap = m.forceCap[t.ID] || time.Since(last) > capacityMaxAge; readCap {
			m.capTried[t.ID] = time.Now()
			delete(m.forceCap, t.ID)
		}
	}
	env := m.env
	return func() tea.Msg {
		c := pingConn(spec, 25*time.Second)
		if t != nil && c.Up() {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			if readCap {
				if _, ok := env.RefreshCapacity(ctx, *t); !ok {
					env.State.SeenConnected(t.ID)
				}
			} else if time.Since(env.State.Device(t.ID).LastSeen) > capacityMaxAge {
				env.State.SeenConnected(t.ID)
			}
		}
		return connMsg{key, c}
	}
}

func (m *model) pingAll() tea.Cmd {
	var cmds []tea.Cmd
	for _, s := range m.env.Cfg.Streams {
		cmds = append(cmds, m.pingCmd("src:"+s.ID, nil, s.Source.Spec))
	}
	for _, t := range m.env.Cfg.Targets {
		cmds = append(cmds, m.pingCmd("tgt:"+t.ID, &t, t.Spec))
	}
	return tea.Batch(cmds...)
}

func (m *model) checkStream(st config.Stream) tea.Cmd {
	var cmds []tea.Cmd
	for _, t := range st.Targets {
		if m.checking[t.ID] || m.runs[t.ID] != nil && !m.runs[t.ID].Done {
			continue
		}
		m.checking[t.ID] = true
		t := t
		cmds = append(cmds, func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			p, err := m.env.Check(ctx, st, t)
			return checkMsg{t.ID, err, p}
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) autoCheck() tea.Cmd {
	if st := m.stream(); st != nil && !m.autoChk[st.ID] {
		m.autoChk[st.ID] = true
		return m.checkStream(*st)
	}
	return nil
}

// ---- update ------------------------------------------------------------

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		m.frame++
		return m, tick()
	case pingTickMsg:
		return m, tea.Batch(pingTick(), m.pingAll())
	case connMsg:
		delete(m.inflight, msg.key)
		m.conns[msg.key] = msg.c
		return m, nil
	case checkMsg:
		delete(m.checking, msg.tid)
		return m, nil
	case prepMsg:
		return m, m.onPrepared(msg)
	case progMsg:
		m.onProgress(msg)
		return m, m.listen()
	case doneMsg:
		m.onDone(msg)
		return m, m.listen()
	case unmountMsg:
		return m, m.onUnmount(msg)
	case previewMsg:
		m.onPreview(msg)
		return m, nil
	case tgTestMsg:
		m.onTgTest(msg)
		return m, nil
	case finishedMsg:
		delete(m.running, msg.sid)
		if st := m.env.Cfg.Find(msg.sid); st != nil {
			for _, t := range st.Targets {
				m.forceCap[t.ID] = true // free space just changed
			}
		}
		m.say("backup finished")
		return m, tea.Batch(m.listen(), m.pingAll())
	case verifyMsg:
		return m, m.onVerify(msg)
	case wizTestMsg:
		return m, m.onWizTest(msg)
	case tea.KeyMsg:
		return m.onKey(msg)
	}
	// blink etc. forwarded to active inputs
	return m, m.forward(msg)
}

func (m *model) forward(msg tea.Msg) tea.Cmd {
	switch m.screen {
	case scWizard:
		return m.wizardForward(msg)
	case scEdit:
		_, c := m.edit.update(msg)
		return c
	case scTgWizard:
		return m.tgForward(msg)
	case scTyped:
		var c tea.Cmd
		m.typ.in, c = m.typ.in.Update(msg)
		return c
	}
	return nil
}

func (m *model) onProgress(p progMsg) {
	rv := m.runs[p.tid]
	if rv == nil {
		return
	}
	rv.Phase = "syncing"
	pr := p.p
	rv.Bytes, rv.Total, rv.Speed, rv.ETA, rv.Current = pr.Bytes, pr.TotalBytes, pr.Speed, pr.ETA, pr.Current
	if pr.TotalBytes > 0 {
		rv.Pct = int(100 * pr.Bytes / pr.TotalBytes)
	}
	if st := m.stream(); st != nil && st.Settings.VerifyAfter && pr.TotalBytes > 0 && pr.Bytes >= pr.TotalBytes {
		rv.Phase = "verifying"
	}
}

func (m *model) onDone(d doneMsg) {
	rv := m.runs[d.tid]
	if rv == nil {
		rv = &runView{}
		m.runs[d.tid] = rv
	}
	rv.Done, rv.OK, rv.Msg, rv.Warnings = true, d.res.OK, d.res.Message, d.res.Warnings
	if d.res.OK {
		rv.Pct = 100
	}
}

// ---- keys --------------------------------------------------------------

func (m *model) onKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if k.String() == "ctrl+c" {
		return m, m.quit()
	}
	switch m.screen {
	case scWizard:
		return m, m.wizardKey(k)
	case scPlan:
		return m, m.planKey(k)
	case scTyped:
		return m, m.typedKey(k)
	case scEdit:
		return m, m.editKey(k)
	case scMenu:
		return m, m.menuKey(k)
	case scTargets:
		return m, m.targetsKey(k)
	case scTgWizard:
		return m, m.tgKey(k)
	case scPreview:
		return m, m.previewKey(k)
	case scInfo, scHelp:
		if k.String() == "esc" || k.String() == "enter" || k.String() == "q" || k.String() == "?" {
			m.screen = scDash
		}
		return m, nil
	}
	return m, m.dashKey(k)
}

func (m *model) quit() tea.Cmd {
	if len(m.running) > 0 && !m.quitArm {
		m.quitArm = true
		m.fail("a backup is running - quit again to cancel it and exit")
		return nil
	}
	for _, c := range m.running {
		c()
	}
	return tea.Quit
}

func (m *model) dashKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	if k.String() != "q" {
		m.quitArm = false
	}
	st := m.stream()
	switch k.String() {
	case "q":
		return m.quit()
	case "up", "k":
		m.moveSel(-1)
		return m.autoCheck()
	case "down", "j":
		m.moveSel(1)
		return m.autoCheck()
	case "t":
		return m.openTargets()
	case "n":
		m.startWizard(nil)
	case "?":
		m.screen = scHelp
	case "p":
		return m.pingAll()
	}
	if st == nil {
		return nil
	}
	switch k.String() {
	case "a":
		m.startWizard(st)
	case "c":
		m.say("checking sync state…")
		return m.checkStream(*st)
	case "r":
		if up, _ := m.streamTargets(*st); up == 0 {
			m.fail("none of this stream's targets is connected - plug one in (t shows all targets)")
			return nil
		}
		return m.startPrepare(*st)
	case "s":
		return m.openPreview(*st)
	case "x":
		if c, ok := m.running[st.ID]; ok {
			c()
			m.say("cancelling…")
		}
	case "v":
		return m.startVerify(*st)
	case "e":
		m.openEdit(st)
	case "d":
		m.openDeleteMenu(st)
	}
	return nil
}

// ---- run flow --------------------------------------------------------

func (m *model) startPrepare(st config.Stream) tea.Cmd {
	if _, busy := m.running[st.ID]; busy {
		m.fail("this stream is already running")
		return nil
	}
	m.plan = &planView{stream: st, preparing: true, skipped: map[int]bool{}}
	m.screen = scPlan
	env := m.env
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		src := env.CheckSource(ctx, st)
		ps := make([]*runner.Prepared, len(st.Targets))
		var wg sync.WaitGroup
		for i, t := range st.Targets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ps[i] = env.Prepare(ctx, st, t, src)
			}()
		}
		wg.Wait()
		return prepMsg{st.ID, ps}
	}
}

func (m *model) onPrepared(p prepMsg) tea.Cmd {
	if m.plan == nil || m.plan.stream.ID != p.sid {
		return nil
	}
	m.plan.preparing, m.plan.ps = false, p.ps
	return nil
}

func (m *model) planKey(k tea.KeyMsg) tea.Cmd {
	pv := m.plan
	switch k.String() {
	case "esc", "q", "n":
		m.screen, m.plan = scDash, nil
		return nil
	case "y", "enter":
		if pv.preparing {
			return nil
		}
		for pi, p := range pv.ps {
			if !p.Runnable() {
				continue
			}
			for ni := range p.Needs {
				pv.queue = append(pv.queue, needRef{pi, ni})
			}
		}
		return m.nextConfirm()
	}
	return nil
}

func (m *model) nextConfirm() tea.Cmd {
	pv := m.plan
	for len(pv.queue) > 0 {
		r := pv.queue[0]
		pv.queue = pv.queue[1:]
		if pv.skipped[r.pi] {
			continue
		}
		p := pv.ps[r.pi]
		n := p.Needs[r.ni]
		title := fmt.Sprintf("Confirm for target %q", p.Target.Name)
		m.typ = newTyped(title, []string{n.Prompt, "", "Type  " + sBold.Render(n.Expect) + "  to allow, or press esc to skip this target."}, n.Expect,
			func(m *model) tea.Cmd { m.screen = scPlan; return m.nextConfirm() },
			func(m *model) tea.Cmd { pv.skipped[r.pi] = true; m.screen = scPlan; return m.nextConfirm() })
		m.screen = scTyped
		return textinputBlink()
	}
	return m.launch()
}

func (m *model) typedKey(k tea.KeyMsg) tea.Cmd {
	t := m.typ
	switch k.String() {
	case "esc":
		return t.cancel(m)
	case "enter":
		if t.in.Value() == t.expect {
			return t.ok(m)
		}
		t.wrong = true
		return nil
	}
	var c tea.Cmd
	t.in, c = t.in.Update(k)
	return c
}

func (m *model) launch() tea.Cmd {
	pv := m.plan
	var go_ []*runner.Prepared
	for i, p := range pv.ps {
		if p.Runnable() && !pv.skipped[i] {
			go_ = append(go_, p)
		}
	}
	m.screen = scDash
	m.plan = nil
	if len(go_) == 0 {
		m.fail("nothing to run - no target passed all safety checks")
		return nil
	}
	st := pv.stream
	lock, err := state.LockStream(st.ID)
	if err != nil {
		m.fail(err.Error())
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.running[st.ID] = cancel
	var wg sync.WaitGroup
	for _, p := range go_ {
		m.runs[p.Target.ID] = &runView{Phase: "syncing"}
		wg.Add(1)
		go func() {
			defer wg.Done()
			tid := p.Target.ID
			res := m.env.Execute(ctx, p, func(pr engine.Progress) {
				select {
				case m.ch <- progMsg{tid, pr}:
				default:
				}
			})
			m.ch <- doneMsg{tid, res}
		}()
	}
	go func() {
		wg.Wait()
		lock.Unlock()
		m.ch <- finishedMsg{st.ID}
	}()
	m.say(fmt.Sprintf("backup started to %d target(s)", len(go_)))
	return nil
}

func (m *model) startVerify(st config.Stream) tea.Cmd {
	if _, busy := m.running[st.ID]; busy {
		m.fail("stream is running")
		return nil
	}
	m.verifyPending, m.verifyOut = len(st.Targets), nil
	m.say("verifying all targets (read-only)…")
	var cmds []tea.Cmd
	for _, t := range st.Targets {
		t := t
		m.runs[t.ID] = &runView{Phase: "verifying"}
		cmds = append(cmds, func() tea.Msg {
			v, err := m.env.Verify(context.Background(), st, t)
			return verifyMsg{t.ID, t.Name, v, err}
		})
	}
	return tea.Batch(cmds...)
}

func (m *model) onVerify(v verifyMsg) tea.Cmd {
	delete(m.runs, v.tid)
	switch {
	case v.err != nil:
		m.verifyOut = append(m.verifyOut, sRed.Render("✗ "+v.name)+"  "+v.err.Error())
	case v.v.OK():
		m.verifyOut = append(m.verifyOut, sGreen.Render("✔ "+v.name)+fmt.Sprintf("  identical (%d files compared)", v.v.Same))
	default:
		m.verifyOut = append(m.verifyOut, sRed.Render("✗ "+v.name)+fmt.Sprintf("  %d differ, %d missing on target, %d extra on target, %d errors",
			v.v.Differ, v.v.MissingOnTarget, v.v.ExtraOnTarget, v.v.Errors))
		for _, s := range v.v.Samples[:min(6, len(v.v.Samples))] {
			m.verifyOut = append(m.verifyOut, sDim.Render("     "+s))
		}
	}
	m.verifyPending--
	if m.verifyPending == 0 {
		m.info = append([]string{sBold.Render("Verification result"), ""}, m.verifyOut...)
		m.screen = scInfo
		m.msg = ""
	}
	return nil
}

// ---- edit / delete ---------------------------------------------------

func (m *model) openEdit(st *config.Stream) {
	s := st.Settings
	m.edit = &form{fields: []*formField{
		newText("name", "Stream name", st.Name, "", false),
		newText("excludes", "Exclude files/dirs (comma separated; /Dir/** or /file anchors to source root)", strings.Join(s.Excludes, ", "), "/Cache/**, /big.iso, *.tmp", false),
		newText("bw", "Bandwidth limit (e.g. 10M, empty = none)", s.BandwidthLimit, "", false),
		newText("transfers", "Parallel transfers", fmt.Sprint(s.Transfers), "4", false),
		newText("retention", "Trash retention in days (0 = keep forever)", fmt.Sprint(s.TrashRetentionDays), "90", false),
		newText("maxdel", "Mass-delete tripwire % (0 = off)", fmt.Sprint(s.MaxDeletePercent), "20", false),
		newText("schedule", "Cron schedule (used by `tines schedule`)", s.Schedule, "0 3 * * *", false),
		newText("notify", "Command to run on failure", s.NotifyCommand, "osascript -e 'display notification ...'", false),
		newBool("checksum", "Compare by checksum", s.Checksum, "slower, catches changes that keep size + mtime"),
		newBool("verify", "Verify after every run", s.VerifyAfter, "re-reads both sides and compares checksums"),
		newBool("mirror", "Store a copy of the config in every target", s.MirrorConfig, "lets you restore your setup from any target"),
	}}
	m.edit.focus()
	m.screen = scEdit
}

func (m *model) editKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "esc" {
		m.screen = scDash
		return nil
	}
	submit, c := m.edit.update(k)
	if !submit {
		return c
	}
	st := m.stream()
	f := m.edit
	atoi := func(key string) (int, error) {
		var n int
		if v := f.value(key); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &n); err != nil || n < 0 {
				return 0, fmt.Errorf("%q must be a non-negative number", f.value(key))
			}
		}
		return n, nil
	}
	tr, e1 := atoi("transfers")
	ret, e2 := atoi("retention")
	md, e3 := atoi("maxdel")
	for _, e := range []error{e1, e2, e3} {
		if e != nil {
			m.fail(e.Error())
			return nil
		}
	}
	old := *st
	st.Name = f.value("name")
	var ex []string
	for _, p := range strings.Split(f.value("excludes"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			ex = append(ex, p)
		}
	}
	st.Settings = config.Settings{Excludes: ex, BandwidthLimit: f.value("bw"), Checksum: f.flag("checksum"), Transfers: tr,
		TrashRetentionDays: ret, MaxDeletePercent: md, VerifyAfter: f.flag("verify"), MirrorConfig: f.flag("mirror"),
		NotifyCommand: f.value("notify"), Schedule: f.value("schedule")}
	if err := m.env.Cfg.Validate(); err != nil {
		*st = old
		m.fail(err.Error())
		return nil
	}
	if m.save() {
		m.say("settings saved")
	}
	m.screen = scDash
	return nil
}

func (m *model) openDeleteMenu(st *config.Stream) {
	mv := &menuView{title: "Remove what?"}
	sid := st.ID
	if len(st.Targets) > 1 {
		for _, t := range st.Targets {
			t := t
			mv.pick.items = append(mv.pick.items, fmt.Sprintf("Remove target %q from this stream", t.Name))
			mv.pick.hints = append(mv.pick.hints, "Only removes it from this stream. The target stays registered; its files are untouched.")
			mv.acts = append(mv.acts, func(m *model) tea.Cmd {
				m.askType(fmt.Sprintf("Remove target %q", t.Name), []string{"This only removes it from this stream. The target stays registered and nothing on it is deleted."}, t.Name, func(m *model) tea.Cmd {
					s := m.env.Cfg.Find(sid)
					out := s.Targets[:0]
					for _, x := range s.Targets {
						if x.ID != t.ID {
							out = append(out, x)
						}
					}
					s.Targets = out
					if m.save() {
						m.say("target removed")
					}
					m.screen = scDash
					return nil
				})
				return textinputBlink()
			})
		}
	}
	mv.pick.items = append(mv.pick.items, fmt.Sprintf("Delete stream %q", st.Name), "Cancel")
	mv.pick.hints = append(mv.pick.hints, "Only forgets the stream in tines. No backup data is deleted anywhere.", "")
	mv.acts = append(mv.acts, func(m *model) tea.Cmd {
		m.askType("Delete stream "+st.Name, []string{"This only removes the stream from tines. Source and backups are never touched."}, st.Name, func(m *model) tea.Cmd {
			m.env.Cfg.Remove(sid)
			m.sel = 0
			if m.save() {
				m.say("stream deleted from config")
			}
			m.screen = scDash
			return nil
		})
		return textinputBlink()
	}, func(m *model) tea.Cmd { m.screen = scDash; return nil })
	m.menu = mv
	m.screen = scMenu
}

func (m *model) askType(title string, lines []string, expect string, ok func(m *model) tea.Cmd) {
	m.typ = newTyped(title, append(lines, "", "Type  "+sBold.Render(expect)+"  to confirm, or press esc to cancel."), expect, ok,
		func(m *model) tea.Cmd { m.screen = scDash; return nil })
	m.screen = scTyped
}

func (m *model) menuKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "esc" {
		m.screen = scDash
		return nil
	}
	if m.menu.pick.update(k) {
		return m.menu.acts[m.menu.pick.cur](m)
	}
	return nil
}

var _ = lipgloss.Width

// ---- which streams can sync right now --------------------------------------

// streamTargets counts the stream's targets that are connected. A target that
// has not been checked yet counts as connected, so nothing flickers into the
// "unavailable" list while the first pings are still running.
func (m *model) streamTargets(st config.Stream) (up, total int) {
	for _, t := range st.Targets {
		total++
		if c := m.conns["tgt:"+t.ID]; !c.Checked || c.Up() {
			up++
		}
	}
	return up, total
}

// streamOrder returns stream indexes: those with at least one connected
// target first (they can sync), then the unavailable ones.
func (m *model) streamOrder() (ready, unavailable []int) {
	for i, st := range m.env.Cfg.Streams {
		if up, _ := m.streamTargets(st); up > 0 {
			ready = append(ready, i)
		} else {
			unavailable = append(unavailable, i)
		}
	}
	return ready, unavailable
}

// moveSel moves the selection through the streams in display order.
func (m *model) moveSel(d int) {
	r, u := m.streamOrder()
	order := append(r, u...)
	for pos, idx := range order {
		if idx == m.sel {
			if n := pos + d; n >= 0 && n < len(order) {
				m.sel = order[n]
			}
			return
		}
	}
	if len(order) > 0 {
		m.sel = order[0]
	}
}
