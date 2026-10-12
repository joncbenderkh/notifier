// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// Config is the JSON file at configPath: the server, credentials, and
// default targets. Flags override environment variables, which override it.
type Config struct {
	URL      string   `json:"url,omitempty"`
	Token    string   `json:"token,omitempty"`
	APIKey   string   `json:"api_key,omitempty"`
	To       []string `json:"to,omitempty"`
	Channels []string `json:"channels,omitempty"`
	Priority string   `json:"priority,omitempty"`
}

// configPath is $NOTIFIER_CONFIG, or config.json in $XDG_CONFIG_HOME/notifier
// (default ~/.config/notifier), which the desktop daemon reads too.
func configPath() (string, error) {
	if p := os.Getenv("NOTIFIER_CONFIG"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the config file: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "notifier", "config.json"), nil
}

// loadConfig reads path; a missing file is an empty config. It refuses a
// file that holds credentials but is readable by other users.
func loadConfig(path string) (Config, error) {
	var cfg Config
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("config file %s: %w", path, err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return cfg, fmt.Errorf("config file %s: unexpected data after the JSON object", path)
	}
	if cfg.Priority != "" && cfg.Priority != "default" && cfg.Priority != "high" {
		return cfg, fmt.Errorf(`config file %s: priority must be "default" or "high"`, path)
	}
	if cfg.Token != "" || cfg.APIKey != "" {
		info, err := os.Stat(path)
		if err != nil {
			return cfg, err
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			return cfg, fmt.Errorf("config file %s holds credentials but other users can read it; run: chmod 600 %s", path, path)
		}
	}
	return cfg, nil
}

// saveConfig writes cfg to path, readable only by its owner. It replaces the
// file atomically and durably, so a failed write or a crash never leaves half
// a config (or a lost token) behind. A symlinked config (from a dotfile
// manager, say) is written through, not replaced.
func saveConfig(path string, cfg Config) error {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".config-*.json") // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		d.Sync() // best effort: not every platform syncs directories
		d.Close()
	}
	return nil
}
