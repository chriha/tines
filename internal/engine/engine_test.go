package engine

import (
	"os"
	"path/filepath"

	"context"
	"github.com/chriha/tines/internal/config"
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

func TestExcludeFlagsIncludeSystemAndUser(t *testing.T) {
	got := strings.Join(excludeFlags([]string{" /Cache/** ", "", "*.tmp"}), " ")
	for _, want := range []string{"--exclude ._*", "--exclude .Spotlight-V100/**", "--exclude /Cache/**", "--exclude *.tmp"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
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

func TestMarkExcluded(t *testing.T) {
	all := []PreviewFile{{Path: ".DS_Store"}, {Path: "a.txt"}, {Path: "big.iso"}}
	sys := []PreviewFile{{Path: "a.txt"}, {Path: "big.iso"}}
	full := []PreviewFile{{Path: "a.txt"}}
	got := markExcluded(all, sys, full)
	want := []string{ExcludedSystem, "", ExcludedRule}
	for i, f := range got {
		if f.Excluded != want[i] {
			t.Errorf("%s: got %q want %q", f.Path, f.Excluded, want[i])
		}
	}
}

func TestClassifyChangesUpdateVsNew(t *testing.T) {
	got := classifyChanges(
		[]Change{{"new", "b", 2}, {"new", "a", 1}},
		[]Change{{"trash", "c", 3}},
		map[string]bool{"a": true})
	if len(got) != 3 || got[0].Op != "update" || got[1].Op != "new" || got[2].Op != "trash" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestPreviewSourceAndDryRunAgainstRealRclone(t *testing.T) {
	if _, err := Available(); err != nil {
		t.Skip("rclone not installed")
	}
	d := t.TempDir()
	src, dst := d+"/src", d+"/dst"
	for _, f := range []string{"src/a.txt", "src/sub/b.txt", "src/sub/skip.tmp", "src/.DS_Store", "dst/a.txt", "dst/gone.txt"} {
		if err := os.MkdirAll(filepath.Dir(d+"/"+f), 0o755); err != nil {
			t.Fatal(err)
		}
		content := "x"
		if f == "dst/a.txt" {
			content = "different size"
		}
		if err := os.WriteFile(d+"/"+f, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	files, err := PreviewSource(ctx, src, []string{"*.tmp"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range files {
		got[f.Path] = f.Excluded
	}
	want := map[string]string{"a.txt": "", "sub/b.txt": "", "sub/skip.tmp": ExcludedRule, ".DS_Store": ExcludedSystem}
	for p, w := range want {
		if g, ok := got[p]; !ok || g != w {
			t.Errorf("%s: got %q (present=%v), want %q", p, g, ok, w)
		}
	}
	plan, err := DryRun(ctx, src, dst, dst+"/.tines-trash/x", config.Settings{Excludes: []string{"*.tmp"}})
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]string{}
	for _, c := range plan.Files {
		ops[c.Path] = c.Op
	}
	if ops["a.txt"] != "update" || ops["sub/b.txt"] != "new" || ops["gone.txt"] != "trash" || len(ops) != 3 {
		t.Fatalf("dry-run changes wrong: %+v", ops)
	}
}
