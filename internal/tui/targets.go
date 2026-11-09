package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/endpoint"
	"github.com/chriha/tines/internal/engine"
	"github.com/chriha/tines/internal/runner"
)

// Up reports whether the endpoint answered (a missing directory on a
// reachable volume still counts as connected).
func (c conn) Up() bool { return c.OK || c.Missing }

func pingConn(spec string, timeout time.Duration) conn {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	d, err := runner.Ping(ctx, spec)
	c := conn{Checked: true, Latency: d}
	switch {
	case err == nil:
		c.OK = true
	case engine.Code(err) == engine.CodeDirNotFound:
		c.Missing, c.Err = true, "directory does not exist"
	default:
		c.Err = err.Error()
	}
	return c
}

// statusText is the connection column of the targets list.
func statusText(c conn) string {
	switch {
	case !c.Checked:
		return sDim.Render("checking…")
	case c.OK:
		return sGreen.Render("connected") + sDim.Render(fmt.Sprintf(" %dms", c.Latency.Milliseconds()))
	case c.Missing:
		return sYellow.Render("connected, folder missing")
	}
	return sRed.Render("not connected")
}

func pad(s string, n int) string {
	s = trunc(s, n)
	if w := ansi.StringWidth(s); w < n {
		s += strings.Repeat(" ", n-w)
	}
	return s
}

// usageBar draws used space; colour warns as the target fills up.
func usageBar(total, free int64, w int, live bool) string {
	if total <= 0 || w < 4 {
		return sDim.Render(strings.Repeat("░", max(w, 0)))
	}
	used := int(100 * (total - free) / total)
	f := min(used*w/100, w)
	col := lipgloss.TerminalColor(cGreen)
	switch left := 100 - used; {
	case !live:
		col = cDim
	case left < 10:
		col = cRed
	case left < 25:
		col = cYellow
	}
	return lipgloss.NewStyle().Foreground(col).Render(strings.Repeat("█", f)) + sDim.Render(strings.Repeat("░", w-f))
}

// ---- targets screen ------------------------------------------------------

type targetsView struct {
	sel     int
	confirm string // target id awaiting a second U to unmount
}

type unmountMsg struct {
	name, root string
	err        error
}

// unmountKey handles U: the first press asks, the second unmounts.
func (m *model) unmountKey(t config.Target) tea.Cmd {
	root, ok := endpoint.VolumeRoot(t.Spec)
	if !ok {
		if _, gone := endpoint.UnmountedVolume(t.Spec); gone {
			m.fail(fmt.Sprintf("%q is already unmounted", t.Name))
		} else {
			m.fail(fmt.Sprintf("%q is not on a removable volume, so there is nothing to unmount", t.Name))
		}
		return nil
	}
	for sid := range m.running {
		if st := m.env.Cfg.Find(sid); st != nil {
			for _, x := range st.Targets {
				if x.ID == t.ID {
					m.fail(fmt.Sprintf("stream %q is running - wait for it to finish or cancel it first", st.Name))
					return nil
				}
			}
		}
	}
	if m.checking[t.ID] || m.runs[t.ID] != nil && !m.runs[t.ID].Done {
		m.fail("this target is being checked or verified - try again in a moment")
		return nil
	}
	if m.tv.confirm != t.ID {
		m.tv.confirm = t.ID
		m.say(fmt.Sprintf("press U again to unmount %s (%s); any other key cancels", t.Name, root))
		return nil
	}
	m.tv.confirm = ""
	m.say("unmounting " + root + "…")
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		return unmountMsg{t.Name, root, endpoint.Unmount(ctx, root)}
	}
}

func (m *model) onUnmount(u unmountMsg) tea.Cmd {
	if u.err != nil {
		m.fail(fmt.Sprintf("could not unmount %s: %s", u.root, u.err))
		return nil
	}
	m.say(fmt.Sprintf("%s unmounted - safe to unplug", u.root))
	return m.pingAll()
}

