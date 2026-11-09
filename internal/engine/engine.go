// Package engine wraps the rclone binary. All data movement is delegated to
// rclone; this package only builds safe command lines and parses output.
package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/secret"
)

// Reserved names inside a target. Everything starting with ".tines-" is
// excluded from sync, so rclone never deletes or overwrites it.
const (
	MarkerName = ".tines-target.json"
	TrashDir   = ".tines-trash"
	MetaDir    = ".tines-meta"
	reserved   = "/.tines-**"
)

var ErrNoRclone = errors.New("rclone not found in PATH (install: https://rclone.org/install/ or `brew install rclone`)")

func Bin() string {
	if b := os.Getenv("TINES_RCLONE"); b != "" {
		return b
	}
	return "rclone"
}

func Available() (string, error) {
	p, err := exec.LookPath(Bin())
	if err != nil {
		return "", ErrNoRclone
	}
	out, err := exec.Command(p, "version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

// Join appends a relative path to an rclone spec.
func Join(spec, sub string) string {
	sub = strings.TrimPrefix(sub, "/")
	if strings.HasSuffix(spec, "/") || strings.HasSuffix(spec, ":") {
		return spec + sub
	}
	return spec + "/" + sub
}

type exitErr struct {
	Code   int
	Stderr string
}

func (e *exitErr) Error() string { return fmt.Sprintf("rclone exit %d: %s", e.Code, e.Stderr) }

// Code returns the rclone exit code of err (0 if unknown).
func Code(err error) int {
	var e *exitErr
	if errors.As(err, &e) {
		return e.Code
	}
	return -1
}

// rc: 3 = directory not found, 4 = file not found.
const (
	CodeDirNotFound  = 3
	CodeFileNotFound = 4
)

func netFlags() []string {
	return []string{"--contimeout", "8s", "--timeout", "20s", "--retries", "1", "--low-level-retries", "1"}
}

var (
	encPassRe = regexp.MustCompile(`pass='(enc:v1:[A-Za-z0-9_-]+)'`)
	obscMu    sync.Mutex
	obscCache = map[string]string{}
)

// resolveArgs swaps encrypted passwords in rclone specs for the obscured form
// rclone expects. Plaintext exists only in memory and in the rclone argv.
func resolveArgs(args []string) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		var rerr error
		out[i] = encPassRe.ReplaceAllStringFunc(a, func(m string) string {
			enc := encPassRe.FindStringSubmatch(m)[1]
			obscMu.Lock()
			ob, ok := obscCache[enc]
			obscMu.Unlock()
			if !ok {
				plain, err := secret.Decrypt(enc)
				if err != nil {
					rerr = err
					return m
				}
				if ob, err = Obscure(context.Background(), plain); err != nil {
					rerr = err
					return m
				}
				obscMu.Lock()
				obscCache[enc] = ob
				obscMu.Unlock()
			}
			return "pass='" + ob + "'"
		})
		if rerr != nil {
			return nil, rerr
		}
	}
	return out, nil
}

// command builds an rclone invocation with secrets resolved.
func command(ctx context.Context, args ...string) (*exec.Cmd, error) {
	r, err := resolveArgs(args)
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, Bin(), r...), nil
}

func run(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd, err := command(ctx, args...)
	if err != nil {
		return nil, err
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), &exitErr{ee.ExitCode(), lastLines(errb.String(), 3)}
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// Ping lists the top level of spec and reports latency. A missing directory is
// reported via Code(err)==CodeDirNotFound (the remote itself is reachable).
func Ping(ctx context.Context, spec string) (time.Duration, error) {
	t := time.Now()
	_, err := run(ctx, nil, append([]string{"lsf", "--max-depth", "1", "--files-only", spec}, netFlags()...)...)
	return time.Since(t), err
}

type DirState int

const (
	DirUnreachable DirState = iota
	DirMissing
	DirEmpty
	DirNonEmpty
)

// Inspect classifies the top level of spec, ignoring tines' own files.
func Inspect(ctx context.Context, spec string) (DirState, int, error) {
	out, err := run(ctx, nil, append([]string{"lsf", "--max-depth", "1", spec}, netFlags()...)...)
	if err != nil {
		if Code(err) == CodeDirNotFound {
			return DirMissing, 0, nil
		}
		return DirUnreachable, 0, err
	}
	n := 0
	for _, l := range strings.Split(string(out), "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, ".tines-") {
			n++
		}
	}
	if n == 0 {
		return DirEmpty, 0, nil
	}
	return DirNonEmpty, n, nil
}

