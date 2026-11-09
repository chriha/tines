package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/endpoint"
)

func textinputBlink() tea.Cmd { return textinput.Blink }

func (m *model) View() string {
	if m.w == 0 {
		return "loading…"
	}
	var body string
	keys := ""
	switch m.screen {
	case scDash:
		body, keys = m.viewDash(), "↑↓ stream  r run  s preview  c check  v verify  f duplicates  t targets  n new  a add target  e edit  d delete  ? help  q quit"
	case scWizard:
		body, keys = m.viewWizard()
	case scPlan:
		body, keys = m.viewPlan()
	case scTyped:
		body, keys = m.viewTyped(), "enter confirm  esc cancel"
	case scEdit:
		body, keys = panel("Stream settings", m.edit.view()), "↑↓/tab move  space toggle  enter next/save  ctrl+s save  esc cancel"
	case scTargets:
		body, keys = m.viewTargets()
	case scTgWizard:
		body, keys = m.viewTgWizard()
	case scPreview:
		body, keys = m.viewPreview()
	case scDupes:
		body, keys = m.viewDupes()
	case scDupPath:
		body, keys = panel("Find duplicate files", m.dpath.view()), "enter scan  esc cancel"
	case scMenu:
		body, keys = panel(m.menu.title, m.menu.pick.view()), "↑↓ choose  enter select  esc cancel"
	case scInfo:
		body, keys = panel("Info", strings.Join(m.info, "\n")), "enter/esc close"
	case scHelp:
		body, keys = panel("How tines keeps your data safe", helpText), "esc close"
	}
	head := sAccent.Render(" ⑂ tines") + sDim.Render("  safe 1→N backups")
	foot := sDim.Render(" " + keys)
	status := ""
	if m.msg != "" {
		if m.msgErr {
			status = sRed.Render(" ✗ " + m.msg)
		} else {
			status = sGreen.Render(" ✔ " + m.msg)
		}
	}
	used := lipgloss.Height(head) + lipgloss.Height(body) + 3
	pad := ""
	if m.h > used {
		pad = strings.Repeat("\n", m.h-used)
	}
	return head + "\n\n" + body + pad + "\n" + status + "\n" + trunc(foot, m.w)
}

func panel(title, content string) string {
	st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cAccent).Padding(1, 2).MarginLeft(1)
	return st.Render(sBold.Render(title) + "\n\n" + content)
}

const helpText = `• Every run starts with a dry-run plan. Nothing is written until you confirm.
• Files that are overwritten or deleted on a target are MOVED to
  <target>/.tines-trash/<timestamp>/ - never destroyed. Old trash is pruned
  only after your retention period.
• Each target holds a .tines-target.json marker. tines refuses to write to
  a folder that has no marker (unless you type "init" / "adopt") or that
  belongs to a different stream.
• Unplugged drives are detected, so a missing /Volumes/X is never silently
  recreated on your boot disk.
• Mass-delete tripwire: a plan that would trash more than N% of a target needs
  a typed confirmation. An empty source needs one too.
• Source and target may never overlap. One stream runs only once at a time.
• Your whole setup lives in one YAML file and is mirrored into each target
  (.tines-meta/config.yaml) so it can be restored: tines import --from <target>.`

func (m *model) viewTyped() string {
	t := m.typ
	body := strings.Join(t.lines, "\n") + "\n\n" + t.in.View()
	if t.wrong {
		body += "\n" + sRed.Render("That does not match.")
	}
	return panel(t.title, body)
}

// ---- dashboard -------------------------------------------------------

func (m *model) streamHealth(st config.Stream) (string, lipgloss.TerminalColor) {
	if _, ok := m.running[st.ID]; ok {
		return m.spin(), cCyan
	}
	down, behind, unknown := false, false, false
	if c := m.conns["src:"+st.ID]; c.Checked && !c.OK {
		down = true
	}
	for _, t := range st.Targets {
		c := m.conns["tgt:"+t.ID]
		ts, _ := m.env.State.Get(st.ID, t.ID)
		switch {
		case c.Checked && !c.OK && !c.Missing:
			down = true
		case ts.Behind > 0:
			behind = true
		case ts.Behind < 0 || ts.CheckedAt.IsZero():
			unknown = true
		}
	}
	switch {
	case down:
		return "○", cRed
	case behind:
		return "◐", cYellow
	case unknown:
		return "◌", cDim
	}
	return "●", cGreen
}

