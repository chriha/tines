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

type Target struct {
	ID       string `yaml:"id"`
	Name     string `yaml:"name"`
	Endpoint `yaml:",inline"`
}

type Stream struct {
	ID       string   `yaml:"id"`
	Name     string   `yaml:"name"`
	Source   Endpoint `yaml:"source"`
	Targets  []Target `yaml:"targets"`
	Settings Settings `yaml:"settings"`
}

type Config struct {
	Version  int      `yaml:"version"`
	Defaults Settings `yaml:"defaults"`
	Streams  []Stream `yaml:"streams"`
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
	c := New()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if c.Version > Version {
		return nil, fmt.Errorf("config version %d is newer than this tines understands (%d)", c.Version, Version)
	}
	c.Version = Version
	return c, c.Validate()
}

// Save writes atomically (temp file + rename), keeps the previous file as
// <path>.bak, and uses 0600 because specs can contain obscured passwords.
func (c *Config) Save(path string) error {
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

func (c *Config) Marshal() ([]byte, error) {
	b, err := yaml.Marshal(c)
	if err != nil {
		return nil, err
	}
	hdr := "# tines configuration - safe to keep as a backup of your whole setup.\n# Contains connection details; keep it private (file mode 0600).\n"
	return append([]byte(hdr), b...), nil
}

func (c *Config) Validate() error {
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
