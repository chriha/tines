package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/chriha/tines/internal/endpoint"
	"github.com/chriha/tines/internal/engine"
)

// ---- messages and state ---------------------------------------------------

type dupScanMsg struct {
	id      int
	groups  []engine.DupeGroup
	skipped int
	err     error
}

type dupDelMsg struct {
	id     int
	done   []string // paths removed
	failed []string // "path: reason"
}

type dupRow struct{ g, f int } // group and file index of a selectable row

type dupView struct {
	id       int
	label    string // "Source of Photos", "Target SSD", or the path
	spec     string
	loading  bool
	deleting bool
	cancel   context.CancelFunc
	err      string
	skipped  int // files that could not be hashed
	groups   []engine.DupeGroup
	marked   map[string]bool // by path, unique within one scan
	rows     []dupRow
	lineOf   []int // display line of each row (group headers take a line)
	lines    int
	cur      int
	top      int
}

var dupScanSeq int

// ---- entry points -----------------------------------------------------------

// openDupMenu lets the user choose what to scan: the selected stream's source
// or one of its targets, or any other folder.
func (m *model) openDupMenu() {
	mv := &menuView{title: "Find duplicate files in…"}
	add := func(item, hint string, act func(m *model) tea.Cmd) {
		mv.pick.items = append(mv.pick.items, item)
		mv.pick.hints = append(mv.pick.hints, hint)
		mv.acts = append(mv.acts, act)
	}
	if st := m.stream(); st != nil {
		st := *st
		add("Source of "+st.Name, endpoint.Display(st.Source), func(m *model) tea.Cmd {
			return m.startDupScan("Source of "+st.Name, st.Source.Spec)
		})
		for _, t := range st.Targets {
			t := t
			add("Target "+t.Name, endpoint.Display(t.Endpoint), func(m *model) tea.Cmd {
				return m.startDupScan("Target "+t.Name, t.Spec)
			})
		}
	}
	add("Another folder…", "A local path or any rclone location, e.g. ~/Pictures or nas:photos", func(m *model) tea.Cmd {
		m.dpath = &form{fields: []*formField{newText("path", "Folder to scan", "", "~/Pictures", false)}}
		m.dpath.focus()
		m.screen = scDupPath
		return textinputBlink()
	})
	add("Cancel", "", func(m *model) tea.Cmd { m.screen = scDash; return nil })
	m.menu = mv
	m.screen = scMenu
}

func (m *model) dupPathKey(k tea.KeyMsg) tea.Cmd {
	if k.String() == "esc" {
		m.screen = scDash
		return nil
	}
	submit, c := m.dpath.update(k)
	if !submit {
		return c
	}
	p := m.dpath.value("path")
	if p == "" {
		m.fail("enter a folder to scan")
		return nil
	}
	if !strings.Contains(p, ":") { // plain local path
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				p = filepath.Join(home, strings.TrimPrefix(p, "~"))
			}
		}
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return m.startDupScan(p, p)
}

func (m *model) startDupScan(label, spec string) tea.Cmd {
	if dir, bad := endpoint.UnmountedVolume(spec); bad {
		m.fail(fmt.Sprintf("volume %s is not mounted", dir))
		m.screen = scDash
		return nil
	}
	dupScanSeq++
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	v := &dupView{id: dupScanSeq, label: label, spec: spec, loading: true, cancel: cancel, marked: map[string]bool{}}
	m.dup, m.screen = v, scDupes
	m.msg = ""
	return func() tea.Msg {
		g, skipped, err := engine.FindDuplicates(ctx, spec)
		return dupScanMsg{v.id, g, skipped, err}
	}
}

func (m *model) onDupScan(s dupScanMsg) {
	v := m.dup
	if v == nil || v.id != s.id {
		return
	}
	v.loading, v.skipped = false, s.skipped
	if s.err != nil {
		v.err = s.err.Error()
		return
	}
	v.groups = s.groups
	v.rebuild()
}

// rebuild recomputes the selectable rows and where they are drawn.
func (v *dupView) rebuild() {
	v.rows, v.lineOf, v.lines = v.rows[:0], v.lineOf[:0], 0
	for gi, g := range v.groups {
		v.lines++ // group header
		for fi := range g.Files {
			v.rows = append(v.rows, dupRow{gi, fi})
			v.lineOf = append(v.lineOf, v.lines)
			v.lines++
		}
	}
	v.cur = max(min(v.cur, len(v.rows)-1), 0)
}

func (v *dupView) markedIn(gi int) int {
	n := 0
	for _, f := range v.groups[gi].Files {
		if v.marked[f.Path] {
			n++
		}
	}
	return n
}

func (v *dupView) markedTotals() (files int, bytes int64) {
	for _, g := range v.groups {
		for _, f := range g.Files {
			if v.marked[f.Path] {
				files++
				bytes += f.Size
			}
		}
	}
	return
}

