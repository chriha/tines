// Package config defines the global tines configuration: every backup
// stream, its source, its targets and its individual settings. The file is
// plain YAML so it can double as a backup of the whole setup.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const Version = 1

// Endpoint is a place data lives: a local dir, SFTP server, SMB share or any
// rclone remote. Spec is an rclone path (e.g. "/Volumes/SSD/photos",
// ":sftp,host='h':/data" or "nas:share/dir").
type Endpoint struct {
	Kind string `yaml:"kind"`
	Spec string `yaml:"spec"`
}

// Settings are per-stream. New streams copy Config.Defaults.
type Settings struct {
	Excludes           []string `yaml:"excludes,omitempty"`
	BandwidthLimit     string   `yaml:"bandwidth_limit,omitempty"` // rclone syntax, e.g. "10M"
	Checksum           bool     `yaml:"checksum"`                  // compare by hash instead of size+mtime
	Transfers          int      `yaml:"transfers,omitempty"`
	TrashRetentionDays int      `yaml:"trash_retention_days"` // 0 = keep forever
	MaxDeletePercent   int      `yaml:"max_delete_percent"`   // tripwire
	VerifyAfter        bool     `yaml:"verify_after"`
	MirrorConfig       bool     `yaml:"mirror_config"`            // copy this config into each target
	NotifyCommand      string   `yaml:"notify_command,omitempty"` // run on failure; env TINES_STREAM/TINES_STATUS/TINES_MESSAGE
	Schedule           string   `yaml:"schedule,omitempty"`       // cron expression, informational (see `tines schedule`)
}

// Device types a target can be, shown in the Targets view.
const (
	TypeMicroSD = "microSD"
	TypeHDD     = "HDD"
	TypeSSD     = "SSD"
	TypeUSB     = "USB stick"
	TypeNAS     = "NAS"
	TypeCloud   = "Cloud"
	TypeOther   = "Other"
)

var TargetTypes = []string{TypeSSD, TypeHDD, TypeUSB, TypeMicroSD, TypeNAS, TypeCloud, TypeOther}

// Target is a registered backup destination. Targets live in Config.Targets
// and streams reference them by ID. Within a loaded Config, Stream.Targets
// holds resolved copies; Save derives the persisted ID lists from them.
type Target struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Type     string `yaml:"type,omitempty"`
	Endpoint `yaml:",inline"`
}

// DisplayType never returns an empty string.
func (t Target) DisplayType() string {
	if t.Type == "" {
		return TypeOther
	}
	return t.Type
}

type Stream struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Source   Endpoint `yaml:"source"`
	Targets  []Target `yaml:"-"`
	Settings Settings `yaml:"settings"`
}

type Config struct {
	Version  int      `yaml:"version"`
	Defaults Settings `yaml:"defaults"`
	Targets  []Target `yaml:"targets"`
	Streams  []Stream `yaml:"streams"`
}

// ---- on-disk form -----------------------------------------------------

// targetEntry is one item of a stream's "targets" list: either a target ID
// (current format) or an inline target mapping (version-1 configs).
type targetEntry struct {
	ID     string
	Legacy *Target
}

func (e *targetEntry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		e.ID = n.Value
		return nil
	}
	var t Target
	if err := n.Decode(&t); err != nil {
		return err
	}
	e.ID, e.Legacy = t.ID, &t
	return nil
}

type diskStream struct {
	ID       string        `yaml:"id"`
	Name     string        `yaml:"name"`
	Source   Endpoint      `yaml:"source"`
	Targets  []targetEntry `yaml:"targets"`
	Settings Settings      `yaml:"settings"`
}

type diskConfig struct {
	Version  int          `yaml:"version"`
	Defaults Settings     `yaml:"defaults"`
	Targets  []Target     `yaml:"targets"`
	Streams  []diskStream `yaml:"streams"`
}

type outStream struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Source   Endpoint `yaml:"source"`
	Targets  []string `yaml:"targets"`
	Settings Settings `yaml:"settings"`
}

type outConfig struct {
	Version  int         `yaml:"version"`
	Defaults Settings    `yaml:"defaults"`
	Targets  []Target    `yaml:"targets"`
	Streams  []outStream `yaml:"streams"`
}

