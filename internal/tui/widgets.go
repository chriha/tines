package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// ---- picker ----------------------------------------------------------

type picker struct {
	items []string
	hints []string
	cur   int
}

func (p *picker) update(k tea.KeyMsg) (chosen bool) {
	switch k.String() {
	case "up", "k":
		if p.cur > 0 {
			p.cur--
		}
	case "down", "j":
		if p.cur < len(p.items)-1 {
			p.cur++
		}
	case "enter":
		return true
	}
	return false
}

func (p *picker) view() string {
	var b strings.Builder
	for i, it := range p.items {
		if i == p.cur {
			b.WriteString(sAccent.Render("› "+it) + "\n")
			if i < len(p.hints) && p.hints[i] != "" {
				b.WriteString("  " + sDim.Render(p.hints[i]) + "\n")
			}
		} else {
			b.WriteString("  " + it + "\n")
		}
	}
	return b.String()
}

// ---- form ------------------------------------------------------------

type fieldKind int

const (
	fText fieldKind = iota
	fBool
)

type formField struct {
	key, label, help string
	kind             fieldKind
	in               textinput.Model
	on               bool
}

type form struct {
	fields []*formField
	cur    int
}

func newText(key, label, value, placeholder string, secret bool) *formField {
	ti := textinput.New()
	ti.SetValue(value)
	ti.Placeholder = placeholder
	ti.CharLimit = 512
	ti.Width = 48
	ti.Prompt = ""
	if secret {
		ti.EchoMode = textinput.EchoPassword
	}
	return &formField{key: key, label: label, in: ti}
}

func newBool(key, label string, v bool, help string) *formField {
	return &formField{key: key, label: label, kind: fBool, on: v, help: help}
}

func (f *form) focus() {
	for i, fl := range f.fields {
		if fl.kind == fText {
			if i == f.cur {
				fl.in.Focus()
			} else {
				fl.in.Blur()
			}
		}
	}
}

func (f *form) value(key string) string {
	for _, fl := range f.fields {
		if fl.key == key {
			return strings.TrimSpace(fl.in.Value())
		}
	}
	return ""
}

func (f *form) flag(key string) bool {
	for _, fl := range f.fields {
		if fl.key == key {
			return fl.on
		}
	}
	return false
}

// update handles navigation; returns true when enter is pressed on the last field.
func (f *form) update(msg tea.Msg) (submit bool, cmd tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		switch k.String() {
		case "tab", "down":
			if f.cur < len(f.fields)-1 {
				f.cur++
				f.focus()
			}
			return false, nil
		case "shift+tab", "up":
			if f.cur > 0 {
				f.cur--
				f.focus()
			}
			return false, nil
		case "enter":
			if f.cur == len(f.fields)-1 {
				return true, nil
			}
			f.cur++
			f.focus()
			return false, nil
		case " ", "space":
			if fl := f.fields[f.cur]; fl.kind == fBool {
				fl.on = !fl.on
				return false, nil
			}
		case "ctrl+s":
			return true, nil
		}
	}
	fl := f.fields[f.cur]
	if fl.kind == fText {
		fl.in, cmd = fl.in.Update(msg)
	}
	return false, cmd
}

func (f *form) view() string {
	var b strings.Builder
	for i, fl := range f.fields {
		label := fl.label
		if i == f.cur {
			label = sAccent.Render("› " + label)
		} else {
			label = "  " + sDim.Render(label)
		}
		b.WriteString(label + "\n")
		if fl.kind == fBool {
			mark := "[ ]"
			if fl.on {
				mark = sGreen.Render("[x]")
			}
			b.WriteString("    " + mark + " " + sDim.Render(fl.help) + "\n")
		} else {
			b.WriteString("    " + fl.in.View() + "\n")
		}
	}
	return b.String()
}

// ---- typed confirmation ---------------------------------------------

type typed struct {
	title  string
	lines  []string
	expect string
	in     textinput.Model
	ok     func(m *model) tea.Cmd
	cancel func(m *model) tea.Cmd
	wrong  bool
}

func newTyped(title string, lines []string, expect string, ok, cancel func(m *model) tea.Cmd) *typed {
	ti := textinput.New()
	ti.Prompt = "› "
	ti.Focus()
	ti.Width = 30
	return &typed{title: title, lines: lines, expect: expect, in: ti, ok: ok, cancel: cancel}
}
