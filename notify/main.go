// SPDX-License-Identifier: GPL-3.0-or-later

// Command notify sends a notification to paired phones through a notifier
// server, and manages the account behind them.
//
//	notify [-to PHONE]... [-c CHANNEL]... [-high] TITLE [BODY | -]
//	notify signup INVITE NAME
//	notify channels
//	notify pair
//
// PHONE is a device name (case-insensitive) or ID, CHANNEL one of the user's
// own channels; without either the notification goes to the default
// channel, which every paired phone subscribes to. A BODY of "-" is read
// from standard input. Flags must come before TITLE: everything from TITLE
// on is message text, so a body from a variable can never act as a flag
// (such as -url). Use "--" before a title that starts with "-" or is a
// subcommand name. The server and credentials come from flags, then
// NOTIFIER_URL, NOTIFIER_API_KEY and NOTIFIER_TOKEN, then the config file
// (see configPath). Sending uses an API key if one is set, else the user
// token; everything else needs the user token. Run with -h for the full list.
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
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// version is overridden by release builds via -ldflags.
var version = "0.2.0"

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

const usage = `Usage:
  notify [flags] TITLE [BODY | -]
  notify devices
  notify pair
  notify signup [-force] INVITE NAME
  notify invite
  notify config init [-force]
  notify keys
  notify key create [-c CHANNEL]... NAME
  notify key delete NAME|ID
  notify channels
  notify channel create|delete|invite|members|leave NAME
  notify channel join CODE
  notify channel remove NAME MEMBER
  notify heartbeats
  notify heartbeat create [-every 1d] [-grace 5m] [-c CHANNEL] NAME
  notify heartbeat delete NAME|ID
  notify hooks
  notify hook create [-c CHANNEL] FORMAT NAME
  notify hook delete NAME|ID
  notify version

Flags must come before TITLE or the subcommand.

Flags:
`

// subcommands are the words that start a command instead of a title,
// unless they come after "--". Those that take no arguments (false) count
// only alone, so `notify "$TITLE"` stays a message for any other title; the
// others need arguments, so `notify "$TITLE" "$BODY"` with such a title fails
// with a usage error instead of acting.
var subcommands = map[string]bool{
	"version": false, "invite": false, "devices": false, "pair": false, "keys": false, "channels": false,
	"heartbeats": false, "hooks": false,
	"config": true, "signup": true, "key": true, "channel": true, "heartbeat": true, "hook": true,
}

// isSubcommand reports whether rest starts a command rather than a message.
func isSubcommand(rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	takesArgs, ok := subcommands[rest[0]]
	return ok && (len(rest) == 1) != takesArgs
}

// cli holds what every command needs: resolved settings and the outputs.
type cli struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	transport      http.RoundTripper
	timeout        time.Duration
	configPath     string
	config         Config
	url            string
	// sendCred authenticates sending and `devices`: an API key or a user
	// token, whichever was given at the highest precedence level.
	sendCred, sendCredName string
	token                  string
	// givenToken and givenKey come from flags or the environment, not the
	// config file; `config init` saves them.
	givenToken, givenKey string
}

