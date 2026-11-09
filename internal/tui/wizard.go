package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/endpoint"
)

type wzStep int

const (
	wzName wzStep = iota
	wzKind
	wzPickTarget
	wzFields
	wzTesting
	wzResult
	wzMore
	wzSettings
	wzReview
)

type wizard struct {
	step     wzStep
	role     string // source | target
	existing string // stream id when adding a target
	name     *form
	pick     picker
	form     *form
	kind     string
	stream   config.Stream
	settings *form
	testEP   config.Endpoint
	testErr  string // blocking error
	testNote string // non-blocking warning
	testOK   bool
	more     picker
	tpick    picker   // registered targets that can still be added
	tids     []string // target id per tpick item
	err      string
}

type wizTestMsg struct {
	ep   config.Endpoint
	conn conn
	err  error
}

func (m *model) startWizard(existing *config.Stream) {
	w := &wizard{stream: config.Stream{ID: config.NewID(), Settings: m.env.Cfg.Defaults}}
	if existing != nil {
		w.existing, w.stream, w.role, w.step = existing.ID, *existing, "target", wzPickTarget
		m.initTargetPicker(w)
	} else {
		w.name = &form{fields: []*formField{newText("name", "Name of this backup stream", "", "e.g. Photos", false)}}
		w.name.focus()
	}
	m.wiz = w
	m.screen = scWizard
}

func (m *model) initKindPicker(w *wizard) {
	w.pick = picker{}
	for _, k := range endpoint.Kinds {
		w.pick.items = append(w.pick.items, k.Label)
		w.pick.hints = append(w.pick.hints, k.Hint)
	}
}

// initTargetPicker lists the registered targets this stream does not use yet.
func (m *model) initTargetPicker(w *wizard) {
	w.tpick, w.tids = picker{}, nil
	used := map[string]bool{}
	for _, t := range w.stream.Targets {
		used[t.ID] = true
	}
	for _, t := range m.env.Cfg.Targets {
		if used[t.ID] {
			continue
		}
		c := m.conns["tgt:"+t.ID]
		dev := m.env.State.Device(t.ID)
		state := "checking…"
		switch {
		case !c.Checked:
		case c.Up():
			state = "connected"
		default:
			state = "not connected"
		}
		line := fmt.Sprintf("%s  ·  %s", t.Name, t.DisplayType())
		if dev.KnownCapacity() {
			line += fmt.Sprintf("  ·  %s left of %s", humanBytes(dev.Free), humanBytes(dev.Total))
		}
		w.tpick.items = append(w.tpick.items, line)
		w.tpick.hints = append(w.tpick.hints, state+"  "+endpoint.Display(t.Endpoint))
		w.tids = append(w.tids, t.ID)
	}
}

// enterMore shows the "add another target / continue" choice.
func (m *model) enterMore(w *wizard) {
	m.initTargetPicker(w)
	w.more = picker{}
	if len(w.tids) > 0 {
		w.more.items = append(w.more.items, "Add another target")
	}
	w.more.items = append(w.more.items, "That's all - continue")
	w.more.cur = len(w.more.items) - 1
	w.step = wzMore
}

func (m *model) wizardForward(msg tea.Msg) tea.Cmd {
	w := m.wiz
	switch w.step {
	case wzName:
		_, c := w.name.update(msg)
		return c
	case wzFields:
		_, c := w.form.update(msg)
		return c
	case wzSettings:
		_, c := w.settings.update(msg)
		return c
	}
	return nil
}

func (m *model) wizardKey(k tea.KeyMsg) tea.Cmd {
	w := m.wiz
	w.err = ""
	if k.String() == "esc" {
		switch {
		case w.step == wzName || (w.existing != "" && w.step <= wzPickTarget):
			m.screen = scDash
		case w.step == wzPickTarget && len(w.stream.Targets) > 0:
			m.enterMore(w)
		case w.step == wzFields:
			w.step = wzKind
		case w.step == wzResult || w.step == wzTesting:
			w.step = wzFields
		case w.step == wzKind && w.role == "source":
			w.step = wzName
		case w.step == wzMore:
			w.step = wzResult
		case w.step == wzSettings:
			w.step = wzMore
		case w.step == wzReview:
			w.step = wzSettings
		default:
			m.screen = scDash
		}
		return textinputBlink()
	}
	switch w.step {
	case wzName:
		if sub, c := w.name.update(k); sub {
			n := w.name.value("name")
			if n == "" {
				w.err = "give the stream a name"
				return nil
			}
			if m.env.Cfg.Find(n) != nil {
				w.err = "a stream with this name already exists"
				return nil
			}
			w.stream.Name, w.role, w.step = n, "source", wzKind
			m.initKindPicker(w)
			return nil
		} else {
			return c
		}
	case wzKind:
		if w.pick.update(k) {
			w.kind = endpoint.Kinds[w.pick.cur].ID
			w.form = m.endpointForm(w)
			w.step = wzFields
			return textinputBlink()
		}
	case wzPickTarget:
		if k.String() == "n" {
			return m.startTargetWizard(nil, scWizard)
		}
		if len(w.tids) == 0 {
			if k.String() == "enter" && len(w.stream.Targets) > 0 {
				m.enterMore(w)
			}
			return nil
		}
		if w.tpick.update(k) {
			return m.pickTarget(w, w.tids[w.tpick.cur])
		}
	case wzFields:
		sub, c := w.form.update(k)
		if sub {
			return m.runTest()
		}
		return c
	case wzResult:
		switch k.String() {
		case "enter", "c":
			if w.testErr != "" {
				w.err = "fix this first - press esc to go back and edit"
				return nil
			}
			return m.acceptEndpoint()
		}
	case wzMore:
		if w.more.update(k) {
			if len(w.tids) > 0 && w.more.cur == 0 {
				w.role, w.step = "target", wzPickTarget
				m.initTargetPicker(w)
			} else {
				w.step = wzSettings
				m.initSettings(w)
			}
		}
	case wzSettings:
		if sub, c := w.settings.update(k); sub {
			m.applySettings(w)
			w.step = wzReview
		} else {
			return c
		}
	case wzReview:
		if k.String() == "enter" || k.String() == "y" {
			return m.finishWizard()
		}
	}
	return nil
}

