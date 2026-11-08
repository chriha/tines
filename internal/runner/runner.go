// Package runner orchestrates safe backups: preflight checks, a dry-run plan,
// explicit confirmations, the sync itself, verification and bookkeeping.
// It is UI-agnostic; the CLI and the TUI both drive it.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/chrishartmann/tines/internal/config"
	"github.com/chrishartmann/tines/internal/endpoint"
	"github.com/chrishartmann/tines/internal/engine"
	"github.com/chrishartmann/tines/internal/state"
)

type NeedKind string

const (
	NeedInit        NeedKind = "init"   // target dir missing/empty, will be claimed by this stream
	NeedAdopt       NeedKind = "adopt"  // target dir has foreign content
	NeedMassDelete  NeedKind = "delete" // plan trashes a suspicious share of the target
	NeedEmptySource NeedKind = "empty"  // source has no files
)

// Need is an explicit, typed confirmation the user must give before a target may be written.
type Need struct {
	Kind   NeedKind
	Prompt string
	Expect string // text the user must type
}

type Marker struct {
	Tool       string    `json:"tool"`
	Version    int       `json:"version"`
	StreamID   string    `json:"stream_id"`
	StreamName string    `json:"stream_name"`
	TargetID   string    `json:"target_id"`
	Created    time.Time `json:"created"`
}

type SourceInfo struct {
	OK    bool
	Err   string
	Files int64
	Bytes int64
}

type Prepared struct {
	Stream      config.Stream
	Target      config.Target
	Plan        engine.Plan
	DestSize    engine.SizeInfo
	Free        int64 // free bytes on the target, valid if FreeKnown
	FreeKnown   bool
	Blocks      []string // hard stops, cannot be overridden
	Warnings    []string
	Needs       []Need
	RunID       string
	writeMarker bool
}

func (p *Prepared) Runnable() bool { return len(p.Blocks) == 0 }

type Env struct {
	Cfg     *config.Config
	CfgPath string
	State   *state.Store
}

func NewRunID() string { return time.Now().UTC().Format("20060102-150405") }

// CheckSource validates the source once per stream.
func (e *Env) CheckSource(ctx context.Context, st config.Stream) SourceInfo {
	if dir, bad := endpoint.UnmountedVolume(st.Source.Spec); bad {
		return SourceInfo{Err: fmt.Sprintf("source volume %s is not mounted", dir)}
	}
	ds, _, err := engine.Inspect(ctx, st.Source.Spec)
	switch {
	case err != nil:
		return SourceInfo{Err: "source unreachable: " + err.Error()}
	case ds == engine.DirMissing:
		return SourceInfo{Err: "source directory does not exist"}
	}
	sz, err := engine.Size(ctx, st.Source.Spec, st.Settings.Excludes)
	if err != nil {
		return SourceInfo{Err: "cannot read source: " + err.Error()}
	}
	return SourceInfo{OK: true, Files: sz.Count, Bytes: sz.Bytes}
}

// Classify inspects a target and returns its marker status without planning.
func (e *Env) classify(ctx context.Context, st config.Stream, t config.Target, p *Prepared) (reachable bool) {
	if dir, bad := endpoint.UnmountedVolume(t.Spec); bad {
		p.Blocks = append(p.Blocks, fmt.Sprintf("volume %s is not mounted (is the drive plugged in?)", dir))
		return false
	}
	ds, n, err := engine.Inspect(ctx, t.Spec)
	if err != nil {
		p.Blocks = append(p.Blocks, "target unreachable: "+err.Error())
		return false
	}
	var m *Marker
	if ds != engine.DirMissing {
		if b, err := engine.ReadFile(ctx, engine.Join(t.Spec, engine.MarkerName)); err == nil {
			m = &Marker{}
			if json.Unmarshal(b, m) != nil || m.Tool != "tines" {
				m = nil
				p.Blocks = append(p.Blocks, "target has an unreadable "+engine.MarkerName+"; refusing to touch it")
				return true
			}
		}
	}
	switch {
	case m != nil && m.StreamID == st.ID && m.TargetID == t.ID:
		// ours
	case m != nil:
		p.Blocks = append(p.Blocks, fmt.Sprintf("target belongs to another backup (stream %q, target id %s). Refusing to write.", m.StreamName, m.TargetID))
	case ds == engine.DirMissing:
		p.writeMarker = true
		p.Needs = append(p.Needs, Need{NeedInit, fmt.Sprintf("Target directory does not exist and will be CREATED at %s", endpoint.Display(t.Endpoint)), "init"})
	case ds == engine.DirEmpty:
		p.writeMarker = true
		p.Needs = append(p.Needs, Need{NeedInit, "Target is empty and will be claimed by this stream (a " + engine.MarkerName + " file is written)", "init"})
	default:
		p.writeMarker = true
		p.Warnings = append(p.Warnings, fmt.Sprintf("target already contains %d item(s) not created by tines", n))
		p.Needs = append(p.Needs, Need{NeedAdopt, fmt.Sprintf("Target holds %d foreign item(s). Adopting makes it mirror the source; extra files are moved to trash, never deleted.", n), "adopt"})
	}
	return true
}