// run executes the command; transport is nil outside tests.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, transport http.RoundTripper) int {
	fs := flag.NewFlagSet("notify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var to, channels multiFlag
	fs.Var(&to, "to", "phone `name or ID` to notify; repeatable or comma-separated")
	fs.Var(&channels, "c", "own `channel` to publish to; repeatable or comma-separated (default: the config file's, else default)")
	high := fs.Bool("high", false, "send as high priority (heads-up, may wake the phone)")
	urlFlag := fs.String("url", "", "notifier server `URL` (default $NOTIFIER_URL, then the config file)")
	keyFlag := fs.String("key", "", "API `key` for sending (default $NOTIFIER_API_KEY, then the config file)")
	tokenFlag := fs.String("token", "", "user `token` (default $NOTIFIER_TOKEN, then the config file)")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout, counted from when the request starts")
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	rest := fs.Args()
	cmd := ""
	if !endedByDoubleDash(fs, args, rest) && isSubcommand(rest) {
		cmd, rest = rest[0], rest[1:]
	}
	if cmd == "version" {
		fmt.Fprintln(stdout, version)
		return 0
	}

	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr, transport: transport, timeout: *timeout}
	var err error
	// `config init` replaces the file, so it must work on one it cannot read.
	if c.configPath, err = configPath(); err == nil && cmd != "config" {
		c.config, err = loadConfig(c.configPath)
	}
	if err != nil {
		fmt.Fprintln(stderr, "notify:", err)
		return exitUsage
	}
	cfg := c.config
	c.url = first(*urlFlag, os.Getenv("NOTIFIER_URL"), cfg.URL)
	envToken := first(os.Getenv("NOTIFIER_TOKEN"), legacyToken(stderr))
	c.givenToken = first(*tokenFlag, envToken)
	c.givenKey = first(*keyFlag, os.Getenv("NOTIFIER_API_KEY"))
	c.token = first(c.givenToken, cfg.Token)
	for _, level := range [][2]string{
		{*keyFlag, *tokenFlag}, {os.Getenv("NOTIFIER_API_KEY"), envToken}, {cfg.APIKey, cfg.Token},
	} {
		if level[0] != "" {
			c.sendCred, c.sendCredName = level[0], "API key"
			break
		}
		if level[1] != "" {
			c.sendCred, c.sendCredName = level[1], "user token"
			break
		}
	}

	switch cmd {
	case "config":
		return c.configCmd(rest)
	case "signup":
		return c.signup(rest)
	case "invite":
		return c.invite(rest)
	case "devices":
		return c.devices(rest)
	case "pair":
		return c.pair(rest)
	case "keys":
		return c.keys(rest)
	case "key":
		return c.key(rest)
	case "channels":
		return c.channels(rest)
	case "channel":
		return c.channel(rest)
	case "heartbeats":
		return c.heartbeats(rest)
	case "heartbeat":
		return c.heartbeat(rest)
	case "hooks":
		return c.hooks(rest)
	case "hook":
		return c.hook(rest)
	}

	if len(rest) < 1 || len(rest) > 2 {
		fs.Usage()
		return exitUsage
	}
	if len(to) == 0 && len(channels) == 0 {
		to, channels = cfg.To, cfg.Channels
	}
	n := Notification{Title: fitTitle(rest[0]), Channels: channels}
	highSet := false
	fs.Visit(func(f *flag.Flag) { highSet = highSet || f.Name == "high" })
	if *high || !highSet && cfg.Priority == "high" {
		n.Priority = "high"
	}
	return c.send(n, to, rest[1:])
}

// legacyToken is NOTIFIER_ADMIN_TOKEN, which `notify pair` read before users
// existed, when it holds a user token. It used to hold the operator's
// ADMIN_TOKEN, which must never be sent as a user's credential.
func legacyToken(stderr io.Writer) string {
	v := os.Getenv("NOTIFIER_ADMIN_TOKEN")
	if v == "" || os.Getenv("NOTIFIER_TOKEN") != "" {
		return ""
	}
	if !strings.HasPrefix(v, userTokenPrefix) {
		fmt.Fprintln(stderr, "notify: ignoring NOTIFIER_ADMIN_TOKEN, which is not a user token; set NOTIFIER_TOKEN")
		return ""
	}
	fmt.Fprintln(stderr, "notify: NOTIFIER_ADMIN_TOKEN is deprecated; rename it to NOTIFIER_TOKEN")
	return v
}

const userTokenPrefix = "nu_"

// first returns the first non-empty value.
func first(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// client returns an API client with [cred], or reports what is missing; an
// empty setHint means the request needs no credential.
func (c *cli) client(cred, setHint string) (*Client, bool) {
	if c.url == "" || cred == "" && setHint != "" {
		msg := "notify: set NOTIFIER_URL (or pass -url)"
		if setHint != "" {
			msg += " and " + setHint
		}
		fmt.Fprintln(c.stderr, msg)
		return nil, false
	}
	if err := checkURL(c.url); err != nil {
		fmt.Fprintln(c.stderr, "notify:", err)
		return nil, false
	}
	return &Client{
		BaseURL: c.url,
		Token:   cred,
		HTTP:    &http.Client{Transport: c.transport, CheckRedirect: refuseRedirect},
	}, true
}

const tokenHint = "NOTIFIER_TOKEN (or pass -token, or run `notify signup`)"

func (c *cli) userClient() (*Client, bool) { return c.client(c.token, tokenHint) }

func (c *cli) sendClient() (*Client, bool) {
	return c.client(c.sendCred, "NOTIFIER_API_KEY or NOTIFIER_TOKEN (or pass -key or -token)")
}

func (c *cli) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.timeout)
}

