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
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
	"github.com/joncbenderkh/notifier/notifyd/internal/sounds"
)

// version is set by release builds with -ldflags "-X main.version=...".
var version = "0.5.0"

const (
	exitError = 1
	exitUsage = 2
	// The server no longer knows this device: restarting cannot help.
	exitUnpaired = 4
	// Another notifyd is connected as this device.
	exitReplaced = 5
)

type app struct {
	stdout, stderr io.Writer
	statePath      string
	historyPath    string
	now            func() time.Time
	http           *http.Client
	desktop        func(context.Context, AnswerFunc, ReceiptFunc, map[string]string) (Desktop, error)
	hostname       func() (string, error)
	signals        func() (context.Context, context.CancelFunc)
	// noSound keeps run from giving notifications a sound.
	noSound bool
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

// run executes the command line and returns the exit code.
func (a *app) run(args []string) int {
	root := a.newRoot()
	if cmd, rest, err := root.Find(args); err == nil {
		if f := oldStyleFlag(cmd, rest); f != "" {
			fmt.Fprintf(a.stderr, "notifyd: unknown flag -%s; long flags take two dashes since 0.4.0: --%s\n", f, f)
			return exitUsage
		}
	}
	root.SetArgs(args)
	root.SetOut(a.stdout)
	root.SetErr(a.stderr)
	cmd, err := root.ExecuteC()
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	// Cobra's own: an unknown command or flag, or a wrong argument count.
	fmt.Fprintln(a.stderr, "notifyd:", err)
	fmt.Fprintf(a.stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	return exitUsage
}

// paths resolves the state and history files, unless set (by tests).
func (a *app) paths() int {
	if a.statePath == "" {
		p, err := statePath()
		if err != nil {
			fmt.Fprintln(a.stderr, "notifyd:", err)
			return exitError
		}
		a.statePath = p
	}
	if a.historyPath == "" {
		p, err := historyPath()
		if err != nil {
			fmt.Fprintln(a.stderr, "notifyd:", err)
			return exitError
		}
		a.historyPath = p
	}
	if a.now == nil {
		a.now = time.Now
	}
	return 0
}

func (a *app) logf(format string, args ...any) {
	fmt.Fprintf(a.stderr, "notifyd: "+format+"\n", args...)
}

func (a *app) fail(err error) int {
	fmt.Fprintln(a.stderr, "notifyd:", err)
	return exitError
}

// pairOptions are the flags of `notifyd pair`.
type pairOptions struct {
	server, name string
	force        bool
}

func (a *app) pair(arg string, o pairOptions) int {
	var srv, code string
	var err error
	if strings.Contains(arg, "://") {
		if o.server != "" {
			fmt.Fprintln(a.stderr, "notifyd: --server is only for a bare code; the link names its server")
			return exitUsage
		}
		srv, code, err = parsePairLink(arg)
	} else {
		if o.server == "" {
			fmt.Fprintln(a.stderr, "notifyd: give the notifier://pair link, or --server with the code")
			return exitUsage
		}
		srv, err = origin(o.server)
		code = arg
	}
	if err != nil {
		fmt.Fprintln(a.stderr, "notifyd:", err)
		return exitUsage
	}

	old, err := loadState(a.statePath)
	hadOld := err == nil
	switch {
	case hadOld && !o.force:
		return a.fail(fmt.Errorf("already paired as %q with %s; run `notifyd unpair` first, or pass --force", old.Name, old.URL))
	case !hadOld && !errors.Is(err, errNotPaired) && !o.force:
		return a.fail(err)
	}

	deviceName := o.name
	if deviceName == "" {
		if deviceName, err = a.hostname(); err != nil || deviceName == "" {
			return a.fail(errors.New("cannot read this machine's hostname; pass --name"))
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
	// One step under the history lock: a running daemon of the old pairing
	// checks that pairing under it before recording anything more.
	err = withHistoryLock(a.historyPath, func() error {
		if err := saveState(a.statePath, st); err != nil {
			return fmt.Errorf("paired as %s but could not save it: %w", reg.DeviceID, err)
		}
		// The new pairing may be another account: its history starts empty.
		if err := clearHistory(a.historyPath); err != nil {
			a.logf("could not clear the previous history: %v", err)
		}
		return nil
	})
	if err != nil {
		return a.fail(err)
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

func (a *app) listen() int {
	st, err := loadState(a.statePath)
	if errors.Is(err, errNotPaired) {
		// Nothing to run until it is paired: don't let systemd retry.
		a.logf("%v", err)
		return exitUnpaired
	}
	if err != nil {
		return a.fail(err)
	}
	release, err := lockRun(a.statePath)
	if err != nil {
		return a.fail(err)
	}
	defer release()
	ctx, stop := a.signals()
	defer stop()
	posting := make(chan struct{}, maxReceiptPosts)
	desktop, err := a.desktop(ctx, func(id string, q histfile.Reply, option string) string {
		outcome, _ := a.answer(st, id, q, option)
		return outcome
	}, func(id, state string) {
		// A backlog of queued messages must not open a connection each;
		// a click, at human pace, need not wait behind them.
		if state == receiptShown {
			posting <- struct{}{}
			defer func() { <-posting }()
		}
		a.receipt(st, id, state)
	}, a.sounds())
	if err != nil {
		return a.fail(fmt.Errorf("cannot reach the desktop's notification service: %w", err))
	}
	s := newStreamer(st, desktop, a.http, a.logf)
	s.history = newHistory(a.historyPath, a.now)
	s.history.current = func() bool {
		saved, err := loadState(a.statePath)
		return err == nil && saved.DeviceID == st.DeviceID
	}
	err = s.run(ctx)
	switch {
	case errors.Is(err, errUnpaired):
		a.logf("%v (notifyd pair --force LINK)", err)
		return exitUnpaired
	case errors.Is(err, errReplaced):
		a.logf("%v; stopping", err)
		return exitReplaced
	case err != nil:
		return a.fail(err)
	}
	return 0
}

func (a *app) test() int {
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

func (a *app) unpair() int {
	st, err := loadState(a.statePath)
	if err != nil {
		return a.fail(err)
	}
	// Already unpaired elsewhere: only the local file is left to remove.
	if err := a.call(http.MethodDelete, st.URL+"/v1/device", st.DeviceToken, nil, nil); err != nil && !isUnauthorized(err) {
		return a.fail(err)
	}
	// The device is gone from the server now; a history that cannot be
	// cleared must not leave a pairing that can no longer be removed.
	err = withHistoryLock(a.historyPath, func() error {
		if err := clearHistory(a.historyPath); err != nil {
			a.logf("could not clear the history (notifyd history clear): %v", err)
		}
		return os.Remove(a.statePath)
	})
	if err != nil {
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

func (a *app) historyClear() int {
	if err := withHistoryLock(a.historyPath, func() error { return clearHistory(a.historyPath) }); err != nil {
		return a.fail(err)
	}
	fmt.Fprintln(a.stdout, "History cleared.")
	return 0
}

// historyDelete removes the entries with [ids]; an ID it does not hold
// fails, so a typo is not mistaken for success.
func (a *app) historyDelete(ids []string) int {
	var removed int
	err := withHistoryLock(a.historyPath, func() (err error) {
		removed, err = histfile.Delete(a.historyPath, a.now(), ids...)
		return err
	})
	if err != nil {
		return a.fail(err)
	}
	if want := len(slices.Compact(slices.Sorted(slices.Values(ids)))); removed < want {
		fmt.Fprintf(a.stderr, "notifyd: deleted %d of %d messages; the others are not in the history\n", removed, want)
		return 1
	}
	fmt.Fprintf(a.stdout, "Deleted %d.\n", removed)
	return 0
}

// history prints the [n] newest entries (all for 0).
func (a *app) history(n int, asJSON bool) int {
	if n < 0 {
		fmt.Fprintln(a.stderr, "notifyd: -n must be 0 or more")
		return exitUsage
	}
	entries, err := readHistory(a.historyPath, a.now())
	if err != nil {
		return a.fail(err)
	}
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	if asJSON {
		if entries == nil {
			entries = []historyEntry{}
		}
		raw, err := json.MarshalIndent(entries, "", "  ")
		if err != nil {
			return a.fail(err)
		}
		fmt.Fprintf(a.stdout, "%s\n", escapeInvisible(raw))
		return 0
	}
	if len(entries) == 0 {
		fmt.Fprintln(a.stdout, "Nothing received yet.")
		return 0
	}
	for i, e := range entries {
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		fmt.Fprint(a.stdout, formatEntry(e))
	}
	return 0
}

// formatEntry renders one entry for a terminal: a heading line, then the
// body indented. Senders control the text, so control characters (terminal
// escapes among them) are replaced.
func formatEntry(e historyEntry) string {
	const layout = "2006-01-02 15:04"
	var b strings.Builder
	b.WriteString(time.Unix(e.ReceivedAt, 0).Format(layout))
	if e.Channel != "" && !strings.HasPrefix(e.ChannelID, "ch_default_") {
		b.WriteString("  [" + printable(e.Channel, false) + "]")
	}
	b.WriteString("  " + printable(e.Title, false))
	if e.Priority == "high" {
		b.WriteString("  (high)")
	}
	// Queued or delayed on the way: say when it was sent.
	if e.ReceivedAt-e.SentAt > 60 {
		b.WriteString("  (sent " + time.Unix(e.SentAt, 0).Format(layout) + ")")
	}
	b.WriteString("\n")
	if e.Body != "" {
		for line := range strings.SplitSeq(printable(e.Body, true), "\n") {
			b.WriteString("    " + line + "\n")
		}
	}
	if q := e.Reply; q != nil {
		options := make([]string, len(q.Options))
		for i, o := range q.Options {
			options[i] = printable(o, false)
		}
		b.WriteString("    answers: " + strings.Join(options, " | ") + "  (notifyd reply " + e.ID + " ANSWER)\n")
	}
	return b.String()
}

var (
	printable = histfile.Printable
	invisible = histfile.Invisible
)

// escapeInvisible rewrites, as \uXXXX escapes, what encoding/json leaves
// raw but a terminal may act on: DEL, C1 controls, and format characters.
// Encoded JSON has no raw C0 controls left to worry about.
func escapeInvisible(raw []byte) []byte {
	var b bytes.Buffer
	for _, r := range string(raw) {
		if r < 0x7f || !invisible(r) {
			b.WriteRune(r)
			continue
		}
		for _, unit := range utf16.Encode([]rune{r}) {
			fmt.Fprintf(&b, "\\u%04x", unit)
		}
	}
	return b.Bytes()
}

// sounds writes notifier's sounds to $XDG_CACHE_HOME/notifier/sounds and
// returns their paths; none with --no-sound, or if they cannot be
// written, which costs only the sound.
func (a *app) sounds() map[string]string {
	if a.noSound {
		return nil
	}
	dir, err := os.UserCacheDir()
	if err == nil {
		var paths map[string]string
		if paths, err = sounds.Install(filepath.Join(dir, "notifier", "sounds")); err == nil {
			return paths
		}
	}
	a.logf("notifications will have no sound: %v", err)
	return nil
}