type SizeInfo struct{ Count, Bytes int64 }

func Size(ctx context.Context, spec string, excludes []string) (SizeInfo, error) {
	args := []string{"size", "--json", "--exclude", reserved}
	args = append(args, excludeFlags(excludes)...)
	args = append(args, spec)
	out, err := run(ctx, nil, append(args, netFlags()...)...)
	if err != nil {
		return SizeInfo{}, err
	}
	var v struct{ Count, Bytes int64 }
	if err := json.Unmarshal(out, &v); err != nil {
		return SizeInfo{}, err
	}
	return SizeInfo{v.Count, v.Bytes}, nil
}

// Capacity is the size and free space of the volume or remote behind a spec.
type Capacity struct{ Total, Free int64 }

// About reports capacity of the filesystem/remote behind spec. ok is false
// when the backend cannot say (not every remote supports `about`) or does
// not know its total size (e.g. unlimited cloud quota).
func About(ctx context.Context, spec string) (c Capacity, ok bool) {
	try := func(sp string) (Capacity, bool) {
		out, err := run(ctx, nil, append([]string{"about", "--json", sp}, netFlags()...)...)
		if err != nil {
			return Capacity{}, false
		}
		var v struct{ Total, Free *int64 }
		if json.Unmarshal(out, &v) != nil || v.Free == nil {
			return Capacity{}, false
		}
		c := Capacity{Free: *v.Free}
		if v.Total != nil {
			c.Total = *v.Total
		}
		return c, true
	}
	if c, ok := try(spec); ok {
		return c, true
	}
	// A not-yet-created local directory: ask the nearest existing parent.
	if strings.HasPrefix(spec, "/") {
		for d := filepath.Dir(spec); d != "/" && d != "."; d = filepath.Dir(d) {
			if _, err := os.Stat(d); err == nil {
				return try(d)
			}
		}
	}
	return Capacity{}, false
}

// Free reports the free bytes on the filesystem/remote behind spec.
func Free(ctx context.Context, spec string) (free int64, ok bool) {
	c, ok := About(ctx, spec)
	return c.Free, ok
}

// SystemExcludes are OS-generated files that are never worth backing up and
// that appear on the target on their own (Finder, Spotlight, "._" AppleDouble
// sidecars on exFAT/SMB/NFS, Windows leftovers). They are applied to every
// stream, so a verify never fails because the OS touched a volume.
var SystemExcludes = []string{
	"._*",
	".DS_Store",
	".AppleDouble/**",
	".Spotlight-V100/**",
	".Trashes/**",
	".fseventsd/**",
	".TemporaryItems/**",
	".DocumentRevisions-V100/**",
	".VolumeIcon.icns",
	".apdisk",
	".localized",
	".com.apple.timemachine.donotpresent",
	"Thumbs.db",
	"desktop.ini",
	"$RECYCLE.BIN/**",
	"System Volume Information/**",
}

// excludeFlags returns the rclone flags for the built-in system excludes plus
// the stream's own patterns. Patterns follow rclone filter syntax: a pattern
// with a leading "/" is anchored to the source root ("/Cache/**" or
// "/Projects/big.iso"), one without matches at any depth ("node_modules/**",
// "*.tmp").
func excludeFlags(ex []string) []string {
	var a []string
	for _, e := range SystemExcludes {
		a = append(a, "--exclude", e)
	}
	for _, e := range ex {
		if e = strings.TrimSpace(e); e != "" {
			a = append(a, "--exclude", e)
		}
	}
	return a
}

// ReadFile / WriteFile move small files (markers, config mirrors).
func ReadFile(ctx context.Context, spec string) ([]byte, error) {
	return run(ctx, nil, append([]string{"cat", spec}, netFlags()...)...)
}

