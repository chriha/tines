package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFindAndDeleteDuplicates(t *testing.T) {
	if _, err := Available(); err != nil {
		t.Skip("rclone not installed")
	}
	root := t.TempDir()
	write := func(rel, content string, age time.Duration) {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		ts := time.Now().Add(-age)
		_ = os.Chtimes(p, ts, ts)
	}
	write("a/orig.jpg", "same-bytes", 48*time.Hour)
	write("b/copy.jpg", "same-bytes", 24*time.Hour)
	write("c/copy2.jpg", "same-bytes", time.Hour)
	write("d/other.jpg", "diff-bytes", time.Hour) // same size, different content
	write("e/unique.txt", "only one of these", time.Hour)
	write("f/empty1", "", time.Hour) // empty files are ignored
	write("f/empty2", "", time.Hour)
	write(".DS_Store", "same-bytes", time.Hour) // system files are ignored
	write(".tines-trash/x", "same-bytes", time.Hour)

	ctx := context.Background()
	groups, skipped, err := FindDuplicates(ctx, root)
	if err != nil || skipped != 0 {
		t.Fatalf("err=%v skipped=%d", err, skipped)
	}
	if len(groups) != 1 || len(groups[0].Files) != 3 {
		t.Fatalf("want one group of 3, got %+v", groups)
	}
	g := groups[0]
	if g.Files[0].Path != "a/orig.jpg" || g.Size != 10 || g.Reclaimable() != 20 {
		t.Fatalf("oldest must come first: %+v", g)
	}
	if err := DeleteFile(ctx, root, "c/copy2.jpg"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "c/copy2.jpg")); !os.IsNotExist(err) {
		t.Fatal("file should be gone")
	}
	groups, _, _ = FindDuplicates(ctx, root)
	if len(groups) != 1 || len(groups[0].Files) != 2 {
		t.Fatalf("after delete want 2 copies, got %+v", groups)
	}
}
