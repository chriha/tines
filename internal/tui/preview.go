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
	"github.com/chriha/tines/internal/runner"
)

type previewMsg struct {
	sid string
	pv  *runner.Preview
}

type pvFilter int

const (
	pfAll pvFilter = iota
	pfChanges
	pfSynced
	pfExcluded
	pfCount
)

func (f pvFilter) label() string {
	return [...]string{"all files", "changes only", "synced only", "not synced"}[f]
}

func (f pvFilter) keep(s runner.RowState) bool {
	switch f {
	case pfChanges:
		return s.Changes()
	case pfSynced:
		return s.Synced()
	case pfExcluded:
		return !s.Synced()
	}
	return true
}

type previewView struct {
	stream  config.Stream
	loading bool
	pv      *runner.Preview
	ti      int          // selected target
	rows    []runner.Row // all rows for the selected target
	shown   []int        // indexes into rows that pass the filter
	sum     runner.Summary
	filter  pvFilter
	cur     int // index into shown
	top     int // first visible index into shown
}

func (m *model) openPreview(st config.Stream) tea.Cmd {
	m.pvw = &previewView{stream: st, loading: true}
	m.screen = scPreview
	return m.loadPreview()
}

func (m *model) loadPreview() tea.Cmd {
	st, env := m.pvw.stream, m.env
	m.pvw.loading = true
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		return previewMsg{st.ID, env.BuildPreview(ctx, st)}
	}
}

func (m *model) onPreview(p previewMsg) {
	v := m.pvw
	if v == nil || v.stream.ID != p.sid {
		return
	}
	v.loading, v.pv = false, p.pv
	v.rebuild(true)
}

// rebuild recomputes rows and the filtered view. keep=false resets the cursor.
func (v *previewView) rebuild(reset bool) {
	var path string
	if !reset && v.cur < len(v.shown) {
		path = v.rows[v.shown[v.cur]].Path
	}
	v.rows = v.pv.Rows(v.ti)
	v.sum = runner.Summarize(v.rows)
	v.shown = v.shown[:0]
	for i, r := range v.rows {
		if v.filter.keep(r.State) {
			v.shown = append(v.shown, i)
		}
	}
	v.cur, v.top = 0, 0
	if path != "" { // keep the cursor on the same file when possible
		for i, ri := range v.shown {
			if v.rows[ri].Path == path {
				v.cur = i
				break
			}
		}
	}
}

func (m *model) previewKey(k tea.KeyMsg) tea.Cmd {
	v := m.pvw
	page := max(m.previewRows()-1, 1)
	switch k.String() {
	case "esc", "q", "s":
		m.screen, m.pvw = scDash, nil
		return nil
	case "r":
		if !v.loading {
			return m.loadPreview()
		}
	}
	if v.loading || v.pv == nil {
		return nil
	}
	switch k.String() {
	case "up", "k":
		v.cur--
	case "down", "j":
		v.cur++
	case "pgup", "ctrl+b", "b":
		v.cur -= page
	case "pgdown", "ctrl+f", "f", " ":
		v.cur += page
	case "ctrl+u":
		v.cur -= page / 2
	case "ctrl+d":
		v.cur += page / 2
	case "home", "g":
		v.cur = 0
	case "end", "G":
		v.cur = len(v.shown) - 1
	case "tab", "right", "l":
		v.ti = (v.ti + 1) % len(v.pv.Targets)
		v.rebuild(false)
	case "shift+tab", "left", "h":
		v.ti = (v.ti + len(v.pv.Targets) - 1) % len(v.pv.Targets)
		v.rebuild(false)
	case "1", "2", "3", "4":
		v.filter = pvFilter(k.String()[0] - '1')
		v.rebuild(false)
	case "m": // cycle the filter
		v.filter = (v.filter + 1) % pfCount
		v.rebuild(false)
	}
	v.clamp(m.previewRows())
	return nil
}

func (v *previewView) clamp(window int) {
	v.cur = max(min(v.cur, len(v.shown)-1), 0)
	if v.cur < v.top {
		v.top = v.cur
	}
	if v.cur >= v.top+window {
		v.top = v.cur - window + 1
	}
	v.top = max(min(v.top, len(v.shown)-window), 0)
}

// previewRows is the number of file rows that fit on screen.
func (m *model) previewRows() int {
	// View() reserves head, blank line, status and footer (4 lines); the
	// preview adds title, target tabs, summary, rule and a scroll line.
	return max(m.h-4-5, 3)
}

func truncLeft(s string, w int) string {
	if sw := ansi.StringWidth(s); sw > w && w > 1 {
		return ansi.TruncateLeft(s, sw-w+1, "…")
	}
	return s
}