func WriteFile(ctx context.Context, spec string, data []byte) error {
	_, err := run(ctx, data, append([]string{"rcat", spec}, netFlags()...)...)
	return err
}

func Obscure(ctx context.Context, pw string) (string, error) {
	out, err := run(ctx, []byte(pw), "obscure", "-")
	return strings.TrimSpace(string(out)), err
}

func ListRemotes(ctx context.Context) []string {
	out, err := run(ctx, nil, "listremotes")
	if err != nil {
		return nil
	}
	var r []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			r = append(r, l)
		}
	}
	return r
}

func Purge(ctx context.Context, spec string) error {
	_, err := run(ctx, nil, append([]string{"purge", spec}, netFlags()...)...)
	return err
}

func ListDirs(ctx context.Context, spec string) ([]string, error) {
	out, err := run(ctx, nil, append([]string{"lsf", "--dirs-only", spec}, netFlags()...)...)
	if err != nil {
		return nil, err
	}
	var d []string
	for _, l := range strings.Split(string(out), "\n") {
		if l = strings.Trim(strings.TrimSpace(l), "/"); l != "" {
			d = append(d, l)
		}
	}
	return d, nil
}

// ---- sync / plan -----------------------------------------------------

func syncArgs(verb, src, dst, trash string, s config.Settings) []string {
	a := []string{verb, src, dst,
		"--exclude", reserved,
		"--use-json-log", "--log-level", "INFO",
	}
	if verb == "sync" {
		// Deleted and overwritten files are MOVED here, never destroyed.
		a = append(a, "--backup-dir", trash)
	}
	a = append(a, excludeFlags(s.Excludes)...)
	if s.Checksum {
		a = append(a, "--checksum")
	}
	if s.BandwidthLimit != "" {
		a = append(a, "--bwlimit", s.BandwidthLimit)
	}
	if s.Transfers > 0 {
		a = append(a, "--transfers", fmt.Sprint(s.Transfers))
	}
	return a
}

type Change struct {
	Op   string // copy, trash (Samples); new, update, trash (Files)
	Path string
	Size int64
}

type Plan struct {
	Copies  int // files copied (new + updated)
	Updated int // of which overwrite an existing file
	Trashed int // files that exist only on the target and get moved to trash
	Bytes   int64
	Samples []Change // first 500 changes, for a quick look
	Files   []Change // every change, sorted by path (Op: new, update, trash)
}

func (p Plan) New() int        { return p.Copies - p.Updated }
func (p Plan) Changes() int    { return p.Copies + p.Trashed }
func (p Plan) InSync() bool    { return p.Copies == 0 && p.Trashed == 0 }
func (p Plan) Overwrites() int { return p.Updated }

type logLine struct {
	Level   string    `json:"level"`
	Msg     string    `json:"msg"`
	Object  string    `json:"object"`
	Size    int64     `json:"size"`
	Skipped string    `json:"skipped"`
	Stats   *rawStats `json:"stats"`
}

type rawStats struct {
	Bytes          int64    `json:"bytes"`
	TotalBytes     int64    `json:"totalBytes"`
	Speed          float64  `json:"speed"`
	ETA            *float64 `json:"eta"`
	Transfers      int64    `json:"transfers"`
	TotalTransfers int64    `json:"totalTransfers"`
	Errors         int64    `json:"errors"`
	Transferring   []struct {
		Name string `json:"name"`
	} `json:"transferring"`
}

