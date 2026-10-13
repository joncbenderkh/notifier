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
	"syscall"
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

var errAlreadyRunning = errors.New("another notifyd is running with this state file (systemctl --user status notifyd)")

// lockRun keeps a second `notifyd run` of the same pairing from starting.
// The server keeps one connection per device, so a second daemon would
// replace the first, which then exits for good (exitReplaced). The lock,
// beside the state file (symlinks resolved, so an alias finds it too), is
// held until release or the process exits.
func lockRun(statePath string) (release func(), err error) {
	path, err := filepath.EvalSymlinks(statePath)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}
		return nil, err
	}
	return func() { f.Close() }, nil
}

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
		return st, fmt.Errorf("state file %s is incomplete; pair again with: notifyd pair --force LINK", path)
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