func (m *model) endpointForm(w *wizard) *form {
	kd := endpoint.KindByID(w.kind)
	var fs []*formField
	for _, f := range kd.Fields {
		fs = append(fs, newText(f.Key, f.Label, f.Default, f.Placeholder, f.Secret))
	}
	f := &form{fields: fs}
	f.focus()
	return f
}

func (m *model) runTest() tea.Cmd {
	w := m.wiz
	vals := map[string]string{}
	for _, fl := range w.form.fields {
		vals[fl.key] = strings.TrimSpace(fl.in.Value())
	}
	ep, err := endpoint.Build(context.Background(), w.kind, vals)
	if err != nil {
		w.err = err.Error()
		return nil
	}
	w.testEP, w.testErr, w.testNote = ep, "", ""
	// safety: the source may not overlap any registered target
	for _, t := range m.env.Cfg.Targets {
		if endpoint.Overlap(t.Spec, ep.Spec) {
			w.testErr = fmt.Sprintf("this overlaps with target %q.", t.Name)
		}
	}
	w.step = wzTesting
	if w.testErr != "" {
		w.step = wzResult
		return nil
	}
	return func() tea.Msg {
		return wizTestMsg{ep: ep, conn: pingConn(ep.Spec, 30*time.Second)}
	}
}

func (m *model) onWizTest(t wizTestMsg) tea.Cmd {
	w := m.wiz
	if w == nil || w.step != wzTesting || t.ep.Spec != w.testEP.Spec {
		return nil
	}
	w.step = wzResult
	w.testOK = t.conn.OK
	switch {
	case t.conn.OK:
	case t.conn.Missing:
		w.testErr = "the source directory does not exist."
	default:
		w.testErr = "cannot connect: " + t.conn.Err
	}
	return nil
}

// acceptEndpoint stores the tested source and moves on to choosing targets.
func (m *model) acceptEndpoint() tea.Cmd {
	w := m.wiz
	w.stream.Source = w.testEP
	w.role, w.step = "target", wzPickTarget
	m.initTargetPicker(w)
	return nil
}

// pickTarget adds a registered target to the stream being built or extended.
func (m *model) pickTarget(w *wizard, id string) tea.Cmd {
	t := m.env.Cfg.FindTarget(id)
	if t == nil {
		return nil
	}
	if w.existing != "" {
		if endpoint.Overlap(w.stream.Source.Spec, t.Spec) {
			w.err = "this target overlaps with the source of the stream."
			return nil
		}
		s := m.env.Cfg.Find(w.existing)
		s.Targets = append(s.Targets, *t)
		if m.save() {
			m.say(fmt.Sprintf("target %q added to %s", t.Name, s.Name))
		}
		m.screen = scDash
		return m.pingAll()
	}
	if endpoint.Overlap(w.stream.Source.Spec, t.Spec) {
		w.err = "this target overlaps with the source (same folder or nested)."
		return nil
	}
	w.stream.Targets = append(w.stream.Targets, *t)
	m.enterMore(w)
	return nil
}

func (m *model) initSettings(w *wizard) {
	s := w.stream.Settings
	w.settings = &form{fields: []*formField{
		newText("retention", "Keep trash (deleted/overwritten files) for how many days? 0 = forever", fmt.Sprint(s.TrashRetentionDays), "90", false),
		newText("maxdel", "Mass-delete tripwire: ask for typed confirmation above this % of a target", fmt.Sprint(s.MaxDeletePercent), "20", false),
		newBool("verify", "Verify checksums after every run", s.VerifyAfter, "slower but proves every target matches"),
		newBool("mirror", "Store a copy of the config in every target", s.MirrorConfig, "recoverable via `tines import --from <target>`"),
	}}
	w.settings.focus()
}

