package runner

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/endpoint"
	"github.com/chriha/tines/internal/engine"
)

// RefreshCapacity asks a connected target for its size and free space and
// stores the reading as "last known". Targets that cannot report capacity
// keep whatever was stored before.
func (e *Env) RefreshCapacity(ctx context.Context, t config.Target) (engine.Capacity, bool) {
	c, ok := engine.About(ctx, t.Spec)
	if ok && c.Total > 0 {
		e.State.SetCapacity(t.ID, c.Total, c.Free)
		return c, true
	}
	return c, false
}

// ---- sync preview -------------------------------------------------------

// RowState is what a sync does with one file on one target.
type RowState int

const (
	RowSame     RowState = iota // included, already identical on the target
	RowNew                      // will be copied
	RowUpdate                   // will overwrite the target's version (old one goes to trash)
	RowTrash                    // only on the target: will be moved to trash
	RowUnknown                  // included, but the target could not be compared
	RowExclSys                  // not synced: OS-generated or tines-reserved
	RowExclRule                 // not synced: matches an exclude pattern
)

// Synced reports whether the file takes part in the sync.
func (s RowState) Synced() bool { return s != RowExclSys && s != RowExclRule }

// Changes reports whether a run would touch the target for this file.
func (s RowState) Changes() bool { return s == RowNew || s == RowUpdate || s == RowTrash }

type Row struct {
	Path  string
	Size  int64
	State RowState
}

type TargetPreview struct {
	Target  config.Target
	Compare bool   // false: target could not be compared, see Err
	Err     string // why not
	changes map[string]engine.Change
	trash   []engine.Change
}

type Preview struct {
	Stream    config.Stream
	SourceErr string
	Files     []engine.PreviewFile
	Targets   []TargetPreview
	Built     time.Time
}

// BuildPreview lists the source (with and without the exclude filters) and
// dry-runs every target. Nothing is written anywhere.
func (e *Env) BuildPreview(ctx context.Context, st config.Stream) *Preview {
	pv := &Preview{Stream: st, Built: time.Now(), Targets: make([]TargetPreview, len(st.Targets))}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if dir, bad := endpoint.UnmountedVolume(st.Source.Spec); bad {
			pv.SourceErr = fmt.Sprintf("source volume %s is not mounted", dir)
			return
		}
		files, err := engine.PreviewSource(ctx, st.Source.Spec, st.Settings.Excludes)
		if err != nil {
			pv.SourceErr = err.Error()
			return
		}
		pv.Files = files
	}()
	for i, t := range st.Targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tp := TargetPreview{Target: t}
			defer func() { pv.Targets[i] = tp }()
			if dir, bad := endpoint.UnmountedVolume(t.Spec); bad {
				tp.Err = fmt.Sprintf("volume %s is not mounted", dir)
				return
			}
			trash := engine.Join(t.Spec, engine.TrashDir+"/preview")
			plan, err := engine.DryRun(ctx, st.Source.Spec, t.Spec, trash, st.Settings)
			if err != nil && engine.Code(err) != engine.CodeDirNotFound {
				tp.Err = err.Error()
				return
			}
			tp.Compare = true
			tp.changes = map[string]engine.Change{}
			for _, c := range plan.Files {
				if c.Op == "trash" {
					tp.trash = append(tp.trash, c)
				} else {
					tp.changes[c.Path] = c
				}
			}
		}()
	}
	wg.Wait()
	return pv
}

// Rows merges the source listing with one target's dry-run into a single
// list sorted by path.
func (p *Preview) Rows(target int) []Row {
	var tp *TargetPreview
	if target >= 0 && target < len(p.Targets) {
		tp = &p.Targets[target]
	}
	rows := make([]Row, 0, len(p.Files))
	for _, f := range p.Files {
		r := Row{Path: f.Path, Size: f.Size}
		switch {
		case f.Excluded == engine.ExcludedSystem:
			r.State = RowExclSys
		case f.Excluded == engine.ExcludedRule:
			r.State = RowExclRule
		case tp == nil || !tp.Compare:
			r.State = RowUnknown
		default:
			switch tp.changes[f.Path].Op {
			case "new":
				r.State = RowNew
			case "update":
				r.State = RowUpdate
			default:
				r.State = RowSame
			}
		}
		rows = append(rows, r)
	}
	if tp != nil {
		for _, c := range tp.trash {
			rows = append(rows, Row{Path: c.Path, Size: c.Size, State: RowTrash})
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	}
	return rows
}

// Summary counts rows per state.
type Summary struct {
	New, Update, Trash, Same, Unknown, Excluded int
	Bytes                                       int64 // to copy
}

func Summarize(rows []Row) Summary {
	var s Summary
	for _, r := range rows {
		switch r.State {
		case RowNew:
			s.New++
			s.Bytes += r.Size
		case RowUpdate:
			s.Update++
			s.Bytes += r.Size
		case RowTrash:
			s.Trash++
		case RowSame:
			s.Same++
		case RowUnknown:
			s.Unknown++
		default:
			s.Excluded++
		}
	}
	return s
}