func (c *cli) send(n Notification, to []string, body []string) int {
	client, ok := c.sendClient()
	if !ok {
		return exitUsage
	}
	if len(body) == 1 {
		text, dropped := body[0], false
		if text == "-" {
			// Read before the timeout starts: the producer may be a long job.
			b, more, err := readTail(c.stdin, stdinTailBytes)
			if err != nil {
				return fail(c.stderr, err, c.sendCredName)
			}
			text, dropped = strings.TrimRight(string(b), "\r\n"), more
			if i := strings.IndexByte(text, '\n'); dropped && i >= 0 {
				// The cut may split a line, and an escape sequence in it.
				text = text[i+1:]
			}
			text = strings.TrimRight(clean(text), "\n")
		}
		n.Body = fitBody(text, dropped)
	}

	ctx, cancel := c.context()
	defer cancel()

	if len(to) > 0 {
		devices, err := client.Devices(ctx)
		if err != nil {
			return fail(c.stderr, err, c.sendCredName)
		}
		if n.DeviceIDs, err = ResolveDevices(devices, to); errors.Is(err, ErrNoSuchDevice) {
			fmt.Fprintln(c.stderr, "notify:", err)
			return exitUndelivered
		} else if err != nil {
			return fail(c.stderr, err, c.sendCredName)
		}
	}

	result, err := client.Notify(ctx, n)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound &&
		(apiErr.Message == noDevicesMessage || strings.HasPrefix(apiErr.Message, noDeviceMessage)) {
		// No paired (or subscribed) phone, or one unpaired since name
		// resolution: nothing was delivered.
		fmt.Fprintln(c.stderr, "notify:", apiErr.Message)
		return exitUndelivered
	}
	if err != nil {
		return fail(c.stderr, err, c.sendCredName)
	}
	code := 0
	for _, d := range result.Deliveries {
		line := fmt.Sprintf("%s: %s", d.DeviceID, d.Status)
		if d.Error != "" {
			line += " (" + d.Error + ")"
		}
		if !delivered(d.Status) {
			code = exitUndelivered
			fmt.Fprintln(c.stderr, line)
		} else {
			fmt.Fprintln(c.stdout, line)
		}
	}
	// Other members' phones are theirs: report them, but only fail when
	// nothing reached anyone.
	if len(result.Members) > 0 {
		fmt.Fprintf(c.stdout, "other members' devices: %s\n", countsLine(result.Members))
	}
	if len(result.Deliveries) == 0 && result.Members["sent"]+result.Members["queued"] == 0 {
		code = exitUndelivered
	}
	return code
}

// delivered reports whether a device has the message or will get it: a
// desktop that is offline gets its queued messages when it reconnects.
func delivered(status string) bool { return status == "sent" || status == "queued" }

// countsLine formats delivery counts by status, in a stable order.
func countsLine(counts map[string]int) string {
	var parts []string
	for _, status := range []string{"sent", "queued", "unreachable", "failed"} {
		if counts[status] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[status], status))
		}
	}
	return strings.Join(parts, ", ")
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

// noDevicesMessage is the server's 404 when no paired phone matches, and
// noDeviceMessage prefixes the one for a device ID that is not (or no
// longer) paired, as opposed to a 404 from a wrong server URL.
const (
	noDevicesMessage = "no matching paired devices"
	noDeviceMessage  = "no paired device "
)

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

// terminalEscape matches ANSI escape sequences: CSI (colours, cursor
// moves), OSC (titles, links), nF (charset choices such as tput's ESC ( B),
// and the two-byte ones.
var terminalEscape = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[ -/]+[0-~]|[@-Z\\-_])`)

// clean makes s valid UTF-8 and removes what the server refuses in a
// message: terminal escapes (a coloured log loses its colours), carriage
// returns (line ends, or a progress bar's redraws, become newlines), and
// any other control or bidi override character, replaced with U+FFFD.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = terminalEscape.ReplaceAllString(s, "")
	s = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(s)
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || !unicode.IsControl(r) && !bidiOverride(r) {
			return r
		}
		return '\uFFFD'
	}, s)
}

// bidiOverride reports the embeddings, overrides and isolates, which
// reorder how text is shown; marks and joiners are fine.
func bidiOverride(r rune) bool {
	return r >= 0x202A && r <= 0x202E || r >= 0x2066 && r <= 0x2069
}

// fitTitle cleans s and shortens it to the server's limit, keeping the
// start and marking the cut.
func fitTitle(s string) string {
	s = clean(s)
	if len(s) <= maxTitleBytes {
		return s
	}
	end := maxTitleBytes - len(truncated)
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + truncated
}

// fitBody cleans s (invalid UTF-8 would otherwise also cost three bytes
// of JSON each) and shortens it to the server's limit, keeping the end —
// for piped logs that is where the error usually is — and marking any
// cut, including one made earlier while reading.
func fitBody(s string, dropped bool) string {
	s = clean(s)
	if !dropped && len(s) <= maxBodyBytes {
		return s
	}
	start := max(0, len(s)-(maxBodyBytes-len(truncated)))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return truncated + s[start:]
}

// fail reports err; credential names what a 401 rejected.
func fail(stderr io.Writer, err error, credential string) int {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusUnauthorized {
		fmt.Fprintln(stderr, "notify: the server rejected the", credential)
		return exitError
	}
	fmt.Fprintln(stderr, "notify:", err)
	return exitError
}
