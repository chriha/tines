package secret

import (
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	t.Setenv("TINES_CONFIG", t.TempDir()+"/config.yaml")
	t.Setenv("TINES_NO_KEYCHAIN", "1")
	mu.Lock()
	key = nil
	mu.Unlock()
	e, err := Encrypt("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(e) || strings.Contains(e, "hunter2") || strings.ContainsAny(e, "'\",:") && strings.ContainsAny(e[len(Prefix):], "'\",") {
		t.Fatalf("bad ciphertext %q", e)
	}
	if p, err := Decrypt(e); err != nil || p != "hunter2" {
		t.Fatalf("got %q, %v", p, err)
	}
	if _, err := Decrypt(e[:len(e)-3] + "AAA"); err == nil {
		t.Fatal("tampered value must fail")
	}
}