func (v *dupView) clamp(window int) {
	if len(v.rows) == 0 {
		v.cur, v.top = 0, 0
		return
	}
	v.cur = max(min(v.cur, len(v.rows)-1), 0)
	first, last := v.lineOf[v.cur], v.lineOf[v.cur]
	if v.rows[v.cur].f == 0 {
		first-- // keep the group header visible with its first file
	}
	if first < v.top {
		v.top = first
	}
	if last >= v.top+window {
		v.top = last - window + 1
	}
	v.top = max(min(v.top, v.lines-window), 0)
}

func (m *model) dupWindow() int { return max(m.h-4-4, 3) }

// ---- keys -------------------------------------------------------------------

func (m *model) dupKey(k tea.KeyMsg) tea.Cmd {
	v := m.dup
	if k.String() == "esc" || k.String() == "q" {
		if v.deleting {
			m.fail("deleting in progress - wait for it to finish")
			return nil
		}
		if v.cancel != nil {
			v.cancel()
		}
		m.screen, m.dup = scDash, nil
		return nil
	}
	if v.loading || v.deleting || v.err != "" {
		return nil
	}
	m.msg = ""
	page := max(m.dupWindow()-1, 1)
	switch k.String() {
	case "up", "k":
		v.cur--
	case "down", "j":
		v.cur++
	case "pgup", "ctrl+b", "b":
		v.cur -= page
	case "pgdown", "ctrl+f", "f":
		v.cur += page
	case "home", "g":
		v.cur = 0
	case "end", "G":
		v.cur = len(v.rows) - 1
	case "n", "tab", "right", "l": // next group
		if len(v.rows) > 0 {
			if gi := v.rows[v.cur].g + 1; gi < len(v.groups) {
				v.cur = v.firstRow(gi)
			}
		}
	case "p", "shift+tab", "left", "h": // previous group
		if len(v.rows) > 0 {
			if gi := v.rows[v.cur].g; v.rows[v.cur].f > 0 {
				v.cur = v.firstRow(gi)
			} else if gi > 0 {
				v.cur = v.firstRow(gi - 1)
			}
		}
	case " ", "space", "x":
		m.dupToggle()
	case "a": // mark every copy except the oldest of each group
		for _, g := range v.groups {
			for _, f := range g.Files[1:] {
				v.marked[f.Path] = true
			}
		}
		m.say("marked all copies except the oldest of each group - review, then press d")
	case "c":
		v.marked = map[string]bool{}
	case "d", "delete":
		return m.dupAskDelete()
	}
	v.clamp(m.dupWindow())
	return nil
}

func (v *dupView) firstRow(gi int) int {
	for i, r := range v.rows {
		if r.g == gi {
			return i
		}
	}
	return v.cur
}

// dupToggle marks or unmarks the file under the cursor. At least one copy of
// every group always stays unmarked.
func (m *model) dupToggle() {
	v := m.dup
	if len(v.rows) == 0 {
		return
	}
	r := v.rows[v.cur]
	f := v.groups[r.g].Files[r.f]
	if v.marked[f.Path] {
		delete(v.marked, f.Path)
		return
	}
	if v.markedIn(r.g)+1 >= len(v.groups[r.g].Files) {
		m.fail("keep at least one copy - unmark another file of this group first")
		return
	}
	v.marked[f.Path] = true
}

func (m *model) dupAskDelete() tea.Cmd {
	v := m.dup
	n, bytes := v.markedTotals()
	if n == 0 {
		m.fail("nothing marked - press space on a file to mark it for deletion")
		return nil
	}
	if len(m.running) > 0 {
		m.fail("a backup is running - wait for it to finish before deleting files")
		return nil
	}
	lines := []string{
		fmt.Sprintf("Permanently delete %s (%s) from %s?", plural(n, "file"), humanBytes(bytes), v.label),
		sRed.Render("They are NOT moved to the trash. This cannot be undone."),
		sDim.Render("At least one copy of every group is kept."),
		"",
		"Type  " + sBold.Render("delete") + "  to confirm, or press esc to cancel.",
	}
	m.typ = newTyped("Delete duplicates", lines, "delete",
		func(m *model) tea.Cmd { return m.dupDelete() },
		func(m *model) tea.Cmd { m.screen = scDupes; return nil })
	m.screen = scTyped
	return textinputBlink()
}

func plural(n int, w string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", w)
	}
	return fmt.Sprintf("%d %ss", n, w)
}

func (m *model) dupDelete() tea.Cmd {
	v := m.dup
	m.screen = scDupes
	var paths []string
	for _, g := range v.groups {
		for _, f := range g.Files {
			if v.marked[f.Path] {
				paths = append(paths, f.Path)
			}
		}
	}
	v.deleting = true
	m.say(fmt.Sprintf("deleting %s…", plural(len(paths), "file")))
	id, spec := v.id, v.spec
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		res := dupDelMsg{id: id}
		for _, p := range paths {
			if err := engine.DeleteFile(ctx, spec, p); err != nil {
				res.failed = append(res.failed, p+": "+err.Error())
				continue
			}
			res.done = append(res.done, p)
		}
		return res
	}
}

