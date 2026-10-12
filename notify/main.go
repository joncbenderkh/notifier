// SPDX-License-Identifier: GPL-3.0-or-later

// Command notify sends a notification to paired phones through a notifier
// server.
//
//	notify [-to PHONE]... [-high] TITLE [BODY | -]
//	notify devices
//	notify pair
//	notify version
//
// PHONE is a device name (case-insensitive) or ID; without -to the
// notification goes to every paired phone. A BODY of "-" is read from
// standard input. Flags must come before TITLE: everything from TITLE on is
// message text, so a body from a variable can never act as a flag (such as
// -url). Use "--" before a title that starts with "-". The server and
// credential come from NOTIFIER_URL and NOTIFIER_API_KEY, or from -url and
// -key. `notify pair` instead uses the admin token from NOTIFIER_ADMIN_TOKEN
// and prints a QR code for the phone app to scan.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// version is overridden by release builds via -ldflags.
var version = "0.1.0"

const (
	exitError       = 1
	exitUsage       = 2
	exitUndelivered = 3

	// maxTitleBytes and maxBodyBytes mirror the server's limits.
	maxTitleBytes = 256
	maxBodyBytes  = 2048
	// stdinTailBytes bounds memory when a large stream is piped in; only its
	// end can ever be sent.
	stdinTailBytes = 8 << 10
	truncated      = "…"
)

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }

func (m *multiFlag) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*m = append(*m, part)
		}
	}
	return nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil))
}

// run executes the command; transport is nil outside tests.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, transport http.RoundTripper) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var to multiFlag
	fs.Var(&to, "to", "phone `name or ID` to notify; repeatable or comma-separated (default: all paired phones)")
	high := fs.Bool("high", false, "send as high priority (heads-up, may wake the phone)")
	baseURL := fs.String("url", os.Getenv("NOTIFIER_URL"), "notifier server `URL` (default $NOTIFIER_URL)")
	apiKey := fs.String("key", os.Getenv("NOTIFIER_API_KEY"), "API `key` (default $NOTIFIER_API_KEY)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout, counted from when the request starts")
	fs.Usage = func() {
		fmt.Fprint(stderr, "Usage:\n  notify [flags] TITLE [BODY | -]\n  notify devices\n  notify pair\n  notify version\n\n"+
			"Flags must come before TITLE.\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	// After "--", "devices", "pair" and "version" are a title like any other.
	subcommands := !endedByDoubleDash(fs, args, rest)
	if subcommands && len(rest) == 1 && rest[0] == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}
	pairing := subcommands && len(rest) == 1 && rest[0] == "pair"
	token, tokenSource := *apiKey, "NOTIFIER_API_KEY (or pass -key)"
	if pairing {
		token, tokenSource = os.Getenv("NOTIFIER_ADMIN_TOKEN"), "NOTIFIER_ADMIN_TOKEN"
	}
	if *baseURL == "" || token == "" {
		fmt.Fprintf(stderr, "notify: set NOTIFIER_URL (or pass -url) and %s\n", tokenSource)
		return exitUsage
	}
	if err := checkURL(*baseURL); err != nil {
		fmt.Fprintln(stderr, "notify:", err)
		return exitUsage
	}
	client := &Client{
		BaseURL: *baseURL,
		Token:   token,
		HTTP:    &http.Client{Transport: transport, CheckRedirect: refuseRedirect},
	}

	if pairing {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		return pair(ctx, client, stdout, stderr)
	}

	if subcommands && len(rest) == 1 && rest[0] == "devices" {
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		return listDevices(ctx, client, stdout, stderr)
	}
	if len(rest) < 1 || len(rest) > 2 {
		fs.Usage()
		return exitUsage
	}

	n := Notification{Title: fitTitle(rest[0])}
	if *high {
		n.Priority = "high"
	}
	if len(rest) == 2 {
		body, dropped := rest[1], false
		if body == "-" {
			// Read before the timeout starts: the producer may be a long job.
			b, more, err := readTail(stdin, stdinTailBytes)
			if err != nil {
				return fail(stderr, err)
			}
			body, dropped = strings.TrimRight(string(b), "\r\n"), more
		}
		n.Body = fitBody(body, dropped)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if len(to) > 0 {
		devices, err := client.Devices(ctx)
		if err != nil {
			return fail(stderr, err)
		}
		if n.DeviceIDs, err = ResolveDevices(devices, to); errors.Is(err, ErrNoSuchDevice) {
			fmt.Fprintln(stderr, "notify:", err)
			return exitUndelivered
		} else if err != nil {
			return fail(stderr, err)
		}
	}

	result, err := client.Notify(ctx, n)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Message == noDevicesMessage {
		// No paired (or matching) phone: nothing was delivered.
		fmt.Fprintln(stderr, "notify:", apiErr.Message)
		return exitUndelivered
	}
	if err != nil {
		return fail(stderr, err)
	}
	code := 0
	reported := make(map[string]bool, len(result.Deliveries))
	for _, d := range result.Deliveries {
		reported[d.DeviceID] = true
		line := fmt.Sprintf("%s: %s", d.DeviceID, d.Status)
		if d.Error != "" {
			line += " (" + d.Error + ")"
		}
		if d.Status != "sent" {
			code = exitUndelivered
			fmt.Fprintln(stderr, line)
		} else {
			fmt.Fprintln(stdout, line)
		}
	}
	// A phone unpaired between name resolution and sending is dropped by the
	// server without a delivery entry.
	for _, id := range n.DeviceIDs {
		if !reported[id] {
			code = exitUndelivered
			fmt.Fprintf(stderr, "%s: no longer paired\n", id)
		}
	}
	return code
}

// endedByDoubleDash reports whether flag parsing stopped at a "--"
// terminator, as opposed to "--" being the value of a flag such as -to.
func endedByDoubleDash(fs *flag.FlagSet, args, rest []string) bool {
	consumed := len(args) - len(rest)
	if consumed == 0 || args[consumed-1] != "--" {
		return false
	}
	if consumed < 2 {
		return true
	}
	prev := strings.TrimLeft(args[consumed-2], "-")
	if strings.Contains(prev, "=") || !strings.HasPrefix(args[consumed-2], "-") {
		return true
	}
	f := fs.Lookup(prev)
	if f == nil {
		return true
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && boolFlag.IsBoolFlag()
}

// refuseRedirect stops the client from following redirects: Go would resend
// the Authorization header to a same-host http:// target, leaking the key.
func refuseRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("server redirected to %s; set NOTIFIER_URL to the final https URL", req.URL)
}

// checkURL refuses to send the API key in cleartext anywhere but loopback.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("invalid server URL %q", raw)
	}
	if u.Scheme == "https" {
		return nil
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); u.Scheme == "http" && (host == "localhost" || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return fmt.Errorf("server URL must use https (got %q)", raw)
}