// Prepare runs all preflight checks and computes the plan for one target.
func (e *Env) Prepare(ctx context.Context, st config.Stream, t config.Target, src SourceInfo) *Prepared {
	p := &Prepared{Stream: st, Target: t, RunID: NewRunID()}
	if !src.OK {
		p.Blocks = append(p.Blocks, src.Err)
		return p
	}
	if endpoint.Overlap(st.Source.Spec, t.Spec) {
		p.Blocks = append(p.Blocks, "source and target overlap (one is inside the other)")
		return p
	}
	for _, o := range st.Targets {
		if o.ID != t.ID && endpoint.Overlap(o.Spec, t.Spec) {
			p.Blocks = append(p.Blocks, fmt.Sprintf("target overlaps with target %q", o.Name))
			return p
		}
	}
	if !e.classify(ctx, st, t, p) || len(p.Blocks) > 0 {
		return p
	}
	if src.Files == 0 {
		p.Needs = append(p.Needs, Need{NeedEmptySource, "SOURCE HAS NO FILES. Mirroring would move everything on the target to trash.", "empty"})
	}
	trash := engine.Join(t.Spec, engine.TrashDir+"/"+p.RunID)
	plan, err := engine.DryRun(ctx, st.Source.Spec, t.Spec, trash, st.Settings)
	if err != nil && !(engine.Code(err) == engine.CodeDirNotFound && p.writeMarker) {
		p.Blocks = append(p.Blocks, "could not compute plan: "+err.Error())
		return p
	}
	p.Plan = plan
	if sz, err := engine.Size(ctx, t.Spec, st.Settings.Excludes); err == nil {
		p.DestSize = sz
	}
	e.checkSpace(ctx, t, p)
	if mp := st.Settings.MaxDeletePercent; mp > 0 && p.Plan.Trashed > 10 && p.DestSize.Count > 0 &&
		p.Plan.Trashed*100 > mp*int(p.DestSize.Count) {
		p.Needs = append(p.Needs, Need{NeedMassDelete,
			fmt.Sprintf("%d of %d files on the target (%d%% > limit %d%%) would be moved to trash.",
				p.Plan.Trashed, p.DestSize.Count, p.Plan.Trashed*100/int(p.DestSize.Count), mp), "delete"})
	}
	e.State.Update(st.ID, t.ID, func(_ *state.StreamState, ts *state.TargetState) {
		ts.CheckedAt = time.Now()
		ts.Behind = p.Plan.Changes()
		ts.BehindBytes = p.Plan.Bytes
	})
	return p
}

// checkSpace blocks the run when the target cannot hold what the plan copies.
// Overwritten and removed files are moved to the target's trash, not freed, so
// the full size of everything copied has to fit.
func (e *Env) checkSpace(ctx context.Context, t config.Target, p *Prepared) {
	need := p.Plan.Bytes
	if need == 0 {
		return
	}
	free, ok := engine.Free(ctx, t.Spec)
	if !ok {
		p.Warnings = append(p.Warnings, fmt.Sprintf("could not determine free space on the target; %s will be written", humanSize(need)))
		return
	}
	p.Free, p.FreeKnown = free, true
	switch {
	case need > free:
		p.Blocks = append(p.Blocks, fmt.Sprintf("not enough space on the target: the backup needs %s but only %s is free (short by %s). Free up space or add exclusions.",
			humanSize(need), humanSize(free), humanSize(need-free)))
	case need > free-free/10: // would leave under 10% free
		p.Warnings = append(p.Warnings, fmt.Sprintf("target will be nearly full: %s needed, %s free", humanSize(need), humanSize(free)))
	}
}

