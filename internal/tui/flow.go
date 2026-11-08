package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/chrishartmann/tines/internal/config"
	"github.com/chrishartmann/tines/internal/endpoint"
	"github.com/chrishartmann/tines/internal/state"
)

var (
	cGreen  = lipgloss.AdaptiveColor{Light: "#1a8f3c", Dark: "#4ade80"}
	cRed    = lipgloss.AdaptiveColor{Light: "#c62828", Dark: "#f87171"}
	cYellow = lipgloss.AdaptiveColor{Light: "#b7791f", Dark: "#fbbf24"}
	cCyan   = lipgloss.AdaptiveColor{Light: "#0e7490", Dark: "#22d3ee"}
	cDim    = lipgloss.AdaptiveColor{Light: "#6b7280", Dark: "#6b7280"}
	cText   = lipgloss.AdaptiveColor{Light: "#111827", Dark: "#e5e7eb"}
	cAccent = lipgloss.AdaptiveColor{Light: "#6d28d9", Dark: "#a78bfa"}

	sDim    = lipgloss.NewStyle().Foreground(cDim)
	sBold   = lipgloss.NewStyle().Bold(true).Foreground(cText)
	sGreen  = lipgloss.NewStyle().Foreground(cGreen)
	sRed    = lipgloss.NewStyle().Foreground(cRed)
	sYellow = lipgloss.NewStyle().Foreground(cYellow)
	sCyan   = lipgloss.NewStyle().Foreground(cCyan)
	sAccent = lipgloss.NewStyle().Foreground(cAccent).Bold(true)
)

// ---- per-node view state --------------------------------------------

type conn struct {
	Checked bool
	OK      bool
	Missing bool // reachable but directory absent
	Latency time.Duration
	Err     string
}

type runView struct {
	Phase    string // preparing | syncing | verifying | done
	Pct      int
	Bytes    int64
	Total    int64
	Speed    float64
	ETA      time.Duration
	Current  string
	Files    string
	Done     bool
	OK       bool
	Msg      string
	Warnings []string
}

type nodeLook struct {
	Color lipgloss.TerminalColor
	Flow  string // idle | active | down | ok | behind
}

func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return ansi.Truncate(s, n, "…")
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