// noDevicesMessage is the server's 404 when no paired phone matches, as
// opposed to a 404 from a wrong server URL.
const noDevicesMessage = "no matching paired devices"

// readTail reads r to the end, keeping only its last max bytes; more reports
// whether anything earlier was discarded.
func readTail(r io.Reader, max int) (tail []byte, more bool, err error) {
	buf := make([]byte, 0, 2*max)
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if len(buf) > max {
			buf, more = append(buf[:0], buf[len(buf)-max:]...), true
		}
		if errors.Is(err, io.EOF) {
			return buf, more, nil
		}
		if err != nil {
			return nil, more, err
		}
	}
}

// fitTitle makes s valid UTF-8 and shortens it to the server's limit,
// keeping the start and marking the cut.
func fitTitle(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= maxTitleBytes {
		return s
	}
	end := maxTitleBytes - len(truncated)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + truncated
}

// fitBody makes s valid UTF-8 (JSON encoding would otherwise expand each
// invalid byte to three) and shortens it to the server's limit, keeping the
// end — for piped logs that is where the error usually is — and marking any
// cut, including one made earlier while reading.
func fitBody(s string, dropped bool) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if !dropped && len(s) <= maxBodyBytes {
		return s
	}
	start := max(0, len(s)-(maxBodyBytes-len(truncated)))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return truncated + s[start:]
}

func listDevices(ctx context.Context, client *Client, stdout, stderr io.Writer) int {
	devices, err := client.Devices(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNAME\tREACHABLE")
	for _, d := range devices {
		fmt.Fprintf(w, "%s\t%s\t%t\n", d.ID, d.Name, d.Reachable)
	}
	w.Flush()
	return 0
}

func fail(stderr io.Writer, err error) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		fmt.Fprintln(stderr, "notify: the server rejected the API key")
		return exitError
	}
	fmt.Fprintln(stderr, "notify:", err)
	return exitError
}
