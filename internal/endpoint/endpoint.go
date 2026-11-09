// Package endpoint turns friendly wizard answers into rclone specs and
// provides display/overlap helpers.
package endpoint

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/chriha/tines/internal/config"
	"github.com/chriha/tines/internal/secret"
)

type Field struct {
	Key, Label, Placeholder, Default string
	Secret, Optional                 bool
}

type Kind struct {
	ID, Label, Hint string
	Fields          []Field
}

var Kinds = []Kind{
	{ID: "local", Label: "Local folder / external drive", Hint: "A directory on this machine, including mounted drives and mounted network shares.",
		Fields: []Field{{Key: "path", Label: "Path", Placeholder: "/Volumes/Backup/photos"}}},
	{ID: "sftp", Label: "Server over SSH (SFTP)", Hint: "Uses your ssh-agent, or an optional key file. Passwords are not stored.",
		Fields: []Field{
			{Key: "host", Label: "Host", Placeholder: "server.example.com"},
			{Key: "user", Label: "User", Placeholder: "me"},
			{Key: "port", Label: "Port", Default: "22"},
			{Key: "path", Label: "Remote path", Placeholder: "/srv/backups/photos"},
			{Key: "key", Label: "Key file (optional)", Placeholder: "~/.ssh/id_ed25519", Optional: true}}},
	{ID: "smb", Label: "SMB / Windows network share", Hint: "The password is stored encrypted (AES-256-GCM); the key lives in your Keychain or a 0600 key file.",
		Fields: []Field{
			{Key: "host", Label: "Host", Placeholder: "nas.local"},
			{Key: "user", Label: "User", Placeholder: "me"},
			{Key: "pass", Label: "Password", Secret: true},
			{Key: "domain", Label: "Domain (optional)", Default: "WORKGROUP", Optional: true},
			{Key: "path", Label: "Share and path", Placeholder: "media/photos"}}},
	{ID: "remote", Label: "Existing rclone remote", Hint: "Anything you configured with `rclone config`: S3, B2, Google Drive, WebDAV, ...",
		Fields: []Field{{Key: "path", Label: "Remote:path", Placeholder: "mynas:backups/photos"}}},
}

func KindByID(id string) Kind {
	for _, k := range Kinds {
		if k.ID == id {
			return k
		}
	}
	return Kinds[0]
}

func quote(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	return p
}

// Build validates the answers and returns an endpoint.
func Build(ctx context.Context, kind string, v map[string]string) (config.Endpoint, error) {
	for _, f := range KindByID(kind).Fields {
		if !f.Optional && strings.TrimSpace(v[f.Key]) == "" {
			return config.Endpoint{}, fmt.Errorf("%s is required", f.Label)
		}
	}
	p := strings.TrimSpace(v["path"])
	switch kind {
	case "local":
		p = ExpandHome(p)
		if !filepath.IsAbs(p) {
			abs, err := filepath.Abs(p)
			if err != nil {
				return config.Endpoint{}, err
			}
			p = abs
		}
		return config.Endpoint{Kind: kind, Spec: filepath.Clean(p)}, nil
	case "sftp":
		s := ":sftp,host=" + quote(v["host"]) + ",user=" + quote(v["user"]) + ",port=" + quote(orDefault(v["port"], "22"))
		if k := strings.TrimSpace(v["key"]); k != "" {
			s += ",key_file=" + quote(ExpandHome(k))
		} else {
			s += ",key_use_agent=true"
		}
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		return config.Endpoint{Kind: kind, Spec: s + ":" + p}, nil
	case "smb":
		ob, err := secret.Encrypt(v["pass"])
		if err != nil {
			return config.Endpoint{}, fmt.Errorf("encrypt password: %w", err)
		}
		s := ":smb,host=" + quote(v["host"]) + ",user=" + quote(v["user"]) + ",pass=" + quote(ob)
		if d := strings.TrimSpace(v["domain"]); d != "" {
			s += ",domain=" + quote(d)
		}
		return config.Endpoint{Kind: kind, Spec: s + ":" + strings.TrimPrefix(p, "/")}, nil
	case "remote":
		if !strings.Contains(p, ":") {
			return config.Endpoint{}, fmt.Errorf("expected remote:path, e.g. mynas:backups")
		}
		return config.Endpoint{Kind: kind, Spec: p}, nil
	}
	return config.Endpoint{}, fmt.Errorf("unknown kind %q", kind)
}

func orDefault(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return strings.TrimSpace(v)
}

var passRe = regexp.MustCompile(`pass='[^']*'`)

// Display is a human-friendly, secret-free rendering of a spec.
func Display(e config.Endpoint) string {
	s := e.Spec
	if !strings.HasPrefix(s, ":") {
		return s
	}
	s = passRe.ReplaceAllString(s, "pass=***")
	host, user := field(s, "host"), field(s, "user")
	i := strings.LastIndex(s, "':")
	if i < 0 {
		i = strings.Index(s[1:], ":")
		if i < 0 {
			return s
		}
		i++
	} else {
		i++
	}
	path := s[i+1:]
	proto := strings.SplitN(s[1:], ",", 2)[0]
	if user != "" {
		return fmt.Sprintf("%s://%s@%s/%s", proto, user, host, strings.TrimPrefix(path, "/"))
	}
	return fmt.Sprintf("%s://%s/%s", proto, host, strings.TrimPrefix(path, "/"))
}

func field(s, key string) string {
	m := regexp.MustCompile(key + `='((?:[^']|'')*)'`).FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "''", "'")
}

func IsLocal(spec string) bool {
	return strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "~") || strings.HasPrefix(spec, ".")
}

// Overlap reports whether a and b are the same location or one is inside the other.
func Overlap(a, b string) bool {
	if IsLocal(a) && IsLocal(b) {
		a, b = resolve(a), resolve(b)
	}
	a, b = strings.TrimRight(a, "/")+"/", strings.TrimRight(b, "/")+"/"
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

// resolve cleans a local path and resolves symlinks of the deepest existing ancestor.
func resolve(p string) string {
	p = filepath.Clean(ExpandHome(p))
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

var volRe = regexp.MustCompile(`^(/Volumes/[^/]+|/media/[^/]+/[^/]+|/run/media/[^/]+/[^/]+)(/|$)`)

// UnmountedVolume returns the mount root when spec points inside a removable
// volume path whose mount point does not exist (drive unplugged).
func UnmountedVolume(spec string) (string, bool) {
	if !IsLocal(spec) {
		return "", false
	}
	m := volRe.FindStringSubmatch(resolve(spec))
	if m == nil {
		return "", false
	}
	if _, err := os.Stat(m[1]); err != nil {
		return m[1], true
	}
	return "", false
}

// VolumeRoot returns the mount point of the removable volume spec lives on
// (/Volumes/X, /media/user/X, ...) when it is currently mounted.
func VolumeRoot(spec string) (string, bool) {
	if !IsLocal(spec) {
		return "", false
	}
	m := volRe.FindStringSubmatch(resolve(spec))
	if m == nil {
		return "", false
	}
	if _, err := os.Stat(m[1]); err != nil {
		return "", false
	}
	return m[1], true
}

// Unmount asks the OS to unmount a volume. It fails (and changes nothing)
// while files on it are in use.
func Unmount(ctx context.Context, root string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.CommandContext(ctx, "diskutil", "unmount", root)
	} else {
		cmd = exec.CommandContext(ctx, "umount", root)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}
