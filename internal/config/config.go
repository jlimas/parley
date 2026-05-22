// Package config handles persistent CLI configuration: agent identity,
// server URL, and the last-seen cursor used to compute unread events.
//
// Stored at $XDG_CONFIG_HOME/parley/config.json (or the OS equivalent) with
// PARLEY_AGENT and PARLEY_SERVER env overrides applied by Resolve.
package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const DefaultServer = "http://localhost:8080"

type Config struct {
	Agent    string    `json:"agent,omitempty"`
	Operator string    `json:"operator,omitempty"` // human operator behind this agent
	Key      string    `json:"key,omitempty"`      // API key for authenticating to parleyd
	Server   string    `json:"server,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

// Dir returns the directory where the parley config lives for the current
// process. Honours $PARLEY_HOME for per-profile setups (so two agents on the
// same machine can keep separate identities and cursors); falls back to the
// OS-standard user config dir otherwise.
func Dir() (string, error) {
	if d := os.Getenv("PARLEY_HOME"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "parley"), nil
}

func Path() (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func Load() (Config, error) {
	cfg := Config{Server: DefaultServer}
	p, err := Path()
	if err != nil {
		return cfg, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Server == "" {
		cfg.Server = DefaultServer
	}
	return cfg, nil
}

func Save(c Config) error {
	p, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Resolve loads the config and applies PARLEY_AGENT / PARLEY_SERVER overrides.
func Resolve() (Config, error) {
	cfg, err := Load()
	if err != nil {
		return cfg, err
	}
	if v := os.Getenv("PARLEY_AGENT"); v != "" {
		cfg.Agent = v
	}
	if v := os.Getenv("PARLEY_SERVER"); v != "" {
		cfg.Server = v
	}
	if v := os.Getenv("PARLEY_KEY"); v != "" {
		cfg.Key = v
	}
	return cfg, nil
}

// AdvanceLastSeen moves the LastSeen cursor forward to t if t is strictly
// newer than the stored value. Persisted to disk on change. No-op otherwise.
//
// Env overrides do not apply here — we always touch the on-disk config so the
// cursor survives across shell sessions.
func AdvanceLastSeen(t time.Time) error {
	cfg, err := Load()
	if err != nil {
		return err
	}
	if !t.After(cfg.LastSeen) {
		return nil
	}
	cfg.LastSeen = t
	return Save(cfg)
}