func (m *model) applySettings(w *wizard) {
	var r, d int
	fmt.Sscanf(w.settings.value("retention"), "%d", &r)
	fmt.Sscanf(w.settings.value("maxdel"), "%d", &d)
	s := &w.stream.Settings
	s.TrashRetentionDays, s.MaxDeletePercent = r, d
	s.VerifyAfter, s.MirrorConfig = w.settings.flag("verify"), w.settings.flag("mirror")
}

func (m *model) finishWizard() tea.Cmd {
	w := m.wiz
	m.env.Cfg.Streams = append(m.env.Cfg.Streams, w.stream)
	if err := m.env.Cfg.Validate(); err != nil {
		m.env.Cfg.Streams = m.env.Cfg.Streams[:len(m.env.Cfg.Streams)-1]
		w.err = err.Error()
		return nil
	}
	if !m.save() {
		m.env.Cfg.Streams = m.env.Cfg.Streams[:len(m.env.Cfg.Streams)-1]
		return nil
	}
	m.sel = len(m.env.Cfg.Streams) - 1
	m.say(fmt.Sprintf("stream %q created - press r to run your first backup", w.stream.Name))
	m.screen = scDash
	return tea.Batch(m.pingAll(), m.autoCheck())
}

func (m *model) viewWizard() (string, string) {
	w := m.wiz
	title := "New backup stream"
	if w.existing != "" {
		title = "Add a target to " + w.stream.Name
	}
	var b strings.Builder
	keys := "enter continue  esc back"
	switch w.step {
	case wzName:
		b.WriteString("A stream backs up ONE source to ONE OR MORE targets (redundant copies).\n\n" + w.name.view())
	case wzKind:
		b.WriteString("Step 2 · Where is the data you want to back up?  (the SOURCE)\n\n")
		b.WriteString(w.pick.view())
		keys = "↑↓ choose  enter select  esc back"
	case wzPickTarget:
		if len(w.tids) == 0 {
			b.WriteString(sYellow.Render("No registered target left to add.") + "\nPress " + sBold.Render("n") + " to register a new one (it must be connected).")
			keys = "n new target  esc back"
			if len(w.stream.Targets) > 0 {
				keys = "n new target  enter continue  esc back"
			}
			break
		}
		b.WriteString(fmt.Sprintf("Where should the backup go?  (TARGET %d)\n\n", len(w.stream.Targets)+1))
		b.WriteString(w.tpick.view())
		keys = "↑↓ choose  enter select  n new target  esc back"
	case wzFields:
		b.WriteString(sDim.Render(endpoint.KindByID(w.kind).Hint) + "\n\n" + w.form.view())
		keys = "tab/↓ next  enter next/test connection  esc back"
	case wzTesting:
		b.WriteString(m.spin() + " testing connection to " + endpoint.Display(w.testEP) + " …")
		keys = "esc cancel"
	case wzResult:
		b.WriteString(sBold.Render(endpoint.Display(w.testEP)) + "\n\n")
		switch {
		case w.testErr != "":
			b.WriteString(sRed.Render("✗ " + w.testErr))
			keys = "esc go back and edit"
		default:
			b.WriteString(sGreen.Render("● connected and readable"))
		}
		if w.testErr == "" {
			keys = "enter use this  esc go back and edit"
		}
	case wzMore:
		b.WriteString(fmt.Sprintf("%d target(s) added so far:\n", len(w.stream.Targets)))
		for _, t := range w.stream.Targets {
			b.WriteString("  → " + t.Name + sDim.Render("  "+endpoint.Display(t.Endpoint)) + "\n")
		}
		b.WriteString("\nMore redundancy?\n\n" + w.more.view())
		keys = "↑↓ choose  enter select"
	case wzSettings:
		b.WriteString("Safety settings for this stream (defaults are sensible - press enter through them):\n\n" + w.settings.view())
		keys = "tab/↓ next  space toggle  enter next/continue  esc back"
	case wzReview:
		s := w.stream.Settings
		b.WriteString(sBold.Render(w.stream.Name) + "\n\n")
		b.WriteString(sCyan.Render("SOURCE ") + endpoint.Display(w.stream.Source) + "\n")
		for _, t := range w.stream.Targets {
			b.WriteString(sGreen.Render("   └─▶ ") + t.Name + sDim.Render("  "+endpoint.Display(t.Endpoint)) + "\n")
		}
		b.WriteString("\n" + sBold.Render("Safety guarantees") + "\n")
		b.WriteString("  ✔ every run shows a dry-run plan first and waits for your confirmation\n")
		b.WriteString(fmt.Sprintf("  ✔ overwritten/deleted files go to trash (kept %s), never destroyed\n", daysText(s.TrashRetentionDays)))
		b.WriteString(fmt.Sprintf("  ✔ tripwire at %d%% deletions; targets are claimed with a marker file\n", s.MaxDeletePercent))
		b.WriteString("\nNothing is copied until you press " + sBold.Render("r") + " on the dashboard.")
		keys = "enter create stream  esc back"
	}
	if w.err != "" {
		b.WriteString("\n\n" + sRed.Render("✗ "+w.err))
	}
	return panel(title, b.String()), keys
}