func (m *model) openTargets() tea.Cmd {
	m.screen = scTargets
	if m.tv.sel >= len(m.env.Cfg.Targets) {
		m.tv.sel = max(len(m.env.Cfg.Targets)-1, 0)
	}
	return m.pingAll()
}

func (m *model) targetsKey(k tea.KeyMsg) tea.Cmd {
	m.msg = ""
	ts := m.env.Cfg.Targets
	armed := m.tv.confirm
	m.tv.confirm = ""
	switch k.String() {
	case "U":
		if len(ts) > 0 {
			m.tv.confirm = armed
			return m.unmountKey(ts[m.tv.sel])
		}
	case "esc", "q", "t":
		m.screen = scDash
	case "up", "k":
		if m.tv.sel > 0 {
			m.tv.sel--
		}
	case "down", "j":
		if m.tv.sel < len(ts)-1 {
			m.tv.sel++
		}
	case "a", "n":
		return m.startTargetWizard(nil, scTargets)
	case "r":
		for _, t := range ts {
			m.forceCap[t.ID] = true
		}
		m.say("refreshing connection and capacity…")
		return m.pingAll()
	case "e":
		if len(ts) > 0 {
			t := ts[m.tv.sel]
			return m.startTargetWizard(&t, scTargets)
		}
	case "d":
		if len(ts) == 0 {
			return nil
		}
		t := ts[m.tv.sel]
		if used := m.env.Cfg.StreamsUsing(t.ID); len(used) > 0 {
			m.fail(fmt.Sprintf("%q is used by stream(s) %s - remove it from them first", t.Name, strings.Join(used, ", ")))
			return nil
		}
		m.typ = newTyped("Remove target "+t.Name, []string{"This only forgets the target in tines. Nothing on it is deleted.", "",
			"Type  " + sBold.Render(t.Name) + "  to confirm, or press esc to cancel."}, t.Name,
			func(m *model) tea.Cmd {
				if err := m.env.Cfg.RemoveTarget(t.ID); err != nil {
					m.fail(err.Error())
				} else if m.save() {
					m.say("target removed")
				}
				m.screen = scTargets
				m.tv.sel = max(min(m.tv.sel, len(m.env.Cfg.Targets)-1), 0)
				return nil
			},
			func(m *model) tea.Cmd { m.screen = scTargets; return nil })
		m.screen = scTyped
		return textinputBlink()
	}
	return nil
}

