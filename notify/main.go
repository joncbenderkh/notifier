// SPDX-License-Identifier: GPL-3.0-or-later

// Command notify sends notifications to paired phones and desktops through
// a notifier server, and manages the account behind them.
//
//	notify send [--to DEVICE]... [-c CHANNEL]... [--high] TITLE [BODY | -]
//	notify signup INVITE NAME
//	notify pair
//	notify channels
//
// DEVICE is a device name (case-insensitive) or ID, CHANNEL one of the
// user's own channels; without either the notification goes to the default
// channel, which every paired device subscribes to. A BODY of "-" is read
// from standard input. Flags of send come before TITLE: everything from
// TITLE on is message text, so a body from a variable can never act as a
// flag. The server and credentials come from flags, then NOTIFIER_URL,
// NOTIFIER_API_KEY and NOTIFIER_TOKEN, then the config file (see
// configPath). Sending uses an API key if one is set, else the user token;
// everything else needs the user token. Run with --help for the commands.
package main

import (
	"context"
	"errors"
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

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// version is overridden by release builds via -ldflags.
var version = "0.5.0"

const (
	exitError       = 1
	exitUsage       = 2
	exitUndelivered = 3
	// exitNoAnswer (4), in ask.go: a question expired unanswered.
	// exitNotSeen (4), in receipts.go: no device opened the message in time.

	// maxTitleBytes and maxBodyBytes mirror the server's limits.
	maxTitleBytes = 256
	maxBodyBytes  = 2048
	// stdinTailBytes bounds memory when a large stream is piped in; only its
	// end can ever be sent.
	stdinTailBytes = 8 << 10
	truncated      = "…"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, nil))
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

// run executes the command line and returns the exit code; transport is
// nil outside tests.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer, transport http.RoundTripper) int {
	c := &cli{stdin: stdin, stdout: stdout, stderr: stderr, transport: transport}
	root := newRoot(c)
	if cmd, rest, err := root.Find(args); err == nil {
		if f := oldStyleFlag(cmd, rest); f != "" {
			fmt.Fprintf(stderr, "notify: unknown flag -%s; long flags take two dashes since 0.4.0: --%s\n", f, f)
			return exitUsage
		}
	}
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	cmd, err := root.ExecuteC()
	var code exitCode
	switch {
	case err == nil:
		return 0
	case errors.As(err, &code):
		return int(code)
	}
	// Cobra's own: an unknown command or flag, or a wrong argument count.
	fmt.Fprintln(stderr, "notify:", err)
	fmt.Fprintf(stderr, "Run '%s --help' for usage.\n", cmd.CommandPath())
	return exitUsage
}

// oldStyleFlag returns the name of the first argument, among [args] left
// for [cmd], written as a single-dash long flag (-high) that [cmd] knows as
// --high. It is checked before parsing, since pflag could read some of
// them as a shorthand with a value (-channel=x as -c hannel=x). It stops
// at "--", and at the first argument of a command whose flags end there.
func oldStyleFlag(cmd *cobra.Command, args []string) string {
	cmd.InitDefaultHelpFlag() // so -help is caught too
	lookup := func(name string) *pflag.Flag {
		if f := cmd.Flags().Lookup(name); f != nil {
			return f
		}
		return cmd.InheritedFlags().Lookup(name)
	}
	short := func(name string) *pflag.Flag {
		if f := cmd.Flags().ShorthandLookup(name); f != nil {
			return f
		}
		return cmd.InheritedFlags().ShorthandLookup(name)
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return ""
		case strings.HasPrefix(a, "--"):
			name, _, hasValue := strings.Cut(a[2:], "=")
			if f := lookup(name); f != nil && !hasValue && f.NoOptDefVal == "" {
				i++ // its value
			}
		case len(a) == 2 && a[0] == '-':
			if f := short(a[1:]); f != nil && f.NoOptDefVal == "" {
				i++
			}
		case len(a) > 2 && a[0] == '-':
			if name, _, _ := strings.Cut(a[1:], "="); lookup(name) != nil {
				return name
			}
		default:
			if cmd.Annotations[flagsEndAtArgs] != "" {
				return ""
			}
		}
	}
	return ""
}

// flagsEndAtArgs marks a command whose flags end at its first argument.
const flagsEndAtArgs = "flags-end-at-args"