// DryRun computes what a sync would do without changing anything.
func DryRun(ctx context.Context, src, dst, trash string, s config.Settings) (Plan, error) {
	args := append(syncArgs("sync", src, dst, trash, s), "--dry-run")
	args = append(args, netFlags()...)
	cmd, err := command(ctx, args...)
	if err != nil {
		return Plan{}, err
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err = cmd.Run()
	var p Plan
	var copies, trashes []Change
	overwritten := map[string]bool{}
	for _, raw := range strings.Split(errb.String(), "\n") {
		var l logLine
		if json.Unmarshal([]byte(raw), &l) != nil || l.Skipped == "" {
			continue
		}
		switch l.Skipped {
		case "copy":
			p.Copies++
			p.Bytes += l.Size
			addSample(&p, Change{"copy", l.Object, l.Size})
			copies = append(copies, Change{"new", l.Object, l.Size})
		case "move": // overwritten old version goes to trash
			p.Updated++
			overwritten[l.Object] = true
		case "move into backup dir", "delete":
			p.Trashed++
			addSample(&p, Change{"trash", l.Object, l.Size})
			trashes = append(trashes, Change{"trash", l.Object, l.Size})
		}
	}
	sort.Slice(p.Samples, func(i, j int) bool { return p.Samples[i].Path < p.Samples[j].Path })
	p.Files = classifyChanges(copies, trashes, overwritten)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return p, &exitErr{ee.ExitCode(), errTail(errb.String())}
		}
		return p, err
	}
	return p, nil
}

// classifyChanges turns raw dry-run lines into one sorted list. A copy whose
// path also shows up as an overwritten file is an update, not a new file.
func classifyChanges(copies, trashes []Change, overwritten map[string]bool) []Change {
	out := make([]Change, 0, len(copies)+len(trashes))
	for _, c := range copies {
		if overwritten[c.Path] {
			c.Op = "update"
		}
		out = append(out, c)
	}
	out = append(out, trashes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func addSample(p *Plan, c Change) {
	if len(p.Samples) < 500 {
		p.Samples = append(p.Samples, c)
	}
}

// errTail extracts the last error-level messages from rclone's JSON log.
func errTail(s string) string {
	var msgs []string
	for _, raw := range strings.Split(s, "\n") {
		var l logLine
		if json.Unmarshal([]byte(raw), &l) == nil && (l.Level == "error" || l.Level == "critical") {
			msgs = append(msgs, l.Msg)
		}
	}
	if len(msgs) == 0 {
		return lastLines(s, 2)
	}
	if len(msgs) > 3 {
		msgs = msgs[len(msgs)-3:]
	}
	return strings.Join(msgs, " | ")
}

type Progress struct {
	Bytes, TotalBytes         int64
	Speed                     float64
	ETA                       time.Duration // 0 = unknown
	Transfers, TotalTransfers int64
	Errors                    int64
	Current                   string
}

// Sync performs the mirror. Overwritten/deleted target files are moved into trash.
func Sync(ctx context.Context, src, dst, trash string, s config.Settings, on func(Progress)) error {
	args := append(syncArgs("sync", src, dst, trash, s), "--stats", "500ms")
	args = append(args, netFlags()...)
	cmd, err := command(ctx, args...)
	if err != nil {
		return err
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 15 * time.Second
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var errs []string
	sc := bufio.NewScanner(stderr)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var l logLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if l.Stats != nil && on != nil {
			st := l.Stats
			p := Progress{st.Bytes, st.TotalBytes, st.Speed, 0, st.Transfers, st.TotalTransfers, st.Errors, ""}
			if st.ETA != nil {
				p.ETA = time.Duration(*st.ETA * float64(time.Second))
			}
			if len(st.Transferring) > 0 {
				p.Current = st.Transferring[0].Name
			}
			on(p)
		} else if l.Level == "error" || l.Level == "critical" {
			errs = append(errs, l.Msg)
		}
	}
	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if len(errs) > 3 {
				errs = errs[len(errs)-3:]
			}
			return &exitErr{ee.ExitCode(), strings.Join(errs, " | ")}
		}
		return err
	}
	return nil
}

type VerifyResult struct {
	Same, Differ, MissingOnTarget, ExtraOnTarget, Errors int
	Samples                                              []string
}

func (v VerifyResult) OK() bool {
	return v.Differ+v.MissingOnTarget+v.ExtraOnTarget+v.Errors == 0
}

