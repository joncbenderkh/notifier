// SPDX-License-Identifier: GPL-3.0-or-later

// notifyd receives notifier messages on a Linux desktop. It pairs with the
// server like a phone does, keeps a WebSocket open to it, decrypts each
// message, and shows it through the desktop's notification daemon
// (org.freedesktop.Notifications: dunst, mako, GNOME, KDE, ...).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// version is set by release builds with -ldflags "-X main.version=...".
var version = "0.1.0"

const (
	exitError = 1
	exitUsage = 2
	// The server no longer knows this device: restarting cannot help.
	exitUnpaired = 4
	// Another notifyd is connected as this device.
	exitReplaced = 5
)

const usage = `Usage:
  notifyd pair [-name NAME] [-force] LINK
  notifyd pair -server URL [-name NAME] [-force] CODE
  notifyd [run]
  notifyd test
  notifyd unpair
  notifyd version

Pair with the notifier://pair link (or the code) from ` + "`notify pair`" + `, then run
notifyd, usually as a systemd user service. The pairing is kept in
$XDG_CONFIG_HOME/notifier/notifyd.json (override with NOTIFYD_STATE).
`

type app struct {
	stdout, stderr io.Writer
	statePath      string
	http           *http.Client
	desktop        func(context.Context) (Desktop, error)
	hostname       func() (string, error)
	signals        func() (context.Context, context.CancelFunc)
}

func main() {
	a := &app{
		stdout:   os.Stdout,
		stderr:   os.Stderr,
		http:     &http.Client{CheckRedirect: refuseRedirect},
		desktop:  connectDesktop,
		hostname: os.Hostname,
		signals: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
	}
	os.Exit(a.run(os.Args[1:]))
}

// refuseRedirect keeps the device token from following a redirect elsewhere.
func refuseRedirect(*http.Request, []*http.Request) error {
	return errors.New("the server answered with a redirect; check its URL")
}

func (a *app) run(args []string) int {
	if a.statePath == "" {
		p, err := statePath()
		if err != nil {
			fmt.Fprintln(a.stderr, "notifyd:", err)
			return exitError
		}
		a.statePath = p
	}
	cmd := "run"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "pair":
		return a.pair(args)
	case "run":
		return a.listen(args)
	case "test":
		return a.test(args)
	case "unpair":
		return a.unpair(args)
	case "version":
		fmt.Fprintln(a.stdout, "notifyd", version)
		return 0
	case "help", "-h", "-help", "--help":
		fmt.Fprint(a.stdout, usage)
		return 0
	}
	fmt.Fprint(a.stderr, usage)
	return exitUsage
}

func (a *app) logf(format string, args ...any) {
	fmt.Fprintf(a.stderr, "notifyd: "+format+"\n", args...)
}

func (a *app) fail(err error) int {
	fmt.Fprintln(a.stderr, "notifyd:", err)
	return exitError
}

// noArgs rejects arguments for commands that take none.
func (a *app) noArgs(cmd string, args []string) bool {
	if len(args) == 0 {
		return true
	}
	fmt.Fprintf(a.stderr, "notifyd: %s takes no arguments\n", cmd)
	return false
}

