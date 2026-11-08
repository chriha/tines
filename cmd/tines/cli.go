package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/endpoint"
	"github.com/chriha/tines/internal/engine"
	"github.com/chriha/tines/internal/runner"
	"github.com/chriha/tines/internal/state"
)

func human(b int64) string {
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

func newEnv() (*runner.Env, error) {
	p := config.Path()
	cfg, err := config.Load(p)
	if err != nil {
		return nil, err
	}
	st, err := state.Open()
	if err != nil {
		return nil, err
	}
	return &runner.Env{Cfg: cfg, CfgPath: p, State: st}, nil
}

func ctxSignal() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func ask(prompt string) string {
	fmt.Print(prompt)
	s, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(s)
}

func cmdList(env *runner.Env) error {
	if len(env.Cfg.Streams) == 0 {
		fmt.Println("No streams yet. Run `tines` and press n, or `tines new`.")
		return nil
	}
	for _, s := range env.Cfg.Streams {
		fmt.Printf("%s\n  source  %s\n", s.Name, endpoint.Display(s.Source))
		for _, t := range s.Targets {
			ts, _ := env.State.Get(s.ID, t.ID)
			last := "never"
			if !ts.LastOK.IsZero() {
				last = ts.LastOK.Format("2006-01-02 15:04")
			}
			fmt.Printf("  target  %-14s %s   last ok: %s\n", t.Name, endpoint.Display(t.Endpoint), last)
		}
	}
	return nil
}

func cmdRun(env *runner.Env, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	yes := fs.Bool("yes", false, "skip the final y/N prompt (never skips safety confirmations)")
	initT := fs.Bool("init-targets", false, "allow claiming empty/missing targets")
	mass := fs.Bool("allow-mass-delete", false, "allow plans that trash more than the stream's limit")
	dry := fs.Bool("dry-run", false, "show the plan and exit")
	only := fs.String("target", "", "only this target (name)")
	if err := fs.Parse(reorder(args)); err != nil || fs.NArg() != 1 {
		return fmt.Errorf("usage: tines run <stream> [--dry-run] [--yes] [--target name] [--init-targets] [--allow-mass-delete]")
	}
	st := env.Cfg.Find(fs.Arg(0))
	if st == nil {
		return fmt.Errorf("no stream named %q", fs.Arg(0))
	}
	ctx, cancel := ctxSignal()
	defer cancel()
	lock, err := state.LockStream(st.ID)
	if err != nil {
		return err
	}
	defer lock.Unlock()

	fmt.Printf("Stream %q: checking source %s ...\n", st.Name, endpoint.Display(st.Source))
	src := env.CheckSource(ctx, *st)
	if !src.OK {
		return fmt.Errorf("source: %s", src.Err)
	}
	fmt.Printf("  %d files, %s\n", src.Files, human(src.Bytes))
	var ready []*runner.Prepared
	for _, t := range st.Targets {
		if *only != "" && !strings.EqualFold(*only, t.Name) {
			continue
		}
		p := env.Prepare(ctx, *st, t, src)
		fmt.Printf("\nTarget %q  %s\n", t.Name, endpoint.Display(t.Endpoint))
		for _, b := range p.Blocks {
			fmt.Println("  ✗ BLOCKED:", b)
		}
		if !p.Runnable() {
			continue
		}
		fmt.Printf("  plan: %d new, %d updated, %d to trash, %s to transfer\n", p.Plan.New(), p.Plan.Updated, p.Plan.Trashed, human(p.Plan.Bytes))
		for _, c := range p.Plan.Samples[:min(8, len(p.Plan.Samples))] {
			fmt.Printf("    %-5s %s\n", c.Op, c.Path)
		}
		for _, w := range p.Warnings {
			fmt.Println("  ! ", w)
		}
		ok := true
		for _, n := range p.Needs {
			fmt.Println("  ⚠ ", n.Prompt)
			switch {
			case n.Kind == runner.NeedInit && *initT, n.Kind == runner.NeedMassDelete && *mass:
				continue
			case *dry:
				continue
			}
			if ask(fmt.Sprintf("    type %q to confirm (anything else skips this target): ", n.Expect)) != n.Expect {
				ok = false
				fmt.Println("    skipped.")
				break
			}
		}
		if ok {
			ready = append(ready, p)
		}
	}
	if *dry || len(ready) == 0 {
		if *dry {
			fmt.Println("\n(dry run - nothing was changed)")
		}
		return nil
	}
	if !*yes && !strings.EqualFold(ask(fmt.Sprintf("\nStart backup to %d target(s)? [y/N] ", len(ready))), "y") {
		fmt.Println("Aborted.")
		return nil
	}
	failed := 0
	for _, p := range ready {
		fmt.Printf("\n→ %s\n", p.Target.Name)
		last := time.Now()
		res := env.Execute(ctx, p, func(pr engine.Progress) {
			if time.Since(last) < time.Second || !isTTY() {
				return
			}
			last = time.Now()
			pct := 0
			if pr.TotalBytes > 0 {
				pct = int(100 * pr.Bytes / pr.TotalBytes)
			}
			fmt.Printf("\r  %3d%%  %s/%s  %s/s  %s\033[K", pct, human(pr.Bytes), human(pr.TotalBytes), human(int64(pr.Speed)), pr.Current)
		})
		fmt.Print("\r\033[K")
		if res.OK {
			fmt.Println("  ✔", res.Message)
		} else {
			failed++
			fmt.Println("  ✗", res.Message)
		}
		for _, w := range res.Warnings {
			fmt.Println("  !", w)
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d target(s) failed", failed)
	}
	return nil
}

func cmdVerify(env *runner.Env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: tines verify <stream>")
	}
	st := env.Cfg.Find(args[0])
	if st == nil {
		return fmt.Errorf("no stream named %q", args[0])
	}
	ctx, cancel := ctxSignal()
	defer cancel()
	bad := 0
	for _, t := range st.Targets {
		fmt.Printf("verifying %s ... ", t.Name)
		v, err := env.Verify(ctx, *st, t)
		switch {
		case err != nil:
			bad++
			fmt.Println("error:", err)
		case v.OK():
			fmt.Printf("✔ identical (%d files)\n", v.Same)
		default:
			bad++
			fmt.Printf("✗ %d differ, %d missing on target, %d extra on target, %d errors\n", v.Differ, v.MissingOnTarget, v.ExtraOnTarget, v.Errors)
			for _, s := range v.Samples {
				fmt.Println("    ", s)
			}
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d target(s) not in sync", bad)
	}
	return nil
}

func cmdStatus(env *runner.Env, args []string) error {
	ctx, cancel := ctxSignal()
	defer cancel()
	for _, s := range env.Cfg.Streams {
		if len(args) > 0 && !strings.EqualFold(args[0], s.Name) {
			continue
		}
		d, err := runner.Ping(ctx, s.Source.Spec)
		fmt.Printf("%s  source %s\n", s.Name, connStr(d, err))
		for _, t := range s.Targets {
			d, err := runner.Ping(ctx, t.Spec)
			plan, perr := env.Check(ctx, s, t)
			sync := "unknown"
			switch {
			case perr != nil:
			case plan.InSync():
				sync = "in sync"
			default:
				sync = fmt.Sprintf("behind: %d new/updated, %d to trash", plan.Copies, plan.Trashed)
			}
			fmt.Printf("  → %-14s %-26s %s\n", t.Name, connStr(d, err), sync)
		}
	}
	return nil
}

func connStr(d time.Duration, err error) string {
	if err == nil || engine.Code(err) == engine.CodeDirNotFound {
		if err != nil {
			return "● reachable (directory missing)"
		}
		return fmt.Sprintf("● connected %dms", d.Milliseconds())
	}
	return "○ unreachable"
}

func cmdSchedule(env *runner.Env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: tines schedule <stream>")
	}
	st := env.Cfg.Find(args[0])
	if st == nil {
		return fmt.Errorf("no stream named %q", args[0])
	}
	exe, _ := os.Executable()
	sched := st.Settings.Schedule
	if sched == "" {
		sched = "0 3 * * *"
		fmt.Println("# no schedule set for this stream; example nightly at 03:00 (set `schedule:` in the config to change)")
	}
	fmt.Println("# Scheduled runs are unattended: safety confirmations cannot be given, so first-time target")
	fmt.Println("# initialisation or mass-delete plans will be skipped. Run once interactively first.")
	fmt.Printf("%s %s run %q --yes\n", sched, exe, st.Name)
	return nil
}

func cmdDoctor() error {
	v, err := engine.Available()
	if err != nil {
		fmt.Println("✗", err)
		return err
	}
	fmt.Println("✔", v)
	fmt.Println("✔ config:", config.Path())
	fmt.Println("✔ state: ", state.Dir())
	return nil
}

func cmdExport(env *runner.Env, args []string) error {
	b, err := env.Cfg.Marshal()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "-" {
		_, err = os.Stdout.Write(b)
		return err
	}
	return os.WriteFile(args[0], b, 0o600)
}

// import: from a file, or `--from <rclone spec>` (reads <spec>/.tines-meta/config.yaml).
func cmdImport(env *runner.Env, args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	from := fs.String("from", "", "rclone path of a backup target to restore the config from")
	force := fs.Bool("force", false, "replace an existing non-empty config (a .bak is kept)")
	_ = fs.Parse(reorder(args))
	var data []byte
	var err error
	switch {
	case *from != "":
		data, err = engine.ReadFile(context.Background(), engine.Join(*from, engine.MetaDir+"/config.yaml"))
	case fs.NArg() == 1:
		data, err = os.ReadFile(fs.Arg(0))
	default:
		return fmt.Errorf("usage: tines import <file> | tines import --from <target-path> [--force]")
	}
	if err != nil {
		return err
	}
	c, err := config.Parse(data)
	if err != nil {
		return err
	}
	if len(env.Cfg.Streams) > 0 && !*force {
		return fmt.Errorf("a config with %d stream(s) already exists; use --force to replace it (the old one is kept as %s.bak)", len(env.Cfg.Streams), env.CfgPath)
	}
	if err := c.Save(env.CfgPath); err != nil {
		return err
	}
	fmt.Printf("imported %d stream(s) into %s\n", len(c.Streams), env.CfgPath)
	return nil
}

// reorder lets flags appear after the positional argument.
func reorder(args []string) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if (a == "--target" || a == "-target" || a == "--from" || a == "-from") && i+1 < len(args) && !strings.Contains(a, "=") {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
