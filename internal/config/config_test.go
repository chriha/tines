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
