# tines ⑂

**Safe, redundant backups from your terminal.** Back up one source to as many
targets as you like, watch every connection live, and never worry about an
accidental overwrite.

Like the tines of a fork, `tines` fans a single source out to N targets: an
external drive, a NAS over SMB, a server over SSH, a cloud bucket. All of it is
managed from a Bubble Tea dashboard that draws your backups as a flow chart.

```
 STREAMS               ● Photos
 › ● Photos            trash kept 90d · tripwire 20% · verify on · config mirror on
   ◐ Documents
                                                  ╭──────────────────────────────╮
                                                  │ ● ssd  12ms                  │
                                          ┌─────▶ │ ✔ in sync  checked just now  │
 ╭────────────────────╮                   │       │ last backup: 2h ago ✔        │
 │ ● SOURCE  4ms      │───────────────────┼─────▶ │ ● nas  41ms                  │
 │ /Users/me/Photos   │                   │       │ ◐ 12 file(s) behind          │
 ╰────────────────────╯                   └┈┈┈┈┈▷ │ ○ usb-stick  offline         │
```

## Why tines

Backup tools usually fail in the boring ways: the wrong disk mounted, an empty
source "mirrored" over a full target, an `rsync --delete` typo. `tines` is built
around making those mistakes hard to make.

### Safety model

| Guard | What it does |
|---|---|
| **Plan first** | Every run starts with a dry run that lists what will be copied, overwritten and trashed. Nothing is written until you confirm. |
| **Trash, not delete** | Files that are overwritten or removed on a target are *moved* to `<target>/.tines-trash/<timestamp>/`. Trash is only pruned after your retention period (default 90 days). |
| **Target markers** | A target gets a `.tines-target.json` marker. `tines` refuses to write into a folder with a marker from a different stream, and into a non-empty folder without a marker unless you type `adopt`. |
| **Unplugged drive detection** | A missing `/Volumes/X` (or `/media/...`) is never silently recreated on your boot disk. |
| **Mass-delete tripwire** | A plan that would trash more than N% of a target (default 20%) needs a typed confirmation. So does an empty source. |
| **No overlaps** | Source and targets (and targets of other streams) may never be the same folder or nested inside each other. |
| **One run at a time** | A per-stream lock prevents concurrent runs, even across processes. |
| **Free-space check** | If the plan copies more than the target has free, the run is blocked before anything is written; a target that would end up over 90% full gets a warning. |
| **Verify** | Optional checksum verification after each run, or on demand with `tines verify`. It is read-only. |
| **Typed confirmations** | Risky steps require typing a word (`init`, `adopt`, `delete`, `empty`), not just pressing `y`. |
| **Deleting a stream is safe** | Removing a stream or target only edits the config. Backup data is never touched. |

## Features

