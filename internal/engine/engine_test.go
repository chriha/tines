package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/chriha/tines/internal/secret"
)

func TestResolveArgsDecryptsPassword(t *testing.T) {
	if _, err := Available(); err != nil {
		t.Skip("rclone not installed")
	}
	t.Setenv("TINES_CONFIG", t.TempDir()+"/config.yaml")
	t.Setenv("TINES_NO_KEYCHAIN", "1")
	enc, err := secret.Encrypt("s3cret")
	if err != nil {
		t.Fatal(err)
	}
	spec := ":smb,host='h',user='u',pass='" + enc + "':share"
	out, err := resolveArgs([]string{"lsf", spec})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out[1], "enc:v1:") || strings.Contains(out[1], "s3cret") {
		t.Fatalf("password not converted to obscured form: %q", out[1])
	}
	if !strings.Contains(spec, enc) {
		t.Fatal("input must not be mutated")
	}
}

func TestFreeLocal(t *testing.T) {
	if _, err := Available(); err != nil {
		t.Skip("rclone not installed")
	}
	d := t.TempDir()
	if f, ok := Free(context.Background(), d+"/not/created/yet"); !ok || f <= 0 {
		t.Fatalf("free = %d, %v", f, ok)
	}
}
