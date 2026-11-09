package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/engine"
	"github.com/chriha/tines/internal/runner"
	"github.com/chriha/tines/internal/state"
)

func testModel(t *testing.T) (*model, string) {
	t.Helper()
	if _, err := engine.Available(); err != nil {
		t.Skip("rclone not installed")
	}
	t.Setenv("TINES_STATE_DIR", t.TempDir())
	root := t.TempDir()
	src := filepath.Join(root, "src")
	for f, c := range map[string]string{"a.txt": "A", "sub/b.txt": "B", "sub/skip.tmp": "T", ".DS_Store": "S"} {
		p := filepath.Join(src, f)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	up, down := filepath.Join(root, "up"), "/Volumes/tines-test-unplugged/x"
	_ = os.MkdirAll(up, 0o755)
	cfg := config.New()
	cfg.Targets = []config.Target{
		{ID: "tu", Name: "Desk SSD", Type: config.TypeSSD, Endpoint: config.Endpoint{Kind: "local", Spec: up}},
		{ID: "td", Name: "Old stick", Type: config.TypeUSB, Endpoint: config.Endpoint{Kind: "local", Spec: down}},
	}
	set := config.DefaultSettings()
	set.Excludes = []string{"*.tmp"}
	mk := func(id, name string, ts ...config.Target) config.Stream {
		return config.Stream{ID: id, Name: name, Source: config.Endpoint{Kind: "local", Spec: src}, Targets: ts, Settings: set}
	}
	cfg.Streams = []config.Stream{
		mk("s1", "Offline only", cfg.Targets[1]),
		mk("s2", "Both", cfg.Targets[0], cfg.Targets[1]),
		mk("s3", "Online only", cfg.Targets[0]),
	}
	st, _ := state.Open()
	m := &model{env: &runner.Env{Cfg: cfg, CfgPath: filepath.Join(root, "c.yaml"), State: st},
		conns: map[string]conn{}, inflight: map[string]bool{}, runs: map[string]*runView{},
		checking: map[string]bool{}, autoChk: map[string]bool{}, capTried: map[string]time.Time{}, forceCap: map[string]bool{},
		running: map[string]context.CancelFunc{}, ch: make(chan tea.Msg, 512), tsel: -1, w: 140, h: 40}
	m.conns["tgt:tu"] = conn{Checked: true, OK: true, Latency: 3 * time.Millisecond}
	m.conns["tgt:td"] = conn{Checked: true, Err: "volume /Volumes/tines-test-unplugged is not mounted"}
	return m, root
}

func plain(s string) string { return ansi.Strip(s) }

func TestStreamsSplitIntoReadyAndUnavailable(t *testing.T) {
	m, _ := testModel(t)
	ready, un := m.streamOrder()
	if len(ready) != 2 || ready[0] != 1 || ready[1] != 2 || len(un) != 1 || un[0] != 0 {
		t.Fatalf("ready=%v unavailable=%v", ready, un)
	}
	out := plain(m.viewDash())
	iReady, iUn := strings.Index(out, "Online only"), strings.Index(out, "UNAVAILABLE")
	iOff := strings.LastIndex(out, "Offline only") // the first hit is the main title
	if !(iReady >= 0 && iReady < iUn && iUn < iOff) {
		t.Fatalf("unavailable stream must be listed below the syncable ones:\n%s", out)
	}
	// navigation follows display order: Both -> Online only -> Offline only
	m.sel = 1
	m.moveSel(1)
	m.moveSel(1)
	if m.sel != 0 {
		t.Fatalf("down twice from 'Both' should reach the unavailable stream, got %d", m.sel)
	}
	if cmd := m.dashKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("r")}); cmd != nil || !m.msgErr {
		t.Fatalf("running an unavailable stream must be refused (msg=%q)", m.msg)
	}
	// an unchecked target counts as connected so nothing flickers on startup
	delete(m.conns, "tgt:td")
	if up, _ := m.streamTargets(m.env.Cfg.Streams[0]); up != 1 {
		t.Fatal("unchecked target should count as up")
	}
}

func TestTargetsViewShowsCapacityAndLastKnown(t *testing.T) {
	m, _ := testModel(t)
	m.env.State.SetCapacity("tu", 1000<<30, 400<<30)
	m.env.State.SetCapacity("td", 16<<30, 3<<30)
	m.screen = scTargets
	body, _ := m.viewTargets()
	out := plain(body)
	for _, want := range []string{"Desk SSD", "SSD", "Old stick", "USB stick", "1000.0 GiB", "400.0 GiB", "16.0 GiB", "3.0 GiB", "connected", "not connected"} {
		if !strings.Contains(out, want) {
			t.Errorf("targets view lacks %q:\n%s", want, out)
		}
	}
	m.tv.sel = 1
	body, _ = m.viewTargets()
	if !strings.Contains(plain(body), "last known 3.0 GiB left of 16.0 GiB") {
		t.Errorf("unplugged target must show last known capacity:\n%s", plain(body))
	}
}

