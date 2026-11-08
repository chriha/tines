package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/engine"
	"github.com/chriha/tines/internal/state"
)

func setup(t *testing.T) (*Env, config.Stream, string, string) {
	t.Helper()
	if _, err := engine.Available(); err != nil {
		t.Skip("rclone not installed")
	}
	t.Setenv("TINES_STATE_DIR", t.TempDir())
	d := t.TempDir()
	src, dst := filepath.Join(d, "src"), filepath.Join(d, "dst")
	_ = os.MkdirAll(src, 0o755)
	_ = os.MkdirAll(dst, 0o755)
	st := config.Stream{ID: "s1", Name: "t", Source: config.Endpoint{Kind: "local", Spec: src},
		Targets:  []config.Target{{ID: "t1", Name: "one", Endpoint: config.Endpoint{Kind: "local", Spec: dst}}},
		Settings: config.DefaultSettings()}
	cfg := config.New()
	cfg.Streams = []config.Stream{st}
	ss, _ := state.Open()
	return &Env{Cfg: cfg, State: ss}, st, src, dst
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestForeignTargetNeedsAdoptAndTrashKeepsFiles(t *testing.T) {
	env, st, src, dst := setup(t)
	ctx := context.Background()
	write(t, filepath.Join(src, "a.txt"), "A")
	write(t, filepath.Join(dst, "precious.txt"), "keep me")

	p := env.Prepare(ctx, st, st.Targets[0], env.CheckSource(ctx, st))
	if !p.Runnable() || len(p.Needs) == 0 || p.Needs[0].Kind != NeedAdopt {
		t.Fatalf("foreign dir must require adopt confirmation: %+v", p)
	}
	if res := env.Execute(ctx, p, nil); !res.OK {
		t.Fatal(res.Message)
	}
	// the foreign file must be in trash, not gone
	matches, _ := filepath.Glob(filepath.Join(dst, engine.TrashDir, "*", "precious.txt"))
	if len(matches) != 1 {
		t.Fatalf("precious.txt not preserved in trash: %v", matches)
	}
}

func TestMarkerBelongingToOtherStreamBlocks(t *testing.T) {
	env, st, src, dst := setup(t)
	ctx := context.Background()
	write(t, filepath.Join(src, "a.txt"), "A")
	write(t, filepath.Join(dst, engine.MarkerName), `{"tool":"tines","version":1,"stream_id":"OTHER","stream_name":"other","target_id":"x"}`)
	p := env.Prepare(ctx, st, st.Targets[0], env.CheckSource(ctx, st))
	if p.Runnable() {
		t.Fatal("target owned by another stream must be blocked")
	}
}

func TestEmptySourceAndMassDeleteNeedConfirmation(t *testing.T) {
	env, st, src, dst := setup(t)
	ctx := context.Background()
	for i := 0; i < 30; i++ {
		write(t, filepath.Join(src, string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"), "x")
	}
	p := env.Prepare(ctx, st, st.Targets[0], env.CheckSource(ctx, st))
	for _, n := range p.Needs {
		_ = n
	}
	if res := env.Execute(ctx, p, nil); !res.OK {
		t.Fatal(res.Message)
	}
	// wipe the source: everything on the target would be trashed
	_ = os.RemoveAll(src)
	_ = os.MkdirAll(src, 0o755)
	p = env.Prepare(ctx, st, st.Targets[0], env.CheckSource(ctx, st))
	kinds := map[NeedKind]bool{}
	for _, n := range p.Needs {
		kinds[n.Kind] = true
	}
	if !kinds[NeedEmptySource] || !kinds[NeedMassDelete] {
		t.Fatalf("expected empty-source and mass-delete confirmations, got %+v", p.Needs)
	}
	_ = dst
}

func TestOverlapBlocked(t *testing.T) {
	env, st, src, _ := setup(t)
	write(t, filepath.Join(src, "a"), "1")
	st.Targets[0].Spec = filepath.Join(src, "inside")
	p := env.Prepare(context.Background(), st, st.Targets[0], env.CheckSource(context.Background(), st))
	if p.Runnable() {
		t.Fatal("nested target must be blocked")
	}
}

func TestNotEnoughSpaceBlocks(t *testing.T) {
	env, st, src, _ := setup(t)
	write(t, filepath.Join(src, "a.txt"), "hello")
	p := &Prepared{Stream: st, Target: st.Targets[0], Plan: engine.Plan{Copies: 1, Bytes: 1 << 60}}
	env.checkSpace(context.Background(), st.Targets[0], p)
	if p.Runnable() {
		t.Fatal("a plan larger than the free space must be blocked")
	}
	p = &Prepared{Stream: st, Target: st.Targets[0], Plan: engine.Plan{Copies: 1, Bytes: 5}}
	env.checkSpace(context.Background(), st.Targets[0], p)
	if !p.Runnable() {
		t.Fatalf("small plan must fit: %v", p.Blocks)
	}
}