type rowLook struct {
	tag   string
	style lipgloss.Style
	note  string
}

func look(s runner.RowState) rowLook {
	switch s {
	case runner.RowNew:
		return rowLook{"+ new", sGreen, ""}
	case runner.RowUpdate:
		return rowLook{"~ update", sYellow, ""}
	case runner.RowTrash:
		return rowLook{"- to trash", sRed, ""}
	case runner.RowSame:
		return rowLook{"= in sync", lipgloss.NewStyle().Foreground(cText), ""}
	case runner.RowUnknown:
		return rowLook{"? unknown", lipgloss.NewStyle().Foreground(cText), ""}
	case runner.RowExclRule:
		return rowLook{"· skipped", sDim, "excluded by your rules"}
	}
	return rowLook{"· skipped", sDim, "system file"}
}

func (m *model) viewPreview() (string, string) {
	v := m.pvw
	keys := "↑↓ scroll  pgup/pgdn page  g/G ends  tab target  m filter  r reload  esc close"
	title := sBold.Render("Sync preview") + sDim.Render("  "+v.stream.Name+" · read-only, nothing is changed")
	if v.loading {
		return " " + title + "\n\n " + m.spin() + " listing the source and computing what each target would receive…", "esc close"
	}
	if v.pv.SourceErr != "" {
		return " " + title + "\n\n " + sRed.Render("✗ cannot read the source: "+v.pv.SourceErr), "r retry  esc close"
	}
	var b strings.Builder
	b.WriteString(" " + title + "\n")

	// target tabs
	var tabs []string
	for i, tp := range v.pv.Targets {
		c := m.conns["tgt:"+tp.Target.ID]
		label := dot(c, false) + " " + tp.Target.Name
		if i == v.ti {
			label = sAccent.Render("[") + sBold.Render(label) + sAccent.Render("]")
		} else {
			label = " " + sDim.Render(label) + " "
		}
		tabs = append(tabs, label)
	}
	b.WriteString(" " + strings.Join(tabs, " ") + "\n")

	// summary
	tp := v.pv.Targets[v.ti]
	s := v.sum
	sum := fmt.Sprintf("%s  %s  %s  %s  %s", sGreen.Render(fmt.Sprintf("+%d new", s.New)), sYellow.Render(fmt.Sprintf("~%d updated", s.Update)),
		sRed.Render(fmt.Sprintf("-%d to trash", s.Trash)), fmt.Sprintf("%d in sync", s.Same), sDim.Render(fmt.Sprintf("%d not synced", s.Excluded)))
	if s.Bytes > 0 {
		sum += sDim.Render("  " + humanBytes(s.Bytes) + " to copy")
	}
	if !tp.Compare {
		sum = sYellow.Render("target not compared: "+trunc(tp.Err, max(m.w-30, 20))) + sDim.Render(fmt.Sprintf("  %d synced · %d not synced", s.Unknown, s.Excluded))
	}
	b.WriteString(" " + sum + sDim.Render("   ["+v.filter.label()+"]") + "\n")
	b.WriteString(" " + sDim.Render(strings.Repeat("─", max(m.w-2, 10))) + "\n")

	// file rows
	window := m.previewRows()
	const wTag, wSize = 11, 10
	const wNote = 26 // room for "excluded by your rules" on wide terminals
	wPath := max(m.w-wTag-wSize-8, 12)
	showNote := m.w >= 100
	if showNote {
		wPath = max(wPath-wNote, 12)
	}
	var lines []string
	if len(v.shown) == 0 {
		msg := "nothing to show"
		if len(v.rows) == 0 {
			msg = "the source has no files"
		}
		lines = append(lines, "   "+sDim.Render(msg))
	}
	end := min(v.top+window, len(v.shown))
	for i := v.top; i < end; i++ {
		r := v.rows[v.shown[i]]
		lk := look(r.State)
		path := pad(truncLeft(r.Path, wPath), wPath)
		size := fmt.Sprintf("%*s", wSize, humanBytes(r.Size))
		line := lk.style.Render(pad(lk.tag, wTag) + path + " " + size)
		if lk.note != "" && showNote {
			line += sDim.Render("  " + lk.note)
		}
		cursor := "  "
		if i == v.cur {
			cursor = sAccent.Render("› ")
		}
		lines = append(lines, " "+cursor+trunc(line, m.w-4))
	}
	for len(lines) < window {
		lines = append(lines, "")
	}
	b.WriteString(strings.Join(lines, "\n"))
	pos := "0 files"
	if len(v.shown) > 0 {
		pos = fmt.Sprintf("%d–%d of %d", v.top+1, end, len(v.shown))
	}
	b.WriteString("\n " + sDim.Render(pos))
	return b.String(), keys
}