func TestRemovingUsedTargetIsRefused(t *testing.T) {
	m, _ := testModel(t)
	m.screen = scTargets
	m.targetsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	if !m.msgErr || m.screen != scTargets || len(m.env.Cfg.Targets) != 2 {
		t.Fatalf("a target used by a stream must not be removable (msg=%q)", m.msg)
	}
}

func TestAddTargetRequiresConnection(t *testing.T) {
	m, root := testModel(t)
	m.startTargetWizard(nil, scTargets)
	w := m.tw
	w.name.fields[0].in.SetValue("New stick")
	m.tgKey(tea.KeyMsg{Type: tea.KeyEnter}) // name -> kind
	m.tgKey(tea.KeyMsg{Type: tea.KeyEnter}) // local folder
	w.fields.fields[0].in.SetValue("/Volumes/tines-test-unplugged/new")
	cmd := m.tgKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatalf("expected a connection test, got err=%q blocker=%q", w.err, w.blocker)
	}
	m.onTgTest(cmd().(tgTestMsg))
	if w.step != tgResult || w.blocker == "" {
		t.Fatalf("unplugged target must be blocked: step=%v blocker=%q", w.step, w.blocker)
	}
	if m.tgKey(tea.KeyMsg{Type: tea.KeyEnter}); len(m.env.Cfg.Targets) != 2 || w.step != tgResult {
		t.Fatal("blocked target must not advance or be added")
	}

	// a connected one is accepted, its capacity stored
	m.tgKey(tea.KeyMsg{Type: tea.KeyEsc})
	good := filepath.Join(root, "good")
	_ = os.MkdirAll(good, 0o755)
	w.fields.fields[0].in.SetValue(good)
	cmd = m.tgKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.onTgTest(cmd().(tgTestMsg))
	if w.blocker != "" || !w.capOK {
		t.Fatalf("connected target should pass with capacity: blocker=%q capOK=%v", w.blocker, w.capOK)
	}
	m.tgKey(tea.KeyMsg{Type: tea.KeyEnter}) // -> type picker
	m.tgKey(tea.KeyMsg{Type: tea.KeyEnter}) // accept default type
	if len(m.env.Cfg.Targets) != 3 || m.screen != scTargets {
		t.Fatalf("target not added (targets=%d screen=%v err=%q)", len(m.env.Cfg.Targets), m.screen, w.err)
	}
	added := m.env.Cfg.Targets[2]
	if d := m.env.State.Device(added.ID); !d.KnownCapacity() || d.Free <= 0 {
		t.Fatalf("capacity not stored: %+v", d)
	}
	if reloaded, err := config.Load(m.env.CfgPath); err != nil || len(reloaded.Targets) != 3 {
		t.Fatalf("config not persisted: %v", err)
	}
}

func TestPreviewListsSyncedAndMutedExcluded(t *testing.T) {
	m, _ := testModel(t)
	st := m.env.Cfg.Streams[2] // Online only
	cmd := m.openPreview(st)
	m.Update(cmd())
	if m.pvw.loading {
		t.Fatal("preview should be loaded")
	}
	body, _ := m.viewPreview()
	out := plain(body)
	for _, want := range []string{"a.txt", "sub/b.txt", "sub/skip.tmp", ".DS_Store", "+ new", "· skipped", "+2 new", "2 not synced"} {
		if !strings.Contains(out, want) {
			t.Errorf("preview lacks %q:\n%s", want, out)
		}
	}
	// the muted rows really are styled differently from synced ones
	var syncedLine, skippedLine string
	for _, l := range strings.Split(body, "\n") {
		switch {
		case strings.Contains(plain(l), "sub/b.txt"):
			syncedLine = l
		case strings.Contains(plain(l), "sub/skip.tmp"):
			skippedLine = l
		}
	}
	if syncedLine == "" || skippedLine == "" {
		t.Fatalf("rows missing:\n%s", out)
	}
	m.pvw.filter = pfExcluded
	m.pvw.rebuild(false)
	body, _ = m.viewPreview()
	if o := plain(body); strings.Contains(o, "a.txt") || !strings.Contains(o, ".DS_Store") {
		t.Fatalf("'not synced' filter wrong:\n%s", o)
	}
}