func (m *model) viewTargets() (string, string) {
	ts := m.env.Cfg.Targets
	if len(ts) == 0 {
		return panel("Targets", "No targets yet.\n\nA target is a drive, stick, card, NAS or cloud location your backups go to.\nPress "+sBold.Render("a")+" to add one. It has to be connected so tines can read its capacity."),
			"a add target  esc back"
	}
	const wName, wType, wCap, wFree, wBar = 20, 10, 11, 11, 12
	var b strings.Builder
	b.WriteString(sDim.Render("   " + pad("", 2) + pad("NAME", wName) + pad("TYPE", wType) + pad("CAPACITY", wCap) + pad("LEFT", wFree) + pad("USED", wBar+2) + "STATUS"))
	b.WriteString("\n")
	for i, t := range ts {
		c := m.conns["tgt:"+t.ID]
		dev := m.env.State.Device(t.ID)
		live := c.Up()
		capS, freeS, bar := sDim.Render(pad("—", wCap)), sDim.Render(pad("—", wFree)), usageBar(0, 0, wBar, false)
		if dev.KnownCapacity() {
			capStyle, freeStyle := sBold, sBold
			if !live {
				capStyle, freeStyle = sDim, sDim
			}
			capS, freeS = capStyle.Render(pad(humanBytes(dev.Total), wCap)), freeStyle.Render(pad(humanBytes(dev.Free), wFree))
			bar = usageBar(dev.Total, dev.Free, wBar, live)
		}
		name := pad(t.Name, wName)
		prefix := "  "
		if i == m.tv.sel {
			prefix = sAccent.Render("› ")
			name = sBold.Render(name)
		} else if !live && c.Checked {
			name = sDim.Render(name)
		}
		fmt.Fprintf(&b, "%s%s %s%s%s%s  %s  %s\n", prefix, dot(c, false), name, pad(t.DisplayType(), wType), capS, freeS, bar, statusText(c))
	}
	// details of the selected target
	t := ts[m.tv.sel]
	c := m.conns["tgt:"+t.ID]
	dev := m.env.State.Device(t.ID)
	b.WriteString("\n" + sDim.Render(strings.Repeat("─", 60)) + "\n")
	row := func(k, v string) { b.WriteString(sDim.Render(pad(k, 11)) + v + "\n") }
	row("location", endpoint.Display(t.Endpoint))
	if used := m.env.Cfg.StreamsUsing(t.ID); len(used) > 0 {
		row("streams", strings.Join(used, ", "))
	} else {
		row("streams", sDim.Render("not used by any stream"))
	}
	switch {
	case dev.KnownCapacity() && c.Up():
		row("capacity", fmt.Sprintf("%s total · %s left · read %s", humanBytes(dev.Total), humanBytes(dev.Free), ago(dev.CapacityAt)))
	case dev.KnownCapacity():
		row("capacity", sYellow.Render(fmt.Sprintf("last known %s left of %s, read %s (target not connected)", humanBytes(dev.Free), humanBytes(dev.Total), ago(dev.CapacityAt))))
	case c.Up():
		row("capacity", sDim.Render("this connection does not report capacity"))
	default:
		row("capacity", sDim.Render("unknown - connect the target to read it"))
	}
	if !c.Up() && c.Checked && c.Err != "" {
		row("problem", sRed.Render(trunc(c.Err, max(m.w-20, 20))))
	}
	if !dev.LastSeen.IsZero() && !c.Up() {
		row("last seen", ago(dev.LastSeen))
	}
	return panel("Targets", strings.TrimRight(b.String(), "\n")), "↑↓ select  a add  e edit  d remove  U unmount  r refresh  esc back"
}

// ---- add / edit target wizard -------------------------------------------

type tgStep int

const (
	tgName tgStep = iota
	tgKind
	tgFields
	tgTesting
	tgResult
	tgType
)

type tgWizard struct {
	step    tgStep
	editing string // target id when editing name/type only
	ret     screen
	name    *form
	kinds   picker
	kind    string
	fields  *form
	types   picker
	ep      config.Endpoint
	conn    conn
	cap     engine.Capacity
	capOK   bool
	blocker string // why this endpoint cannot be added
	err     string
	added   string // id of the target that was added, for the caller
}

type tgTestMsg struct {
	ep    config.Endpoint
	conn  conn
	cap   engine.Capacity
	capOK bool
}

func (m *model) startTargetWizard(edit *config.Target, ret screen) tea.Cmd {
	w := &tgWizard{ret: ret}
	w.name = &form{fields: []*formField{newText("name", "Name of this target", "", "e.g. Backup SSD", false)}}
	for _, k := range endpoint.Kinds {
		w.kinds.items = append(w.kinds.items, k.Label)
		w.kinds.hints = append(w.kinds.hints, k.Hint)
	}
	w.types.items = append(w.types.items, config.TargetTypes...)
	if edit != nil {
		w.editing = edit.ID
		w.name.fields[0].in.SetValue(edit.Name)
		w.types.cur = indexOf(config.TargetTypes, edit.DisplayType())
	}
	w.name.focus()
	m.tw = w
	m.screen = scTgWizard
	return textinputBlink()
}

func indexOf(s []string, v string) int {
	for i := range s {
		if s[i] == v {
			return i
		}
	}
	return len(s) - 1
}

func (m *model) tgForward(msg tea.Msg) tea.Cmd {
	w := m.tw
	switch w.step {
	case tgName:
		_, c := w.name.update(msg)
		return c
	case tgFields:
		_, c := w.fields.update(msg)
		return c
	}
	return nil
}

