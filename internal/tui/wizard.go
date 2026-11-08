package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chrishartmann/tines/internal/config"
	"github.com/chrishartmann/tines/internal/endpoint"
	"github.com/chrishartmann/tines/internal/engine"
	"github.com/chrishartmann/tines/internal/runner"
)

type wzStep int

const (
	wzName wzStep = iota
	wzKind
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
		w.existing, w.stream, w.role, w.step = existing.ID, *existing, "target", wzKind
		m.initKindPicker(w)
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
		case w.step == wzName || (w.existing != "" && w.step <= wzKind):
			m.screen = scDash
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
			if w.more.cur == 0 {
				w.role, w.step = "target", wzKind
				m.initKindPicker(w)
				w.pick.cur = 0
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
	if w.role == "target" {
		fs = append(fs, newText("tname", "Target name", fmt.Sprintf("backup %d", len(w.stream.Targets)+1), "e.g. External SSD", false))
	}
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
	if w.role == "target" {
		if vals["tname"] == "" {
			w.err = "give the target a name"
			return nil
		}
		for _, t := range w.stream.Targets {
			if strings.EqualFold(t.Name, vals["tname"]) {
				w.err = "a target with this name already exists in this stream"
				return nil
			}
		}
	}
	ep, err := endpoint.Build(context.Background(), w.kind, vals)
	if err != nil {
		w.err = err.Error()
		return nil
	}
	w.testEP, w.testErr, w.testNote = ep, "", ""
	// safety: overlap checks happen immediately
	if w.role == "target" {
		if w.stream.Source.Spec != "" && endpoint.Overlap(w.stream.Source.Spec, ep.Spec) {
			w.testErr = "this target overlaps with the source (same folder or nested)."
		}
		for _, t := range w.stream.Targets {
			if endpoint.Overlap(t.Spec, ep.Spec) {
				w.testErr = fmt.Sprintf("this target overlaps with target %q.", t.Name)
			}
		}
	}
	for _, o := range m.env.Cfg.Streams {
		if o.ID == w.stream.ID {
			continue
		}
		if w.role == "target" {
			if endpoint.Overlap(o.Source.Spec, ep.Spec) {
				w.testErr = fmt.Sprintf("this overlaps with the SOURCE of stream %q.", o.Name)
			}
			for _, t := range o.Targets {
				if endpoint.Overlap(t.Spec, ep.Spec) {
					w.testErr = fmt.Sprintf("this location is already used by target %q of stream %q.", t.Name, o.Name)
				}
			}
		} else {
			for _, t := range o.Targets {
				if endpoint.Overlap(t.Spec, ep.Spec) {
					w.testErr = fmt.Sprintf("this overlaps with target %q of stream %q.", t.Name, o.Name)
				}
			}
		}
	}
	w.step = wzTesting
	if w.testErr != "" {
		w.step = wzResult
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		d, err := runner.Ping(ctx, ep.Spec)
		c := conn{Checked: true, Latency: d}
		switch {
		case err == nil:
			c.OK = true
		case engine.Code(err) == engine.CodeDirNotFound:
			c.Missing = true
		default:
			c.Err = err.Error()
		}
		return wizTestMsg{ep, c, err}
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
	case t.conn.Missing && w.role == "source":
		w.testErr = "the source directory does not exist."
	case t.conn.Missing:
		w.testNote = "Connected, but the directory does not exist yet. It will be created on the first run - after you confirm."
	case w.role == "source":
		w.testErr = "cannot connect: " + t.conn.Err
	default:
		w.testNote = "Cannot connect right now (" + t.conn.Err + "). You can still add it - e.g. for a drive that is unplugged. tines will refuse to run until it is reachable."
	}
	return nil
}

func (m *model) acceptEndpoint() tea.Cmd {
	w := m.wiz
	ep := w.testEP
	if w.role == "source" {
		w.stream.Source = ep
		w.role, w.step = "target", wzKind
		m.initKindPicker(w)
		return nil
	}
	name := w.form.value("tname")
	w.stream.Targets = append(w.stream.Targets, config.Target{ID: config.NewID(), Name: name, Endpoint: ep})
	if w.existing != "" {
		cfg := m.env.Cfg
		s := cfg.Find(w.existing)
		s.Targets = w.stream.Targets
		if m.save() {
			m.say(fmt.Sprintf("target %q added", name))
		}
		m.screen = scDash
		return m.pingAll()
	}
	w.more = picker{items: []string{"Add another target", "That's all - continue"}}
	w.more.cur = 1
	w.step = wzMore
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
		if w.role == "source" {
			b.WriteString("Step 2 · Where is the data you want to back up?  (the SOURCE)\n\n")
		} else {
			b.WriteString(fmt.Sprintf("Where should the backup go?  (TARGET %d)\n\n", len(w.stream.Targets)+1))
		}
		b.WriteString(w.pick.view())
		keys = "↑↓ choose  enter select  esc back"
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
		case w.testOK:
			b.WriteString(sGreen.Render("● connected and readable"))
		default:
			b.WriteString(sYellow.Render("◐ " + w.testNote))
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