func TestPreviewScrollKeepsCursorInWindow(t *testing.T) {
	m, root := testModel(t)
	src := filepath.Join(root, "src")
	for i := 0; i < 200; i++ {
		_ = os.WriteFile(filepath.Join(src, "file"+strings.Repeat("0", 3-len(itoa(i)))+itoa(i)+".dat"), []byte("x"), 0o644)
	}
	m.h = 24
	m.Update(m.openPreview(m.env.Cfg.Streams[2])())
	press := func(k string) {
		m.previewKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)})
	}
	for i := 0; i < 50; i++ {
		press("j")
	}
	v := m.pvw
	if v.cur != 50 || v.cur < v.top || v.cur >= v.top+m.previewRows() {
		t.Fatalf("cursor %d outside window top=%d rows=%d", v.cur, v.top, m.previewRows())
	}
	press("G")
	if v.cur != len(v.shown)-1 {
		t.Fatalf("G should jump to the end: %d/%d", v.cur, len(v.shown))
	}
	if h := strings.Count(m.View(), "\n") + 1; h > m.h {
		t.Fatalf("screen is %d lines, terminal has %d", h, m.h)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

func TestAddTargetToStreamPicksFromRegistry(t *testing.T) {
	m, _ := testModel(t)
	st := m.env.Cfg.Streams[2] // "Online only" uses Desk SSD; Old stick is still free
	m.startWizard(&st)
	w := m.wiz
	if w.step != wzPickTarget || len(w.tids) != 1 || w.tids[0] != "td" {
		t.Fatalf("picker should offer only registered targets the stream lacks: step=%v ids=%v", w.step, w.tids)
	}
	if view, _ := m.viewWizard(); !strings.Contains(plain(view), "Old stick") || !strings.Contains(plain(view), "not connected") {
		t.Fatalf("picker should show name and connection state:\n%s", plain(view))
	}
	m.wizardKey(tea.KeyMsg{Type: tea.KeyEnter})
	got := m.env.Cfg.Find("Online only")
	if len(got.Targets) != 2 || got.Targets[1].ID != "td" || m.screen != scDash {
		t.Fatalf("target not added to stream: %+v screen=%v", got.Targets, m.screen)
	}
	reloaded, err := config.Load(m.env.CfgPath)
	if err != nil || len(reloaded.Find("Online only").Targets) != 2 || len(reloaded.Targets) != 2 {
		t.Fatalf("not persisted as references: %v", err)
	}
}

func TestUnmountRefusesNonRemovableAndUnplugged(t *testing.T) {
	m, _ := testModel(t)
	m.screen = scTargets
	press := func() tea.Cmd { return m.targetsKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("U")}) }
	if cmd := press(); cmd != nil || !m.msgErr || !strings.Contains(m.msg, "not on a removable volume") {
		t.Fatalf("plain folder: cmd=%v msg=%q", cmd != nil, m.msg)
	}
	m.tv.sel = 1
	if cmd := press(); cmd != nil || !strings.Contains(m.msg, "already unmounted") {
		t.Fatalf("unplugged: msg=%q", m.msg)
	}
}

func TestDuplicatesMarkAndDelete(t *testing.T) {
	m, root := testModel(t)
	dir := filepath.Join(root, "dups")
	for f, c := range map[string]string{"one.bin": "xx", "two.bin": "xx", "three.bin": "xx", "solo.bin": "yy-yy"} {
		_ = os.MkdirAll(dir, 0o755)
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := m.startDupScan("test", dir)
	m.Update(cmd())
	v := m.dup
	if v.loading || len(v.groups) != 1 || len(v.rows) != 3 {
		t.Fatalf("scan: %+v", v)
	}
	key := func(s string) { m.onKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}) }
	space := func() { m.onKey(tea.KeyMsg{Type: tea.KeySpace}) }

	// the last copy of a group can never be marked
	space()
	key("j")
	space()
	key("j")
	space()
	if n, _ := v.markedTotals(); n != 2 || !m.msgErr {
		t.Fatalf("expected 2 marked and an error, got %d (%q)", n, m.msg)
	}
	if !strings.Contains(ansi.Strip(m.View()), "2 files marked") {
		t.Fatalf("view should show marks:\n%s", ansi.Strip(m.View()))
	}

	// deleting needs the typed word
	key("d")
	if m.screen != scTyped || m.typ.expect != "delete" {
		t.Fatalf("expected typed confirmation, screen=%v", m.screen)
	}
	m.typ.in.SetValue("nope")
	m.typedKey(tea.KeyMsg{Type: tea.KeyEnter})
	if m.screen != scTyped || !m.typ.wrong {
		t.Fatal("wrong word must not confirm")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Fatal("nothing may be deleted before confirmation")
	}
	m.typ.in.SetValue("delete")
	del := m.typedKey(tea.KeyMsg{Type: tea.KeyEnter})
	m.Update(del())
	entries, _ = os.ReadDir(dir)
	if len(entries) != 2 || len(m.dup.groups) != 0 {
		t.Fatalf("want 2 files left and no groups, got %d / %d", len(entries), len(m.dup.groups))
	}
}