func (m *model) tgKey(k tea.KeyMsg) tea.Cmd {
	w := m.tw
	w.err = ""
	if k.String() == "esc" {
		switch {
		case w.step == tgName || (w.editing != "" && w.step == tgType):
			if w.editing != "" && w.step == tgType {
				w.step = tgName
				return textinputBlink()
			}
			m.screen = w.ret
		case w.step == tgKind:
			w.step = tgName
		case w.step == tgFields:
			w.step = tgKind
		case w.step == tgTesting, w.step == tgResult:
			w.step = tgFields
		case w.step == tgType:
			w.step = tgResult
		}
		return textinputBlink()
	}
	switch w.step {
	case tgName:
		sub, c := w.name.update(k)
		if !sub {
			return c
		}
		n := w.name.value("name")
		switch {
		case n == "":
			w.err = "give the target a name"
		case m.env.Cfg.TargetNameTaken(n, w.editing):
			w.err = "a target with this name already exists"
		case w.editing != "":
			w.step = tgType
		default:
			w.step = tgKind
		}
	case tgKind:
		if w.kinds.update(k) {
			w.kind = endpoint.Kinds[w.kinds.cur].ID
			w.fields = tgFieldsForm(w.kind)
			w.step = tgFields
			return textinputBlink()
		}
	case tgFields:
		sub, c := w.fields.update(k)
		if !sub {
			return c
		}
		return m.tgTest()
	case tgResult:
		if k.String() == "enter" || k.String() == "c" {
			if w.blocker != "" {
				w.err = "this target cannot be added - press esc to go back and edit"
				return nil
			}
			w.types.cur = indexOf(config.TargetTypes, config.GuessTypeForKind(w.kind))
			w.step = tgType
		}
	case tgType:
		if w.types.update(k) {
			return m.tgFinish()
		}
	}
	return nil
}

func tgFieldsForm(kind string) *form {
	var fs []*formField
	for _, f := range endpoint.KindByID(kind).Fields {
		fs = append(fs, newText(f.Key, f.Label, f.Default, f.Placeholder, f.Secret))
	}
	f := &form{fields: fs}
	f.focus()
	return f
}

// targetConflict explains why ep may not become a target, or returns "".
func (m *model) targetConflict(ep config.Endpoint, selfID string) string {
	for _, o := range m.env.Cfg.Targets {
		if o.ID != selfID && endpoint.Overlap(o.Spec, ep.Spec) {
			return fmt.Sprintf("this location overlaps with the existing target %q.", o.Name)
		}
	}
	for _, s := range m.env.Cfg.Streams {
		if endpoint.Overlap(s.Source.Spec, ep.Spec) {
			return fmt.Sprintf("this location overlaps with the SOURCE of stream %q.", s.Name)
		}
	}
	return ""
}

func (m *model) tgTest() tea.Cmd {
	w := m.tw
	vals := map[string]string{}
	for _, fl := range w.fields.fields {
		vals[fl.key] = strings.TrimSpace(fl.in.Value())
	}
	ep, err := endpoint.Build(context.Background(), w.kind, vals)
	if err != nil {
		w.err = err.Error()
		return nil
	}
	w.ep, w.blocker, w.capOK, w.cap = ep, m.targetConflict(ep, ""), false, engine.Capacity{}
	if w.blocker != "" {
		w.step = tgResult
		return nil
	}
	w.step = tgTesting
	return func() tea.Msg {
		c := pingConn(ep.Spec, 30*time.Second)
		msg := tgTestMsg{ep: ep, conn: c}
		if c.Up() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			msg.cap, msg.capOK = engine.About(ctx, ep.Spec)
			msg.capOK = msg.capOK && msg.cap.Total > 0
		}
		return msg
	}
}

func (m *model) onTgTest(t tgTestMsg) {
	w := m.tw
	if w == nil || w.step != tgTesting || t.ep.Spec != w.ep.Spec {
		return
	}
	w.step, w.conn, w.cap, w.capOK = tgResult, t.conn, t.cap, t.capOK
	if !t.conn.Up() {
		w.blocker = "not connected (" + t.conn.Err + "). Only connected targets can be added - plug it in or fix the connection, then try again."
	}
}

