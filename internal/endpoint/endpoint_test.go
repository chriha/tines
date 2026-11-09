package endpoint

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverlap(t *testing.T) {
	d := t.TempDir()
	a, b := filepath.Join(d, "a"), filepath.Join(d, "a", "sub")
	_ = os.MkdirAll(b, 0o755)
	if !Overlap(a, b) || !Overlap(b, a) || !Overlap(a, a) {
		t.Fatal("nested/same paths must overlap")
	}
	if Overlap(filepath.Join(d, "a"), filepath.Join(d, "ab")) {
		t.Fatal("siblings with common prefix must not overlap")
	}
	link := filepath.Join(d, "link")
	_ = os.Symlink(a, link)
	if !Overlap(link, b) {
		t.Fatal("symlink to parent must overlap")
	}
}

func TestUnmountedVolume(t *testing.T) {
	if _, bad := UnmountedVolume("/Volumes/definitely-not-mounted-xyz/photos"); !bad {
		t.Fatal("expected unmounted volume to be detected")
	}
	if _, bad := UnmountedVolume(t.TempDir()); bad {
		t.Fatal("temp dir is not a removable volume")
	}
}

func TestBuildAndDisplay(t *testing.T) {
	ep, err := Build(context.Background(), "sftp", map[string]string{"host": "h.example", "user": "me", "port": "2222", "path": "srv/x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ep.Spec, "host='h.example'") || !strings.HasSuffix(ep.Spec, ":/srv/x") {
		t.Fatalf("bad spec %q", ep.Spec)
	}
	if got := Display(ep); got != "sftp://me@h.example/srv/x" {
		t.Fatalf("display = %q", got)
	}
	if _, err := Build(context.Background(), "local", map[string]string{"path": ""}); err == nil {
		t.Fatal("empty path must fail")
	}
}

func TestVolumeRootOnlyForMountedRemovableVolumes(t *testing.T) {
	if _, ok := VolumeRoot(t.TempDir()); ok {
		t.Fatal("a plain folder is not a removable volume")
	}
	if _, ok := VolumeRoot("/Volumes/tines-test-unplugged/x"); ok {
		t.Fatal("an unmounted volume has no root to unmount")
	}
	if _, ok := VolumeRoot(":sftp,host='h':/x"); ok {
		t.Fatal("remotes are not volumes")
	}
}