func (m *model) viewDash() string {
	if len(m.env.Cfg.Streams) == 0 {
		return panel("Welcome", "No backup streams yet.\n\nPress "+sBold.Render("t")+" to register the drives and servers you back up to,\nthen "+sBold.Render("n")+" to create your first stream with the wizard.\nA stream backs up one source to as many targets as you like.")
	}
	sidebar := m.w >= 100
	sw := 0
	var side string
	if sidebar {
		sw = 28
		ready, unavail := m.streamOrder()
		line := func(i int, dim bool) string {
			st := m.env.Cfg.Streams[i]
			g, c := m.streamHealth(st)
			text := trunc(st.Name, sw-6)
			if dim {
				g, c = "○", cDim
				text = sDim.Render(text)
			}
			l := lipgloss.NewStyle().Foreground(c).Render(g) + " " + text
			if i == m.sel {
				return sAccent.Render("› ") + sBold.Render(l)
			}
			return "  " + l
		}
		l := []string{sDim.Render("STREAMS"), ""}
		for _, i := range ready {
			l = append(l, line(i, false))
		}
		if len(ready) == 0 {
			l = append(l, sDim.Render("  none can sync right now"))
		}
		if len(unavail) > 0 {
			l = append(l, "", sDim.Render("UNAVAILABLE"), sDim.Render("no target connected"), "")
			for _, i := range unavail {
				l = append(l, line(i, true))
			}
		}
		side = lipgloss.NewStyle().Width(sw).PaddingLeft(1).Render(strings.Join(l, "\n"))
	}
	st := *m.stream()
	g, c := m.streamHealth(st)
	title := lipgloss.NewStyle().Foreground(c).Render(g) + " " + sBold.Render(st.Name)
	if !sidebar {
		title += sDim.Render(fmt.Sprintf("   (%d/%d, ↑↓ to switch)", m.sel+1, len(m.env.Cfg.Streams)))
	}
	s := st.Settings
	meta := fmt.Sprintf("trash kept %s · tripwire %d%% · verify %s · config mirror %s",
		daysText(s.TrashRetentionDays), s.MaxDeletePercent, onOff(s.VerifyAfter), onOff(s.MirrorConfig))
	if up, total := m.streamTargets(st); up == 0 {
		title += "  " + sYellow.Render("unavailable: no target connected")
	} else if up < total {
		title += "  " + sYellow.Render(fmt.Sprintf("%d of %d targets connected - backs up to those", up, total))
	}
	flowW := m.w - sw - 3
	main := title + "\n" + sDim.Render(meta) + "\n\n" + m.renderFlow(st, flowW)
	if !sidebar {
		return " " + strings.ReplaceAll(main, "\n", "\n ")
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, side, " ", main)
}

func daysText(d int) string {
	if d == 0 {
		return "forever"
	}
	return fmt.Sprintf("%dd", d)
}
func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// ---- plan screen -----------------------------------------------------

func (m *model) viewPlan() (string, string) {
	pv := m.plan
	if pv.preparing {
		return panel("Preparing backup plan", m.spin()+" checking source, targets and computing the dry run…\n"+sDim.Render("Nothing is being changed.")), "esc cancel"
	}
	var b strings.Builder
	runnable := 0
	for _, p := range pv.ps {
		b.WriteString(sBold.Render("→ "+p.Target.Name) + sDim.Render("  "+endpoint.Display(p.Target.Endpoint)) + "\n")
		for _, x := range p.Blocks {
			b.WriteString("  " + sRed.Render("✗ BLOCKED: "+x) + "\n")
		}
		if p.Runnable() {
			runnable++
			b.WriteString(fmt.Sprintf("  %s new   %s overwritten   %s to trash   %s\n",
				sGreen.Render(fmt.Sprint(p.Plan.New())), sYellow.Render(fmt.Sprint(p.Plan.Updated)),
				sYellow.Render(fmt.Sprint(p.Plan.Trashed)), humanBytes(p.Plan.Bytes)))
			for i, c := range p.Plan.Samples {
				if i >= 4 {
					b.WriteString(sDim.Render(fmt.Sprintf("    … and %d more", len(p.Plan.Samples)-4)) + "\n")
					break
				}
				b.WriteString(sDim.Render(fmt.Sprintf("    %-5s %s", c.Op, c.Path)) + "\n")
			}
			if p.Plan.InSync() && len(p.Needs) == 0 {
				b.WriteString("  " + sGreen.Render("✔ already in sync") + "\n")
			}
			for _, w := range p.Warnings {
				b.WriteString("  " + sYellow.Render("! "+w) + "\n")
			}
			for _, n := range p.Needs {
				b.WriteString("  " + sYellow.Render("⚠ needs confirmation: "+n.Prompt) + "\n")
			}
		}
		b.WriteString("\n")
	}
	keys := "esc cancel"
	if runnable > 0 {
		keys = "y start (you'll be asked to confirm anything risky)  esc cancel"
	}
	return panel("Backup plan - nothing has been changed yet", strings.TrimRight(b.String(), "\n")), keys
}