func (m *model) onDupDeleted(d dupDelMsg) {
	v := m.dup
	if v == nil || v.id != d.id {
		return
	}
	v.deleting = false
	gone := map[string]bool{}
	for _, p := range d.done {
		gone[p] = true
		delete(v.marked, p)
	}
	var left []engine.DupeGroup
	for _, g := range v.groups {
		var keep []engine.DupeFile
		for _, f := range g.Files {
			if !gone[f.Path] {
				keep = append(keep, f)
			}
		}
		if len(keep) > 1 { // a lone file is no duplicate any more
			g.Files = keep
			left = append(left, g)
		}
	}
	v.groups = left
	v.rebuild()
	v.clamp(m.dupWindow())
	if len(d.failed) > 0 {
		m.fail(fmt.Sprintf("deleted %d, %d failed: %s", len(d.done), len(d.failed), trunc(d.failed[0], max(m.w-40, 20))))
		return
	}
	m.say(fmt.Sprintf("deleted %s", plural(len(d.done), "file")))
}

// ---- view ---------------------------------------------------------------------

func (m *model) viewDupes() (string, string) {
	v := m.dup
	title := sBold.Render("Duplicate files") + sDim.Render("  "+v.label)
	switch {
	case v.loading:
		return " " + title + "\n\n " + m.spin() + " listing files and comparing checksums of same-sized candidates…\n " +
			sDim.Render("Read-only. Large folders or remote locations can take a while."), "esc cancel"
	case v.err != "":
		return " " + title + "\n\n " + sRed.Render("✗ "+trunc(v.err, max(m.w-6, 20))), "esc close"
	}
	var b strings.Builder
	b.WriteString(" " + title + "\n")
	if len(v.groups) == 0 {
		b.WriteString("\n " + sGreen.Render("✔ no duplicates found"))
		if v.skipped > 0 {
			b.WriteString(sYellow.Render(fmt.Sprintf("  (%d files could not be checksummed)", v.skipped)))
		}
		return b.String(), "esc close"
	}
	files := 0
	var reclaim int64
	for _, g := range v.groups {
		files += len(g.Files)
		reclaim += g.Reclaimable()
	}
	sum := fmt.Sprintf("%s in %s · %s reclaimable", plural(files, "file"), plural(len(v.groups), "group"), sBold.Render(humanBytes(reclaim)))
	if n, by := v.markedTotals(); n > 0 {
		sum += "   " + sRed.Render(fmt.Sprintf("%s marked (%s)", plural(n, "file"), humanBytes(by)))
	}
	if v.skipped > 0 {
		sum += sYellow.Render(fmt.Sprintf("   %d unchecked", v.skipped))
	}
	b.WriteString(" " + sum + "\n")
	b.WriteString(" " + sDim.Render(strings.Repeat("─", max(m.w-2, 10))) + "\n")

	const wSize, wDate, wTag = 10, 11, 10
	wPath := max(m.w-2-1-4-wSize-wDate-wTag-3, 12)
	window := m.dupWindow()
	lines := make([]string, 0, v.lines)
	for gi, g := range v.groups {
		lines = append(lines, "   "+sAccent.Render(fmt.Sprintf("Group %d/%d", gi+1, len(v.groups)))+
			sDim.Render(fmt.Sprintf("  %d copies · %s each · %s reclaimable", len(g.Files), humanBytes(g.Size), humanBytes(g.Reclaimable()))))
		for fi, f := range g.Files {
			ri := len(lines) - 1 - gi // row index: lines so far minus the headers
			cursor := "  "
			if ri == v.cur {
				cursor = sAccent.Render("› ")
			}
			mark := "[ ]"
			if v.marked[f.Path] {
				mark = sRed.Render("[x]")
			}
			date := "—"
			if !f.ModTime.IsZero() {
				date = f.ModTime.Format("2006-01-02")
			}
			tag := ""
			if fi == 0 {
				tag = "oldest"
			}
			path := pad(truncLeft(f.Path, wPath), wPath)
			if v.marked[f.Path] {
				path = sRed.Render(path)
			}
			lines = append(lines, " "+cursor+mark+" "+path+" "+fmt.Sprintf("%*s", wSize, humanBytes(f.Size))+" "+sDim.Render(pad(date, wDate-1)+" "+tag))
		}
	}
	end := min(v.top+window, len(lines))
	vis := lines[min(v.top, len(lines)):end]
	for i := range vis {
		vis[i] = trunc(vis[i], m.w-1)
	}
	for len(vis) < window {
		vis = append(vis, "")
	}
	b.WriteString(strings.Join(vis, "\n"))
	pos := fmt.Sprintf("file %d of %d", v.cur+1, len(v.rows))
	if v.cur < len(v.rows) {
		r := v.rows[v.cur]
		pos += "  ·  " + v.groups[r.g].Files[r.f].Path
	}
	b.WriteString("\n " + sDim.Render(trunc(pos, m.w-2)))
	keys := "↑↓ move  n/p group  space mark  a mark all but oldest  c clear  d delete marked  esc close"
	if v.deleting {
		keys = "deleting…"
	}
	return b.String(), keys
}
