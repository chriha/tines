package engine

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DupeFile is one file that has at least one byte-identical twin.
type DupeFile struct {
	Path    string // relative to the scanned root
	Size    int64
	ModTime time.Time
}

// DupeGroup is a set of files with identical content (same size and MD5).
// Files are ordered oldest first, so Files[0] is the likely original.
type DupeGroup struct {
	Hash  string
	Size  int64
	Files []DupeFile
}

// Reclaimable is the space freed by keeping one file of the group.
func (g DupeGroup) Reclaimable() int64 { return g.Size * int64(len(g.Files)-1) }

var md5Re = regexp.MustCompile(`^[0-9a-f]{32}$`)

const lsTimeLayout = "2006-01-02 15:04:05"

// listForDupes lists every file below spec with size and modification time.
// OS-generated files and tines' reserved folders are left out.
func listForDupes(ctx context.Context, spec string) ([]DupeFile, error) {
	args := []string{"lsf", "-R", "--files-only", "--format", "stp", "--separator", lsSep}
	for _, e := range append([]string{reserved}, SystemExcludes...) {
		args = append(args, "--exclude", e)
	}
	args = append(args, spec)
	out, err := run(ctx, nil, append(args, netFlags()...)...)
	if err != nil {
		return nil, err
	}
	var files []DupeFile
	for _, l := range strings.Split(string(out), "\n") {
		parts := strings.SplitN(l, lsSep, 3)
		if len(parts) != 3 || parts[2] == "" {
			continue
		}
		f := DupeFile{Path: parts[2]}
		var n int64
		if _, err := fmt.Sscanf(parts[0], "%d", &n); err == nil {
			f.Size = n
		}
		if t, err := time.ParseInLocation(lsTimeLayout, parts[1], time.Local); err == nil {
			f.ModTime = t
		}
		files = append(files, f)
	}
	return files, nil
}

// hashFiles returns the MD5 of each path. Backends that store MD5 themselves
// answer instantly; for the others the files are downloaded and hashed.
func hashFiles(ctx context.Context, spec string, paths []string) (map[string]string, error) {
	list, err := os.CreateTemp("", "tines-dupes-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(list.Name())
	list.WriteString(strings.Join(paths, "\n") + "\n")
	list.Close()

	sums := make(map[string]string, len(paths))
	pass := func(download bool) (missing []string, err error) {
		args := []string{"hashsum", "md5", spec, "--files-from-raw", list.Name()}
		if download {
			args = append(args, "--download")
		}
		out, err := run(ctx, nil, append(args, netFlags()...)...)
		if err != nil && out == nil {
			return nil, err
		}
		for _, l := range strings.Split(string(out), "\n") {
			sum, path, ok := strings.Cut(l, "  ")
			if !ok {
				continue
			}
			if md5Re.MatchString(sum) {
				sums[path] = sum
			}
		}
		for _, p := range paths {
			if _, ok := sums[p]; !ok {
				missing = append(missing, p)
			}
		}
		return missing, nil
	}
	missing, err := pass(false)
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if len(missing) > 0 && ctx.Err() == nil { // no native MD5: read the files
		if _, err = pass(true); err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return sums, ctx.Err()
}

// FindDuplicates scans spec for files with identical content. Only files that
// share a size are hashed, so most files are never read. Zero-byte files are
// ignored. skipped counts files that could not be hashed.
func FindDuplicates(ctx context.Context, spec string) (groups []DupeGroup, skipped int, err error) {
	files, err := listForDupes(ctx, spec)
	if err != nil {
		return nil, 0, err
	}
	bySize := map[int64][]DupeFile{}
	for _, f := range files {
		if f.Size > 0 {
			bySize[f.Size] = append(bySize[f.Size], f)
		}
	}
	var cand []string
	for _, fs := range bySize {
		if len(fs) > 1 {
			for _, f := range fs {
				cand = append(cand, f.Path)
			}
		}
	}
	if len(cand) == 0 {
		return nil, 0, nil
	}
	sort.Strings(cand)
	sums, err := hashFiles(ctx, spec, cand)
	if err != nil {
		return nil, 0, err
	}
	byKey := map[string]*DupeGroup{}
	for size, fs := range bySize {
		if len(fs) < 2 {
			continue
		}
		for _, f := range fs {
			h, ok := sums[f.Path]
			if !ok {
				skipped++
				continue
			}
			k := fmt.Sprintf("%d:%s", size, h)
			g := byKey[k]
			if g == nil {
				g = &DupeGroup{Hash: h, Size: size}
				byKey[k] = g
			}
			g.Files = append(g.Files, f)
		}
	}
	for _, g := range byKey {
		if len(g.Files) < 2 {
			continue
		}
		sort.Slice(g.Files, func(i, j int) bool {
			a, b := g.Files[i], g.Files[j]
			if !a.ModTime.Equal(b.ModTime) {
				return a.ModTime.Before(b.ModTime)
			}
			return a.Path < b.Path
		})
		groups = append(groups, *g)
	}
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i], groups[j]
		if a.Reclaimable() != b.Reclaimable() {
			return a.Reclaimable() > b.Reclaimable()
		}
		return a.Files[0].Path < b.Files[0].Path
	})
	return groups, skipped, nil
}

// DeleteFile permanently removes one file below spec. It is not moved to the
// trash: callers must have asked the user first.
func DeleteFile(ctx context.Context, spec, path string) error {
	_, err := run(ctx, nil, append([]string{"deletefile", Join(spec, path)}, netFlags()...)...)
	return err
}