func (m *model) tgFinish() tea.Cmd {
	w := m.tw
	cfg := m.env.Cfg
	name := w.name.value("name")
	typ := config.TargetTypes[w.types.cur]
	if w.editing != "" {
		t := cfg.FindTarget(w.editing)
		if t == nil {
			m.screen = w.ret
			return nil
		}
		t.Name, t.Type = name, typ
		cfg.Resolve()
		if m.save() {
			m.say("target updated")
		}
		m.screen = w.ret
		return nil
	}
	t := config.Target{ID: config.NewID(), Name: name, Type: typ, Endpoint: w.ep}
	cfg.Targets = append(cfg.Targets, t)
	if err := cfg.Validate(); err != nil {
		cfg.Targets = cfg.Targets[:len(cfg.Targets)-1]
		w.err = err.Error()
		return nil
	}
	if !m.save() {
		cfg.Targets = cfg.Targets[:len(cfg.Targets)-1]
		return nil
	}
	if w.capOK {
		m.env.State.SetCapacity(t.ID, w.cap.Total, w.cap.Free)
	}
	w.added = t.ID
	m.conns["tgt:"+t.ID] = w.conn
	m.tv.sel = len(cfg.Targets) - 1
	m.say(fmt.Sprintf("target %q added", name))
	m.screen = w.ret
	if w.ret == scWizard && m.wiz != nil {
		m.initTargetPicker(m.wiz) // the new target is now pickable
		for i, id := range m.wiz.tids {
			if id == t.ID {
				m.wiz.tpick.cur = i
			}
		}
	}
	return m.pingAll()
}

func (m *model) viewTgWizard() (string, string) {
	w := m.tw
	title := "Add a target"
	if w.editing != "" {
		title = "Edit target"
	}
	var b strings.Builder
	keys := "enter continue  esc back"
	switch w.step {
	case tgName:
		b.WriteString("A target is a place backups are written to: a drive, stick, card, NAS or cloud bucket.\nIt has to be connected now, so tines can read its capacity.\n\n" + w.name.view())
	case tgKind:
		b.WriteString("How is it connected?\n\n" + w.kinds.view())
		keys = "↑↓ choose  enter select  esc back"
	case tgFields:
		b.WriteString(sDim.Render(endpoint.KindByID(w.kind).Hint) + "\n\n" + w.fields.view())
		keys = "tab/↓ next  enter next/test connection  esc back"
	case tgTesting:
		b.WriteString(m.spin() + " connecting to " + endpoint.Display(w.ep) + " and reading its capacity …")
		keys = "esc cancel"
	case tgResult:
		b.WriteString(sBold.Render(endpoint.Display(w.ep)) + "\n\n")
		if w.blocker != "" {
			b.WriteString(sRed.Render("✗ " + w.blocker))
			keys = "esc go back and edit"
			break
		}
		if w.conn.Missing {
			b.WriteString(sGreen.Render("● connected") + sYellow.Render("  (the folder does not exist yet; it is created on the first backup)") + "\n")
		} else {
			b.WriteString(sGreen.Render("● connected") + sDim.Render(fmt.Sprintf("  %dms", w.conn.Latency.Milliseconds())) + "\n")
		}
		if w.capOK {
			b.WriteString(fmt.Sprintf("  capacity %s, %s left\n", sBold.Render(humanBytes(w.cap.Total)), sBold.Render(humanBytes(w.cap.Free))))
		} else {
			b.WriteString(sYellow.Render("  This connection does not report its capacity, so none will be shown.") + "\n")
		}
		keys = "enter continue  esc go back and edit"
	case tgType:
		b.WriteString("What kind of device is it?\n\n" + w.types.view())
		keys = "↑↓ choose  enter save  esc back"
	}
	if w.err != "" {
		b.WriteString("\n\n" + sRed.Render("✗ "+w.err))
	}
	return panel(title, b.String()), keys
}