func humanBytes(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

func dot(c conn, running bool) string {
	switch {
	case !c.Checked:
		return sDim.Render("◌")
	case c.OK && !c.Missing:
		return sGreen.Render("●")
	case c.Missing:
		return sYellow.Render("◐")
	}
	return sRed.Render("○")
}

func connText(c conn) string {
	switch {
	case !c.Checked:
		return sDim.Render("checking…")
	case c.OK:
		return sDim.Render(fmt.Sprintf("%dms", c.Latency.Milliseconds()))
	case c.Missing:
		return sYellow.Render("dir missing")
	}
	return sRed.Render("offline")
}

func bar(pct, w int) string {
	if w < 4 {
		return ""
	}
	f := pct * w / 100
	if f > w {
		f = w
	}
	return sCyan.Render(strings.Repeat("█", f)) + sDim.Render(strings.Repeat("░", w-f))
}

const boxInner = 5 // content lines per box

func box(lines []string, w int, color lipgloss.TerminalColor, title string) []string {
	cw := w - 4
	for len(lines) < boxInner {
		lines = append(lines, "")
	}
	for i := range lines {
		lines[i] = trunc(lines[i], cw)
	}
	st := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(color).Padding(0, 1).Width(w - 2).Height(boxInner)
	return strings.Split(st.Render(strings.Join(lines, "\n")), "\n")
}

// ---- boxes -----------------------------------------------------------

func (m *model) sourceBox(st config.Stream, w int) ([]string, nodeLook) {
	c := m.conns["src:"+st.ID]
	color := lipgloss.TerminalColor(cDim)
	flow := "idle"
	switch {
	case c.Checked && c.OK:
		color, flow = cGreen, "ok"
	case c.Checked:
		color, flow = cRed, "down"
	}
	lines := []string{
		dot(c, false) + " " + sBold.Render("SOURCE") + "  " + connText(c),
		sDim.Render(endpoint.KindByID(st.Source.Kind).Label),
		trunc(endpoint.Display(st.Source), w-4),
	}
	if !c.OK && c.Checked && c.Err != "" {
		lines = append(lines, sRed.Render(trunc(c.Err, w-4)))
	}
	return box(lines, w, color, ""), nodeLook{color, flow}
}

func (m *model) targetBox(st config.Stream, t config.Target, w int, selected bool) ([]string, nodeLook) {
	c := m.conns["tgt:"+t.ID]
	ts, _ := m.env.State.Get(st.ID, t.ID)
	rv := m.runs[t.ID]
	l1 := dot(c, false) + " " + sBold.Render(trunc(t.Name, w-18)) + "  " + connText(c)
	l2 := sDim.Render(endpoint.Display(t.Endpoint))
	var l3, l4, l5 string
	color := lipgloss.TerminalColor(cDim)
	flow := "idle"

	switch {
	case rv != nil && !rv.Done:
		color, flow = cCyan, "active"
		switch rv.Phase {
		case "preparing":
			l3 = sCyan.Render(m.spin() + " preparing…")
		case "verifying":
			l3 = sCyan.Render(m.spin() + " verifying checksums…")
		default:
			l3 = sCyan.Render(m.spin()+" syncing ") + fmt.Sprintf("%d%%", rv.Pct) +
				sCyan.Render(fmt.Sprintf("  ⚡ %s/s", humanBytes(int64(rv.Speed))))
			if rv.Current != "" {
				l2 = sDim.Render(trunc(rv.Current, w-4))
			}
			l4 = bar(rv.Pct, w-8-0)
			eta := ""
			if rv.ETA > 0 {
				eta = "  ETA " + rv.ETA.Round(time.Second).String()
			}
			l5 = sDim.Render(fmt.Sprintf("%s / %s%s", humanBytes(rv.Bytes), humanBytes(rv.Total), eta))
		}
	case c.Checked && !c.OK && !c.Missing:
		color, flow = cRed, "down"
		l3 = sRed.Render("✗ cannot reach target")
		l4 = sDim.Render("last backup: ") + lastText(ts)
		l5 = sDim.Render(trunc(c.Err, w-4))
	default:
		switch {
		case ts.Behind == 0 && !ts.CheckedAt.IsZero():
			color, flow = cGreen, "ok"
			l3 = sGreen.Render("✔ in sync") + sDim.Render("  checked "+ago(ts.CheckedAt))
		case ts.Behind > 0:
			color, flow = cYellow, "behind"
			l3 = sYellow.Render(fmt.Sprintf("◐ %d file(s) behind", ts.Behind))
			if ts.BehindBytes > 0 {
				l3 += sDim.Render("  " + humanBytes(ts.BehindBytes))
			}
		default:
			l3 = sDim.Render("? sync state unknown — press c")
		}
		if c.Missing {
			l3 = sYellow.Render("◐ directory will be created on first run")
		}
		l4 = sDim.Render("last backup: ") + lastText(ts)
		switch {
		case !ts.LastVerify.IsZero() && ts.VerifyOK:
			l5 = sGreen.Render("✔ verified ") + sDim.Render(ago(ts.LastVerify))
		case !ts.LastVerify.IsZero():
			l5 = sRed.Render("✗ verify failed ") + sDim.Render(ago(ts.LastVerify))
		}
		if rv != nil && rv.Done && !rv.OK {
			l5 = sRed.Render(trunc("✗ "+rv.Msg, w-4))
		}
	}
	if selected {
		color = lipgloss.TerminalColor(color)
	}
	return box([]string{l1, l2, l3, l4, l5}, w, color, ""), nodeLook{color, flow}
}

func lastText(ts state.TargetState) string {
	switch {
	case ts.LastRun.IsZero():
		return sDim.Render("never")
	case ts.LastStatus == "ok":
		return sGreen.Render(ago(ts.LastOK) + " ✔")
	case ts.LastStatus == "cancelled":
		return sYellow.Render(ago(ts.LastRun) + " cancelled")
	}
	return sRed.Render(ago(ts.LastRun) + " failed")
}

func (m *model) spin() string {
	f := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	return f[m.frame%len(f)]
}

// ---- flow chart ------------------------------------------------------

const (
	bU = 1 << iota
	bD
	bL
	bR
)

var glyph = map[int]string{
	0: " ", bU: "│", bD: "│", bU | bD: "│", bL: "─", bR: "─", bL | bR: "─",
	bU | bR: "└", bU | bL: "┘", bD | bR: "┌", bD | bL: "┐",
	bU | bD | bR: "├", bU | bD | bL: "┤", bU | bL | bR: "┴", bD | bL | bR: "┬", bU | bD | bL | bR: "┼",
}

func (m *model) flowStyle(l nodeLook) lipgloss.Style {
	return lipgloss.NewStyle().Foreground(l.Color)
}

// renderFlow draws SOURCE ──┬──▶ target boxes, lines colored by state, with
// a moving dot on lines that are transferring data right now.
func (m *model) renderFlow(st config.Stream, width int) string {
	const connW, trunk = 9, 2
	// Source and target boxes share one width.
	boxW := (width - connW) / 2
	if boxW < 32 {
		boxW = 32
	}
	srcW, tgtW := boxW, boxW
	n := len(st.Targets)
	boxH := boxInner + 2
	H := n * boxH

	srcBox, srcLook := m.sourceBox(st, srcW)
	top := (H - boxH) / 2
	rs := top + boxH/2

	tgtLines := make([]string, 0, H)
	looks := make([]nodeLook, n)
	mids := make([]int, n)
	for i, t := range st.Targets {
		b, lk := m.targetBox(st, t, tgtW, i == m.tsel)
		tgtLines = append(tgtLines, b...)
		looks[i] = lk
		mids[i] = i*boxH + boxH/2
	}

	lo, hi := rs, rs
	if mids[0] < lo {
		lo = mids[0]
	}
	if mids[n-1] > hi {
		hi = mids[n-1]
	}
	isMid := map[int]int{}
	for i, r := range mids {
		isMid[r] = i
	}

	var out []string
	for row := 0; row < H; row++ {
		// source column
		left := strings.Repeat(" ", srcW)
		if row >= top && row < top+boxH {
			left = srcBox[row-top]
		}
		// connector column
		cells := make([]string, connW)
		for i := range cells {
			cells[i] = " "
		}
		trunkStyle := sDim
		put := func(x int, bits int, s lipgloss.Style) { cells[x] = s.Render(glyph[bits]) }
		ti, onTarget := isMid[row]
		if row == rs {
			s := m.flowStyle(srcLook)
			for x := 0; x < trunk; x++ {
				put(x, bL|bR, s)
			}
		}
		if row >= lo && row <= hi {
			bits := 0
			if row > lo {
				bits |= bU
			}
			if row < hi {
				bits |= bD
			}
			if row == rs {
				bits |= bL
			}
			if onTarget {
				bits |= bR
			}
			s := trunkStyle
			if row == rs {
				s = m.flowStyle(srcLook)
			}
			if onTarget {
				s = m.flowStyle(looks[ti])
			}
			put(trunk, bits, s)
		}
		if onTarget {
			lk := looks[ti]
			s := m.flowStyle(lk)
			span := connW - 1 - (trunk + 1)
			for k := 0; k < span; k++ {
				ch := "─"
				switch lk.Flow {
				case "down":
					ch = "┈"
				case "active":
					if k == (m.frame/2)%span {
						ch = "●"
					}
				}
				cells[trunk+1+k] = s.Render(ch)
			}
			arrow := "▶"
			if lk.Flow == "down" {
				arrow = "▷"
			}
			cells[connW-1] = s.Render(arrow)
		}
		right := ""
		if row < len(tgtLines) {
			right = tgtLines[row]
		}
		out = append(out, left+strings.Join(cells, "")+right)
	}
	return strings.Join(out, "\n")
}