func DefaultSettings() Settings {
	return Settings{
		TrashRetentionDays: 90,
		MaxDeletePercent:   20,
		VerifyAfter:        false,
		MirrorConfig:       true,
		Transfers:          4,
		Excludes:           []string{".DS_Store", "Thumbs.db"},
	}
}

func New() *Config { return &Config{Version: Version, Defaults: DefaultSettings()} }

func NewID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Path returns the config file location ($TINES_CONFIG or ~/.config/tines/config.yaml).
func Path() string {
	if p := os.Getenv("TINES_CONFIG"); p != "" {
		return p
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "tines", "config.yaml")
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "tines", "config.yaml")
}

// Load reads the config; a missing file yields an empty config.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	d := diskConfig{Defaults: DefaultSettings()}
	if err := yaml.Unmarshal(b, &d); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if d.Version > Version {
		return nil, fmt.Errorf("config version %d is newer than this tines understands (%d)", d.Version, Version)
	}
	c := &Config{Version: Version, Defaults: d.Defaults, Targets: d.Targets}
	for _, ds := range d.Streams {
		s := Stream{ID: ds.ID, Name: ds.Name, Source: ds.Source, Settings: ds.Settings}
		for _, e := range ds.Targets {
			if e.Legacy != nil {
				s.Targets = append(s.Targets, c.adoptLegacy(*e.Legacy))
				continue
			}
			t := c.FindTarget(e.ID)
			if t == nil {
				return nil, fmt.Errorf("stream %q uses unknown target %q", ds.Name, e.ID)
			}
			s.Targets = append(s.Targets, *t)
		}
		c.Streams = append(c.Streams, s)
	}
	return c, c.Validate()
}

// adoptLegacy moves a version-1 inline target into the registry and returns
// the registered copy. A target already registered under the same ID (the
// same drive listed in several streams) is reused; a name clash with a
// different target is resolved by numbering.
func (c *Config) adoptLegacy(t Target) Target {
	if r := c.FindTarget(t.ID); r != nil {
		return *r
	}
	if t.Type == "" {
		t.Type = GuessType(t.Endpoint)
	}
	base, n := t.Name, 1
	for c.targetNameTaken(t.Name, "") {
		n++
		t.Name = fmt.Sprintf("%s (%d)", base, n)
	}
	c.Targets = append(c.Targets, t)
	return t
}

// GuessType picks a plausible device type for a migrated endpoint. A local
// folder could be anything, so it stays "Other" until the user says.
func GuessType(e Endpoint) string {
	if t := GuessTypeForKind(e.Kind); t != TypeSSD {
		return t
	}
	return TypeOther
}

// GuessTypeForKind is the preselected type when adding a target of that
// endpoint kind.
func GuessTypeForKind(kind string) string {
	switch kind {
	case "smb", "sftp":
		return TypeNAS
	case "remote":
		return TypeCloud
	}
	return TypeSSD
}