func humanSize(b int64) string {
	const u = 1024
	if b < u {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(u), 0
	for n := b / u; n >= u; n /= u {
		div *= u
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

type Result struct {
	OK       bool
	Message  string
	Verified *engine.VerifyResult
	Warnings []string
}

// Execute performs the sync for a prepared (and confirmed) target.
// The caller must hold the stream lock and must have satisfied every Need.
func (e *Env) Execute(ctx context.Context, p *Prepared, on func(engine.Progress)) (res Result) {
	st, t := p.Stream, p.Target
	defer func() {
		status := "ok"
		if !res.OK {
			status = "failed"
			if ctx.Err() != nil {
				status = "cancelled"
			}
		}
		e.State.Update(st.ID, t.ID, func(ss *state.StreamState, ts *state.TargetState) {
			ts.LastRun, ts.LastStatus, ts.LastMessage = time.Now(), status, res.Message
			if res.OK {
				ts.LastOK, ts.Behind, ts.BehindBytes, ts.CheckedAt = time.Now(), 0, 0, time.Now()
				if res.Verified != nil {
					ts.LastVerify, ts.VerifyOK = time.Now(), res.Verified.OK()
				}
			}
		})
		if !res.OK && status == "failed" {
			Notify(st, status, t.Name+": "+res.Message)
		}
	}()
	if !p.Runnable() {
		return Result{Message: "blocked: " + strings.Join(p.Blocks, "; ")}
	}
	if p.writeMarker {
		m := Marker{"tines", 1, st.ID, st.Name, t.ID, time.Now().UTC()}
		b, _ := json.MarshalIndent(m, "", "  ")
		if err := engine.WriteFile(ctx, engine.Join(t.Spec, engine.MarkerName), b); err != nil {
			return Result{Message: "could not write target marker: " + err.Error()}
		}
	}
	trash := engine.Join(t.Spec, engine.TrashDir+"/"+p.RunID)
	if err := engine.Sync(ctx, st.Source.Spec, t.Spec, trash, st.Settings, on); err != nil {
		if ctx.Err() != nil {
			return Result{Message: "cancelled"}
		}
		return Result{Message: err.Error()}
	}
	res = Result{OK: true, Message: fmt.Sprintf("synced: %d copied, %d moved to trash", p.Plan.Copies, p.Plan.Trashed)}
	if st.Settings.VerifyAfter {
		v, err := engine.Verify(ctx, st.Source.Spec, t.Spec, st.Settings)
		if err != nil {
			res.OK, res.Message = false, "sync done but verification failed to run: "+err.Error()
			return res
		}
		res.Verified = &v
		if !v.OK() {
			res.OK = false
			res.Message = fmt.Sprintf("sync done but VERIFY FOUND PROBLEMS: %d differ, %d missing, %d extra, %d errors", v.Differ, v.MissingOnTarget, v.ExtraOnTarget, v.Errors)
			return res
		}
		res.Message += fmt.Sprintf(", verified %d files", v.Same)
	}
	if days := st.Settings.TrashRetentionDays; days > 0 {
		if n := pruneTrash(ctx, t.Spec, days); n > 0 {
			res.Message += fmt.Sprintf(", pruned %d old trash run(s)", n)
		}
	}
	if st.Settings.MirrorConfig {
		if err := e.MirrorConfig(ctx, t); err != nil {
			res.Warnings = append(res.Warnings, "config mirror failed: "+err.Error())
		}
	}
	return res
}

// pruneTrash removes trash run folders older than the retention period.
func pruneTrash(ctx context.Context, targetSpec string, days int) int {
	dirs, err := engine.ListDirs(ctx, engine.Join(targetSpec, engine.TrashDir))
	if err != nil {
		return 0
	}
	sort.Strings(dirs)
	cutoff := time.Now().UTC().AddDate(0, 0, -days)
	n := 0
	for _, d := range dirs {
		ts, err := time.Parse("20060102-150405", d) // only ever touch folders named by us
		if err != nil || !ts.Before(cutoff) {
			continue
		}
		if engine.Purge(ctx, engine.Join(targetSpec, engine.TrashDir+"/"+d)) == nil {
			n++
		}
	}
	return n
}

// MirrorConfig stores a copy of the whole config inside the target so a lost
// machine does not mean a lost setup. Restore with `tines import --from`.
func (e *Env) MirrorConfig(ctx context.Context, t config.Target) error {
	b, err := e.Cfg.Marshal()
	if err != nil {
		return err
	}
	return engine.WriteFile(ctx, engine.Join(t.Spec, engine.MetaDir+"/config.yaml"), b)
}

// Notify runs the stream's notify_command on failure.
func Notify(st config.Stream, status, msg string) {
	if st.Settings.NotifyCommand == "" {
		return
	}
	c := exec.Command("sh", "-c", st.Settings.NotifyCommand)
	c.Env = append(os.Environ(), "TINES_STREAM="+st.Name, "TINES_STATUS="+status, "TINES_MESSAGE="+msg)
	_ = c.Start()
	go c.Wait()
}

// Check recomputes drift for a target (dry run) and stores it.
func (e *Env) Check(ctx context.Context, st config.Stream, t config.Target) (engine.Plan, error) {
	trash := engine.Join(t.Spec, engine.TrashDir+"/check")
	plan, err := engine.DryRun(ctx, st.Source.Spec, t.Spec, trash, st.Settings)
	e.State.Update(st.ID, t.ID, func(_ *state.StreamState, ts *state.TargetState) {
		if err == nil {
			ts.CheckedAt, ts.Behind, ts.BehindBytes = time.Now(), plan.Changes(), plan.Bytes
		}
	})
	return plan, err
}

// Verify compares content and stores the result.
func (e *Env) Verify(ctx context.Context, st config.Stream, t config.Target) (engine.VerifyResult, error) {
	v, err := engine.Verify(ctx, st.Source.Spec, t.Spec, st.Settings)
	if err == nil {
		e.State.Update(st.ID, t.ID, func(_ *state.StreamState, ts *state.TargetState) {
			ts.LastVerify, ts.VerifyOK = time.Now(), v.OK()
		})
	}
	return v, err
}

// Ping checks an endpoint's connection. Unmounted removable volumes are
// reported as unreachable instead of "directory missing".
func Ping(ctx context.Context, spec string) (time.Duration, error) {
	if dir, bad := endpoint.UnmountedVolume(spec); bad {
		return 0, fmt.Errorf("volume %s is not mounted", dir)
	}
	return engine.Ping(ctx, spec)
}