func (a *app) pair(args []string) int {
	fs := flag.NewFlagSet("pair", flag.ContinueOnError)
	fs.SetOutput(a.stderr)
	server := fs.String("server", "", "server URL, when pairing with a bare `code`")
	name := fs.String("name", "", "device `name` (default: this machine's hostname)")
	force := fs.Bool("force", false, "replace an existing pairing, unpairing it first")
	fs.Usage = func() { fmt.Fprint(a.stderr, usage) }
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return exitUsage
	}

	var srv, code string
	var err error
	if arg := fs.Arg(0); strings.Contains(arg, "://") {
		if *server != "" {
			fmt.Fprintln(a.stderr, "notifyd: -server is only for a bare code; the link names its server")
			return exitUsage
		}
		srv, code, err = parsePairLink(arg)
	} else {
		if *server == "" {
			fmt.Fprintln(a.stderr, "notifyd: give the notifier://pair link, or -server with the code")
			return exitUsage
		}
		srv, err = origin(*server)
		code = arg
	}
	if err != nil {
		fmt.Fprintln(a.stderr, "notifyd:", err)
		return exitUsage
	}

	old, err := loadState(a.statePath)
	hadOld := err == nil
	switch {
	case hadOld && !*force:
		return a.fail(fmt.Errorf("already paired as %q with %s; run `notifyd unpair` first, or pass -force", old.Name, old.URL))
	case !hadOld && !errors.Is(err, errNotPaired) && !*force:
		return a.fail(err)
	}

	deviceName := *name
	if deviceName == "" {
		if deviceName, err = a.hostname(); err != nil || deviceName == "" {
			return a.fail(errors.New("cannot read this machine's hostname; pass -name"))
		}
	}
	var reg struct {
		DeviceID    string `json:"device_id"`
		DeviceToken string `json:"device_token"`
		SealKey     string `json:"seal_key"`
	}
	req := map[string]string{"code": code, "name": deviceName, "transport": "ws"}
	if err := a.call(http.MethodPost, srv+"/v1/devices", "", req, &reg); err != nil {
		return a.fail(fmt.Errorf("pairing with %s failed: %w", srv, err))
	}
	st := State{URL: srv, DeviceID: reg.DeviceID, DeviceToken: reg.DeviceToken, SealKey: reg.SealKey, Name: deviceName}
	if err := saveState(a.statePath, st); err != nil {
		return a.fail(fmt.Errorf("paired as %s but could not save it: %w", reg.DeviceID, err))
	}
	// Only now that the new pairing is saved: a bad code must not cost the old one.
	if hadOld {
		if err := a.call(http.MethodDelete, old.URL+"/v1/device", old.DeviceToken, nil, nil); err != nil && !isUnauthorized(err) {
			a.logf("could not unpair the previous device %s (%v); remove it with DELETE /v1/devices/%s", old.DeviceID, err, old.DeviceID)
		}
	}
	fmt.Fprintf(a.stdout, "Paired with %s as %q (%s).\nStart it with: systemctl --user enable --now notifyd\n", srv, deviceName, reg.DeviceID)
	return 0
}

func (a *app) listen(args []string) int {
	if !a.noArgs("run", args) {
		return exitUsage
	}
	st, err := loadState(a.statePath)
	if errors.Is(err, errNotPaired) {
		// Nothing to run until it is paired: don't let systemd retry.
		a.logf("%v", err)
		return exitUnpaired
	}
	if err != nil {
		return a.fail(err)
	}
	ctx, stop := a.signals()
	defer stop()
	desktop, err := a.desktop(ctx)
	if err != nil {
		return a.fail(fmt.Errorf("cannot reach the desktop's notification service: %w", err))
	}
	err = newStreamer(st, desktop, a.http, a.logf).run(ctx)
	switch {
	case errors.Is(err, errUnpaired):
		a.logf("%v (notifyd pair -force LINK)", err)
		return exitUnpaired
	case errors.Is(err, errReplaced):
		a.logf("%v; stopping", err)
		return exitReplaced
	case err != nil:
		return a.fail(err)
	}
	return 0
}

func (a *app) test(args []string) int {
	if !a.noArgs("test", args) {
		return exitUsage
	}
	st, err := loadState(a.statePath)
	if err != nil {
		return a.fail(err)
	}
	var result struct {
		Deliveries []struct {
			Status string `json:"status"`
		} `json:"deliveries"`
	}
	if err := a.call(http.MethodPost, st.URL+"/v1/device/test", st.DeviceToken, nil, &result); err != nil {
		return a.fail(err)
	}
	status := "unknown"
	if len(result.Deliveries) > 0 {
		status = result.Deliveries[0].Status
	}
	switch status {
	case "sent":
		fmt.Fprintln(a.stdout, "Sent a test notification.")
	case "queued":
		fmt.Fprintln(a.stdout, "Queued a test notification: notifyd is not connected, so it shows once notifyd runs.")
	default:
		return a.fail(fmt.Errorf("test notification: %s", status))
	}
	return 0
}

func (a *app) unpair(args []string) int {
	if !a.noArgs("unpair", args) {
		return exitUsage
	}
	st, err := loadState(a.statePath)
	if err != nil {
		return a.fail(err)
	}
	// Already unpaired elsewhere: only the local file is left to remove.
	if err := a.call(http.MethodDelete, st.URL+"/v1/device", st.DeviceToken, nil, nil); err != nil && !isUnauthorized(err) {
		return a.fail(err)
	}
	if err := os.Remove(a.statePath); err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.stdout, "Unpaired.")
	return 0
}

// apiError is an error answer from the server.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message) }

func isUnauthorized(err error) bool {
	var apiErr *apiError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized
}

// call sends one API request with an optional bearer token and JSON body,
// decoding a JSON answer into out when it is non-nil.
func (a *app) call(method, url, token string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) != nil || e.Error == "" {
			e.Error = http.StatusText(resp.StatusCode)
		}
		return &apiError{Status: resp.StatusCode, Message: e.Error}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}