// resolve settles the server and credentials once the flags are parsed:
// flags, then the environment, then the config file, which [loadFile]
// skips (`config init` replaces it, so it must work on one it cannot read).
func (c *cli) resolve(f settingsFlags, loadFile bool) int {
	var err error
	if c.configPath, err = configPath(); err == nil && loadFile {
		c.config, err = loadConfig(c.configPath)
	}
	if err != nil {
		fmt.Fprintln(c.stderr, "notify:", err)
		return exitUsage
	}
	cfg := c.config
	c.timeout = f.timeout
	c.url = first(f.url, os.Getenv("NOTIFIER_URL"), cfg.URL)
	envToken := first(os.Getenv("NOTIFIER_TOKEN"), legacyToken(c.stderr))
	c.givenToken = first(f.token, envToken)
	c.givenKey = first(f.key, os.Getenv("NOTIFIER_API_KEY"))
	c.token = first(c.givenToken, cfg.Token)
	for _, level := range [][2]string{
		{f.key, f.token}, {os.Getenv("NOTIFIER_API_KEY"), envToken}, {cfg.APIKey, cfg.Token},
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
	return 0
}

// sendCmd sends [args] (TITLE [BODY | -]); [high] is nil unless --high was
// given, so the config file's priority applies otherwise, and [waitSeen]
// is nil unless --wait-seen was given.
func (c *cli) sendCmd(args, to, channels []string, high *bool, receipts bool, waitSeen *time.Duration) int {
	if waitSeen != nil && (*waitSeen < minAskWait || *waitSeen > maxAskWait) {
		fmt.Fprintf(c.stderr, "notify: --wait-seen must be %s to %s\n", minAskWait, maxAskWait)
		return exitUsage
	}
	n, to := c.notification(args[0], to, channels, high)
	n.Receipts = receipts || waitSeen != nil
	client, id, code := c.deliver(n, to, args[1:], c.stdout, true)
	if id == "" || !n.Receipts {
		return code
	}
	// For notify status, even while waiting: it may end undecided.
	fmt.Fprintln(c.stdout, "id:", id)
	if waitSeen == nil {
		return code
	}
	// Whether it was seen is what the caller waits for, even when some
	// devices could not be reached.
	return c.awaitSeen(client, id, *waitSeen)
}

// notification builds a message titled [title] and returns the devices to
// resolve for it: the config file's targets apply when neither [to] nor
// [channels] is given, and its priority unless [high] is set.
func (c *cli) notification(title string, to, channels []string, high *bool) (Notification, []string) {
	if len(to) == 0 && len(channels) == 0 {
		to, channels = c.config.To, c.config.Channels
	}
	n := Notification{Title: fitTitle(title), Channels: channels}
	if high != nil && *high || high == nil && c.config.Priority == "high" {
		n.Priority = "high"
	}
	return n, to
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
		msg := "notify: set NOTIFIER_URL (or pass --url)"
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

const tokenHint = "NOTIFIER_TOKEN (or pass --token, or run `notify signup`)"

func (c *cli) userClient() (*Client, bool) { return c.client(c.token, tokenHint) }

func (c *cli) sendClient() (*Client, bool) {
	return c.client(c.sendCred, "NOTIFIER_API_KEY or NOTIFIER_TOKEN (or pass --key or --token)")
}

func (c *cli) context() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), c.timeout)
}

// deliver sends [n] with [body] (none, BODY or -) to [to] and its
// channels, and reports each device on [out], or on stderr when it was
// missed. It returns the message ID, if the message reached one of the
// sender's devices or, when [members] counts them, another member's, and
// the exit code.
func (c *cli) deliver(n Notification, to []string, body []string, out io.Writer, members bool) (*Client, string, int) {
	client, ok := c.sendClient()
	if !ok {
		return nil, "", exitUsage
	}
	if len(body) == 1 {
		text, dropped := body[0], false
		if text == "-" {
			// Read before the timeout starts: the producer may be a long job.
			b, more, err := readTail(c.stdin, stdinTailBytes)
			if err != nil {
				return nil, "", fail(c.stderr, err, c.sendCredName)
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
			return nil, "", fail(c.stderr, err, c.sendCredName)
		}
		if n.DeviceIDs, err = ResolveDevices(devices, to); errors.Is(err, ErrNoSuchDevice) {
			fmt.Fprintln(c.stderr, "notify:", err)
			return nil, "", exitUndelivered
		} else if err != nil {
			return nil, "", fail(c.stderr, err, c.sendCredName)
		}
	}

	result, err := client.Notify(ctx, n)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound &&
		(apiErr.Message == noDevicesMessage || strings.HasPrefix(apiErr.Message, noDeviceMessage) ||
			strings.HasPrefix(apiErr.Message, noAnswererMessage)) {
		// No paired (or subscribed) phone, or one unpaired since name
		// resolution: nothing was delivered.
		fmt.Fprintln(c.stderr, "notify:", apiErr.Message)
		return nil, "", exitUndelivered
	}
	if err != nil {
		return nil, "", fail(c.stderr, err, c.sendCredName)
	}
	code, reached := 0, false
	for _, d := range result.Deliveries {
		line := fmt.Sprintf("%s: %s", d.DeviceID, d.Status)
		if d.Error != "" {
			line += " (" + d.Error + ")"
		}
		if !delivered(d.Status) {
			code = exitUndelivered
			fmt.Fprintln(c.stderr, line)
		} else {
			reached = true
			fmt.Fprintln(out, line)
		}
	}
	// Other members' phones are theirs: report them, but only fail when
	// nothing reached anyone.
	if len(result.Members) > 0 {
		fmt.Fprintf(out, "other members' devices: %s\n", countsLine(result.Members))
	}
	if members && result.Members["sent"]+result.Members["queued"] > 0 {
		reached = true
	}
	if !reached {
		return client, "", exitUndelivered
	}
	return client, result.ID, code
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

// noDevicesMessage is the server's 404 when no paired phone matches,
// noDeviceMessage prefixes the one for a device ID that is not (or no
// longer) paired, and noAnswererMessage the one for a question none of the
// matching devices may answer, as opposed to a 404 from a wrong server URL.
const (
	noDevicesMessage  = "no matching paired devices"
	noDeviceMessage   = "no paired device "
	noAnswererMessage = "no matching paired devices may answer"
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
