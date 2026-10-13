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
)

// State is this desktop's pairing: the server and the device credentials it
// issued. It lives in its own file, apart from the notify CLI's config.json.
type State struct {
	URL         string `json:"url"`
	DeviceID    string `json:"device_id"`
	DeviceToken string `json:"device_token"`
	SealKey     string `json:"seal_key"`
	Name        string `json:"name"`
}

var errNotPaired = errors.New("not paired; run: notifyd pair LINK")

// statePath is $NOTIFYD_STATE, or notifyd.json in $XDG_CONFIG_HOME/notifier
// (default ~/.config/notifier).
func statePath() (string, error) {
	if p := os.Getenv("NOTIFYD_STATE"); p != "" {
		return p, nil
	}
	dir := os.Getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot locate the state file: %w", err)
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "notifier", "notifyd.json"), nil
}

// loadState reads path, refusing a file that other users can read: it holds
// the device token and the key that decrypts every message.
func loadState(path string) (State, error) {
	var st State
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, errNotPaired
	}
	if err != nil {
		return st, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return st, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return st, fmt.Errorf("state file %s holds credentials but other users can read it; run: chmod 600 %s", path, path)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&st); err != nil {
		return st, fmt.Errorf("state file %s: %w", path, err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return st, fmt.Errorf("state file %s: unexpected data after the JSON object", path)
	}
	if st.URL == "" || st.DeviceID == "" || st.DeviceToken == "" || st.SealKey == "" {
		return st, fmt.Errorf("state file %s is incomplete; pair again with: notifyd pair -force LINK", path)
	}
	return st, nil
}

// saveState writes st to path, readable only by its owner, atomically and
// durably, so a crash never leaves half a pairing behind.
func saveState(path string, st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), ".notifyd-*.json")
}

// writeFileAtomic replaces path with data, readable only by its owner: a
// temporary file (named by pattern) is synced, then renamed over it. A
// symlink is written through rather than replaced.
func writeFileAtomic(path string, data []byte, pattern string) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, pattern) // created 0600
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
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
		d.Sync() // best effort
		d.Close()
	}
	return nil
}
