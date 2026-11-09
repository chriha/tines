package config

import (
	"path/filepath"
	"testing"
)

func TestRoundTripAndValidate(t *testing.T) {
	c := New()
	c.Streams = append(c.Streams, Stream{ID: "a", Name: "Photos",
		Source:   Endpoint{Kind: "local", Spec: "/a"},
		Targets:  []Target{{ID: "t", Name: "ssd", Endpoint: Endpoint{Kind: "local", Spec: "/b"}}},
		Settings: DefaultSettings()})
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Find("photos") == nil || got.Streams[0].Targets[0].Spec != "/b" {
		t.Fatalf("round trip failed: %+v", got)
	}
	c.Streams = append(c.Streams, c.Streams[0])
	if c.Validate() == nil {
		t.Fatal("expected duplicate error")
	}
}

func TestLegacyInlineTargetsMigrate(t *testing.T) {
	legacy := `version: 1
streams:
  - id: a
    name: Photos
    source: {kind: local, spec: /src}
    targets:
      - {id: t1, name: ssd, kind: local, spec: /b}
    settings: {}
  - id: b
    name: Docs
    source: {kind: local, spec: /docs}
    targets:
      - {id: t1, name: ssd, kind: local, spec: /b}
      - {id: t2, name: ssd, kind: smb, spec: ":smb,host='h':x"}
    settings: {}
`
	c, err := Parse([]byte(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Targets) != 2 {
		t.Fatalf("shared target must register once, got %+v", c.Targets)
	}
	if c.Targets[1].Name != "ssd (2)" || c.Targets[1].Type != TypeNAS {
		t.Fatalf("clash not resolved / type not guessed: %+v", c.Targets[1])
	}
	if got := c.Find("Docs").Targets[1].Name; got != "ssd (2)" {
		t.Fatalf("stream copy not resolved: %q", got)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	again, err := Load(p)
	if err != nil || len(again.Targets) != 2 || len(again.Find("Docs").Targets) != 2 {
		t.Fatalf("reload after migration failed: %v %+v", err, again)
	}
}

func TestRemoveTargetRefusesWhileUsed(t *testing.T) {
	c := New()
	tg := Target{ID: "t", Name: "ssd", Endpoint: Endpoint{Kind: "local", Spec: "/b"}}
	c.Streams = []Stream{{ID: "a", Name: "P", Source: Endpoint{Spec: "/a"}, Targets: []Target{tg}}}
	c.Normalize()
	if c.RemoveTarget("t") == nil {
		t.Fatal("must refuse to remove a target that a stream uses")
	}
	c.Streams[0].Targets = nil
	if err := c.RemoveTarget("t"); err != nil || len(c.Targets) != 0 {
		t.Fatalf("unused target should be removable: %v", err)
	}
}