// Save writes atomically (temp file + rename), keeps the previous file as
// <path>.bak, and uses 0600 because specs can contain obscured passwords.
func (c *Config) Save(path string) error {
	c.Normalize()
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := c.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o600)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Normalize registers every target used by a stream (the in-memory copies in
// Stream.Targets are the source of truth for membership) and refreshes those
// copies from the registry, so renaming or retyping a registered target
// shows up everywhere.
func (c *Config) Normalize() {
	for i := range c.Streams {
		for _, t := range c.Streams[i].Targets {
			if c.FindTarget(t.ID) == nil && t.ID != "" {
				c.Targets = append(c.Targets, t)
			}
		}
	}
	c.Resolve()
}

// Resolve refreshes Stream.Targets from the registry.
func (c *Config) Resolve() {
	for i := range c.Streams {
		for j, t := range c.Streams[i].Targets {
			if r := c.FindTarget(t.ID); r != nil {
				c.Streams[i].Targets[j] = *r
			}
		}
	}
}

func (c *Config) Marshal() ([]byte, error) {
	c.Normalize()
	o := outConfig{Version: c.Version, Defaults: c.Defaults, Targets: c.Targets}
	for _, s := range c.Streams {
		ost := outStream{ID: s.ID, Name: s.Name, Source: s.Source, Settings: s.Settings, Targets: []string{}}
		for _, t := range s.Targets {
			ost.Targets = append(ost.Targets, t.ID)
		}
		o.Streams = append(o.Streams, ost)
	}
	b, err := yaml.Marshal(o)
	if err != nil {
		return nil, err
	}
	hdr := "# tines configuration - safe to keep as a backup of your whole setup.\n# Contains connection details; keep it private (file mode 0600).\n"
	return append([]byte(hdr), b...), nil
}

func (c *Config) Validate() error {
	tnames := map[string]bool{}
	tids := map[string]bool{}
	for _, t := range c.Targets {
		if t.ID == "" || tids[t.ID] {
			return fmt.Errorf("target %q has a missing or duplicate id", t.Name)
		}
		tids[t.ID] = true
		if strings.TrimSpace(t.Name) == "" {
			return errors.New("a target has no name")
		}
		if t.Spec == "" {
			return fmt.Errorf("target %q has no location", t.Name)
		}
		k := strings.ToLower(t.Name)
		if tnames[k] {
			return fmt.Errorf("duplicate target name %q", t.Name)
		}
		tnames[k] = true
	}
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, s := range c.Streams {
		if strings.TrimSpace(s.Name) == "" {
			return errors.New("a stream has no name")
		}
		k := strings.ToLower(s.Name)
		if names[k] {
			return fmt.Errorf("duplicate stream name %q", s.Name)
		}
		names[k] = true
		if s.ID == "" || ids[s.ID] {
			return fmt.Errorf("stream %q has a missing or duplicate id", s.Name)
		}
		ids[s.ID] = true
		if s.Source.Spec == "" {
			return fmt.Errorf("stream %q has no source", s.Name)
		}
		if len(s.Targets) == 0 {
			return fmt.Errorf("stream %q has no targets", s.Name)
		}
		tn := map[string]bool{}
		for _, t := range s.Targets {
			if t.Spec == "" || t.ID == "" {
				return fmt.Errorf("stream %q has an incomplete target", s.Name)
			}
			if tn[strings.ToLower(t.Name)] {
				return fmt.Errorf("stream %q: duplicate target name %q", s.Name, t.Name)
			}
			tn[strings.ToLower(t.Name)] = true
		}
	}
	return nil
}

func (c *Config) Find(name string) *Stream {
	for i := range c.Streams {
		if strings.EqualFold(c.Streams[i].Name, name) || c.Streams[i].ID == name {
			return &c.Streams[i]
		}
	}
	return nil
}

func (c *Config) Remove(id string) {
	out := c.Streams[:0]
	for _, s := range c.Streams {
		if s.ID != id {
			out = append(out, s)
		}
	}
	c.Streams = out
}

func (c *Config) FindTarget(id string) *Target {
	for i := range c.Targets {
		if c.Targets[i].ID == id {
			return &c.Targets[i]
		}
	}
	return nil
}

func (c *Config) targetNameTaken(name, exceptID string) bool {
	for _, t := range c.Targets {
		if t.ID != exceptID && strings.EqualFold(t.Name, name) {
			return true
		}
	}
	return false
}

// TargetNameTaken reports whether another registered target uses name.
func (c *Config) TargetNameTaken(name, exceptID string) bool {
	return c.targetNameTaken(name, exceptID)
}

// StreamsUsing lists the names of streams that write to the target.
func (c *Config) StreamsUsing(targetID string) []string {
	var out []string
	for _, s := range c.Streams {
		for _, t := range s.Targets {
			if t.ID == targetID {
				out = append(out, s.Name)
				break
			}
		}
	}
	return out
}

// RemoveTarget forgets a registered target. It refuses while a stream still
// uses it. Nothing on the target itself is touched.
func (c *Config) RemoveTarget(id string) error {
	if used := c.StreamsUsing(id); len(used) > 0 {
		return fmt.Errorf("still used by stream(s): %s", strings.Join(used, ", "))
	}
	out := c.Targets[:0]
	for _, t := range c.Targets {
		if t.ID != id {
			out = append(out, t)
		}
	}
	c.Targets = out
	return nil
}