// Verify compares source and target content without changing anything.
func Verify(ctx context.Context, src, dst string, s config.Settings) (VerifyResult, error) {
	args := []string{"check", src, dst, "--combined", "-", "--exclude", reserved}
	args = append(args, excludeFlags(s.Excludes)...)
	args = append(args, netFlags()...)
	cmd, err := command(ctx, args...)
	if err != nil {
		return VerifyResult{}, err
	}
	stdout, _ := cmd.StdoutPipe()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		return VerifyResult{}, err
	}
	var v VerifyResult
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		l := sc.Text()
		if len(l) < 3 {
			continue
		}
		note := func() {
			if len(v.Samples) < 20 {
				v.Samples = append(v.Samples, l)
			}
		}
		switch l[0] {
		case '=':
			v.Same++
		case '*':
			v.Differ++
			note()
		case '+':
			v.MissingOnTarget++
			note()
		case '-':
			v.ExtraOnTarget++
			note()
		case '!':
			v.Errors++
			note()
		}
	}
	err = cmd.Wait()
	if err != nil && ctx.Err() != nil {
		return v, ctx.Err()
	}
	// exit 1 just means differences were found; that is a valid result.
	var ee *exec.ExitError
	if err != nil && errors.As(err, &ee) && ee.ExitCode() != 1 {
		return v, &exitErr{ee.ExitCode(), errTail(errb.String())}
	}
	return v, nil
}

// ---- source preview ----------------------------------------------------

// Why a source file is left out of the backup.
const (
	ExcludedSystem = "system" // OS-generated file or tines' own reserved name
	ExcludedRule   = "rule"   // matched one of the stream's exclude patterns
)

// PreviewFile is one file of the source and whether it takes part in the sync.
type PreviewFile struct {
	Path     string
	Size     int64
	Excluded string // "", ExcludedSystem or ExcludedRule
}

const lsSep = "\x1f"

// ListFiles lists every file below spec (recursive) after applying exclude
// patterns, sorted by path.
func ListFiles(ctx context.Context, spec string, excludes []string) ([]PreviewFile, error) {
	args := []string{"lsf", "-R", "--files-only", "--format", "sp", "--separator", lsSep}
	for _, e := range excludes {
		args = append(args, "--exclude", e)
	}
	args = append(args, spec)
	out, err := run(ctx, nil, append(args, netFlags()...)...)
	if err != nil {
		return nil, err
	}
	return parseListing(string(out)), nil
}

func parseListing(out string) []PreviewFile {
	var files []PreviewFile
	for _, l := range strings.Split(out, "\n") {
		size, path, ok := strings.Cut(l, lsSep)
		if !ok || path == "" {
			continue
		}
		var n int64
		fmt.Sscanf(size, "%d", &n)
		files = append(files, PreviewFile{Path: path, Size: n})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files
}

// PreviewSource lists all source files and marks the ones the sync leaves out.
// It never touches a target.
func PreviewSource(ctx context.Context, spec string, userExcludes []string) ([]PreviewFile, error) {
	sys := append([]string{reserved}, SystemExcludes...)
	full := append([]string(nil), sys...)
	for _, e := range userExcludes {
		if e = strings.TrimSpace(e); e != "" {
			full = append(full, e)
		}
	}
	lists := make([][]PreviewFile, 3)
	errs := make([]error, 3)
	var wg sync.WaitGroup
	for i, ex := range [][]string{nil, sys, full} {
		if i == 2 && len(full) == len(sys) {
			continue // no user rules: same as the system pass
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i], errs[i] = ListFiles(ctx, spec, ex)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil && !(i == 2 && len(full) == len(sys)) {
			return nil, err
		}
	}
	if len(full) == len(sys) {
		lists[2] = lists[1]
	}
	return markExcluded(lists[0], lists[1], lists[2]), nil
}

// markExcluded labels each file of all: kept by the full filter set → "",
// otherwise "system" if the system filters already drop it, else "rule".
func markExcluded(all, sysKept, fullKept []PreviewFile) []PreviewFile {
	sk := make(map[string]bool, len(sysKept))
	for _, f := range sysKept {
		sk[f.Path] = true
	}
	fk := make(map[string]bool, len(fullKept))
	for _, f := range fullKept {
		fk[f.Path] = true
	}
	for i := range all {
		switch {
		case fk[all[i].Path]:
		case !sk[all[i].Path]:
			all[i].Excluded = ExcludedSystem
		default:
			all[i].Excluded = ExcludedRule
		}
	}
	return all
}