- **1 → N streams**: one source, any number of redundant targets.
- **Wizard** to create a stream in a minute: it tests every connection as you go.
- **Live dashboard**: connection status and latency of the source and every target (refreshed every 5 s), sync state per target ("in sync" / "12 files behind"), animated transfer lines, progress, speed and ETA.
- **Manual start** from the dashboard (`r`) or the command line (`tines run`).
- **Many named streams**, each with its own settings (excludes, bandwidth limit, retention, tripwire, verification, notifications).
- **One global config** (`~/.config/tines/config.yaml`) describing everything. It is also copied into each target (`.tines-meta/config.yaml`), so a lost laptop does not mean a lost setup.
- **Any backend** through [rclone](https://rclone.org): local folders, external drives, SFTP, SMB, S3, B2, WebDAV, Google Drive, and about 50 more.

## Install

`tines` uses [rclone](https://rclone.org/install/) as its transfer engine.

```sh
brew install rclone            # or see https://rclone.org/install/
go install github.com/chriha/tines/cmd/tines@latest
tines doctor
```

Or build from source:

```sh
git clone https://github.com/chriha/tines && cd tines
make build && ./bin/tines
```

Supported: macOS and Linux.

## Quick start

```sh
tines            # open the dashboard (the wizard starts if you have no streams yet)
```

1. Press **`n`** and follow the wizard: name → source → one or more targets → safety settings.
2. Press **`r`** to see the plan, confirm, and start the backup.
3. Press **`c`** any time to re-check whether targets are in sync, **`v`** to verify checksums.

### Dashboard keys

| Key | Action | Key | Action |
|---|---|---|---|
| `↑` `↓` | switch stream | `n` | new stream (wizard) |
| `r` | plan + run backup | `a` | add a target |
| `c` | check sync state | `e` | edit settings |
| `v` | verify (read-only) | `d` | remove target / delete stream |
| `x` | cancel running backup | `?` | safety help |
| `p` | ping all connections now | `q` | quit |

### Command line

```
tines                      open the dashboard
tines new                  open the wizard
tines list                 list streams and targets
tines status [stream]      connection + sync state (scriptable)
tines run <stream>         plan, confirm and run   [--dry-run] [--yes] [--target name]
                           [--init-targets] [--allow-mass-delete]
tines verify <stream>      compare source and every target (read-only)
tines schedule <stream>    print a crontab line for unattended runs
tines export [file]        write the global config
tines import <file>        restore config, or: tines import --from <target-path>
tines doctor               check the rclone installation
```

`--yes` only skips the final "start?" question. It never bypasses a safety
confirmation; claiming a new target needs `--init-targets` and a large delete
needs `--allow-mass-delete`. Unattended runs therefore skip anything unusual and
fail loudly (run `notify_command` on failure) instead of guessing.

## Configuration

```yaml
version: 1
defaults:                      # copied into every new stream
  trash_retention_days: 90
  max_delete_percent: 20
  mirror_config: true
streams:
  - id: 3fa9c1d2e8
    name: Photos
    source: { kind: local, spec: /Users/me/Photos }
    targets:
      - { id: 9b1c0a77d2, name: ssd, kind: local, spec: /Volumes/Backup/Photos }
      - { id: 51e7f2a0cc, name: nas, kind: smb, spec: ":smb,host='nas.local',user='me',pass='enc:v1:…':media/photos" }
    settings:
      excludes: ["/Cache/**", "/Projects/big.iso", "node_modules/**", "*.tmp"]
      bandwidth_limit: 10M
      checksum: false
      transfers: 4
      trash_retention_days: 90
      max_delete_percent: 20
      verify_after: true
      mirror_config: true
      notify_command: osascript -e 'display notification "$TINES_MESSAGE"'
      schedule: "0 3 * * *"
```

A `spec` is any [rclone path](https://rclone.org/docs/#syntax-of-remote-paths), so
existing rclone remotes (`mynas:backups/photos`) work out of the box.

State (last runs, sync cache) lives separately in `~/.local/state/tines/`. The config
file is written atomically with mode `0600` and the previous version is kept as `config.yaml.bak`.

**Secrets:** SFTP uses your ssh-agent or a key file and stores no password. SMB passwords are
*encrypted* (AES-256-GCM) in the config as `pass='enc:v1:…'`. The key is kept in the macOS Keychain, or
in `~/.config/tines/secret.key` (mode `0600`) elsewhere; it is never written to the config or mirrored to
targets. After restoring a config on a new machine, re-add SMB targets (the key does not travel with the config).
The password is decrypted only in memory when rclone is started.

### Disaster recovery

Because each target carries a copy of the config:

```sh
tines import --from /Volumes/Backup/Photos      # any target path or rclone spec
```

### Scheduling

`tines schedule Photos` prints a ready-to-paste crontab line. Run each stream once interactively first,
so that targets are initialised and the safety confirmations have been given.

## How it works

`tines` is a safety and UX layer over `rclone sync`:
`--backup-dir` provides the trash, `--dry-run` provides the plan, `rclone check` provides verification,
and a reserved `.tines-*` namespace on each target is excluded from every sync so tines' own files can never be deleted by a sync.

## Development

```sh
make test      # unit tests; integration tests use real rclone and skip if it is missing
make build
```

## License

[MIT](LICENSE)

## Excludes

Every stream has an `excludes` list (edit it in the TUI or in the config). Patterns use
[rclone filter syntax](https://rclone.org/filtering/) and apply to sync, plan, size and verify alike:

| Pattern | Matches |
|---|---|
| `/Cache/**` | the top-level `Cache` directory and everything in it |
| `/Projects/big.iso` | exactly that one file |
| `node_modules/**` | a `node_modules` directory at any depth |
| `*.tmp` | any `.tmp` file at any depth |

A leading `/` anchors the pattern to the source root; without it the pattern matches at any depth.
Use `**` to exclude a directory's contents. Excluded paths are neither copied nor deleted from the target.

### macOS and Windows system files (always excluded)

macOS and Windows create files on volumes on their own, which would otherwise make `verify` fail.
These are excluded from every stream automatically: `._*`, `.DS_Store`, `.AppleDouble`, `.Spotlight-V100`,
`.Trashes`, `.fseventsd`, `.TemporaryItems`, `.DocumentRevisions-V100`, `.VolumeIcon.icns`, `.apdisk`,
`.localized`, `.com.apple.timemachine.donotpresent`, `Thumbs.db`, `desktop.ini`, `$RECYCLE.BIN`,
`System Volume Information`.

`._name` files are AppleDouble sidecars: on filesystems that cannot store macOS metadata natively
(exFAT, FAT32, SMB/NFS shares), macOS writes extended attributes, tags and resource forks into a hidden `._name`
file next to `name`. They are not your data.
