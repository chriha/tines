package main

import (
	"fmt"
	"os"

	"github.com/chrishartmann/tines/internal/engine"
	"github.com/chrishartmann/tines/internal/runner"
	"github.com/chrishartmann/tines/internal/tui"
)

var version = "dev"

const usage = `tines - safe one-to-many backups from your terminal

Usage:
  tines                      open the dashboard
  tines new                  open the dashboard straight into the stream wizard
  tines list                 list streams and targets
  tines status [stream]      connection + sync state (non-interactive)
  tines run <stream>         plan, confirm and run a backup  [--dry-run] [--yes] [--target n]
                             [--init-targets] [--allow-mass-delete]
  tines verify <stream>      compare source and every target (read-only)
  tines schedule <stream>    print a crontab line for unattended runs
  tines export [file]        write the global config (default: stdout)
  tines import <file>        restore config  (or: --from <target-path>) [--force]
  tines doctor               check that rclone is installed
  tines version

Config: $TINES_CONFIG or ~/.config/tines/config.yaml
`

func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "version", "--version", "-v":
		fmt.Println("tines", version)
		return
	case "help", "--help", "-h":
		fmt.Print(usage)
		return
	case "doctor":
		err = cmdDoctor()
	default:
		if _, e := engine.Available(); e != nil {
			fmt.Fprintln(os.Stderr, "error:", e)
			os.Exit(1)
		}
		var env = mustEnv()
		switch cmd {
		case "":
			err = tui.Run(env, false)
		case "new":
			err = tui.Run(env, true)
		case "list", "ls":
			err = cmdList(env)
		case "status":
			err = cmdStatus(env, args)
		case "run":
			err = cmdRun(env, args)
		case "verify":
			err = cmdVerify(env, args)
		case "schedule":
			err = cmdSchedule(env, args)
		case "export":
			err = cmdExport(env, args)
		case "import":
			err = cmdImport(env, args)
		default:
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func mustEnv() *runner.Env {
	env, err := newEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return env
}
