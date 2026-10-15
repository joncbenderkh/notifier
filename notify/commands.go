// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func (c *cli) configInit(force bool) int {
	if _, err := os.Stat(c.configPath); err == nil && !force {
		fmt.Fprintf(c.stderr, "notify: %s already exists (pass --force to replace it)\n", c.configPath)
		return exitError
	}
	if c.url == "" {
		fmt.Fprintln(c.stderr, "notify: set NOTIFIER_URL or pass --url")
		return exitUsage
	}
	if err := checkURL(c.url); err != nil {
		fmt.Fprintln(c.stderr, "notify:", err)
		return exitUsage
	}
	// Only what was given now: the old file's contents are being replaced.
	cfg := Config{URL: c.url, Token: c.givenToken, APIKey: c.givenKey}
	if err := saveConfig(c.configPath, cfg); err != nil {
		fmt.Fprintln(c.stderr, "notify:", err)
		return exitError
	}
	fmt.Fprintln(c.stdout, "Wrote", c.configPath)
	return 0
}

func (c *cli) signup(args []string, force bool) int {
	if c.config.Token != "" && !force {
		fmt.Fprintf(c.stderr, "notify: %s already holds a user token (pass --force to replace it)\n", c.configPath)
		return exitError
	}
	client, ok := c.client("", "") // the invite is the credential
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	s, err := client.Signup(ctx, args[0], args[1])
	if err != nil {
		return fail(c.stderr, err, "invite")
	}
	// The old key and targets belong to the previous account (or server).
	cfg := Config{URL: c.url, Token: s.Token, Priority: c.config.Priority}
	if err := saveConfig(c.configPath, cfg); err != nil {
		// The token exists only in this response: never lose it.
		fmt.Fprintf(c.stderr, "notify: could not save the token: %v\nYour token (shown once): %s\n", err, s.Token)
		return exitError
	}
	fmt.Fprintf(c.stdout, "Signed up as %s (%s); token saved to %s\n", s.Name, s.UserID, c.configPath)
	if c.config.APIKey != "" || len(c.config.To) > 0 || len(c.config.Channels) > 0 {
		fmt.Fprintln(c.stderr, "notify: removed the previous account's api_key, to and channels from the config file")
	}
	return 0
}

func (c *cli) invite() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	inv, err := client.Invite(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintf(c.stdout, "Invite %s (single use, expires %s). To sign up:\n  notify --url %s signup %s NAME\n",
		inv.Code, inv.ExpiresAt.Local().Format(time.DateTime), c.url, inv.Code)
	return 0
}

func (c *cli) devices() int {
	client, ok := c.sendClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	devices, err := client.Devices(ctx)
	if err != nil {
		return fail(c.stderr, err, c.sendCredName)
	}
	w := table(c.stdout, "ID\tNAME\tTRANSPORT\tREACHABLE")
	for _, d := range devices {
		fmt.Fprintf(w, "%s\t%s\t%s\t%t\n", d.ID, d.Name, d.Transport, d.Reachable)
	}
	w.Flush()
	return 0
}

func (c *cli) pair() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	p, err := client.Pair(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	// The code already exists on the server, so always print it, even when
	// there is no QR to go with it.
	how := c.printQR(p.URI, "Scan with the Notifier app, or enter by hand:", "Enter in the Notifier app:")
	fmt.Fprintf(c.stdout, "%s\n  server  %s\n  code    %s  (expires %s, single use)\n",
		how, client.BaseURL, p.Code, p.ExpiresAt.Local().Format(time.Kitchen))
	return 0
}

// printQR prints data as a QR code and returns [scan]; without a QR (data
// does not fit, say) it returns [manual].
func (c *cli) printQR(data, scan, manual string) string {
	qr, err := renderQR(data)
	if err != nil {
		fmt.Fprintln(c.stderr, "notify: no QR code:", err)
		return manual
	}
	fmt.Fprint(c.stdout, qr, "\n")
	return scan
}

func (c *cli) keys() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	keys, err := client.Keys(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	w := table(c.stdout, "ID\tNAME\tCHANNELS\tLAST USED")
	for _, k := range keys {
		scope, used := "any", "never"
		if len(k.Channels) > 0 {
			scope = strings.Join(k.Channels, ",")
		}
		if k.LastUsedAt != nil {
			used = k.LastUsedAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.ID, k.Name, scope, used)
	}
	w.Flush()
	return 0
}

func (c *cli) keyCreate(name string, scope []string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	k, err := client.CreateKey(ctx, name, scope)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintf(c.stdout, "%s\n", k.Key)
	fmt.Fprintf(c.stderr, "Created API key %s (%s); it is shown only this once.\n", k.Name, k.ID)
	return 0
}

func (c *cli) keyDelete(sel string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	keys, err := client.Keys(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	var matches []APIKey
	for _, k := range keys {
		if k.ID == sel || k.Name == sel {
			matches = append(matches, k)
		}
	}
	if len(matches) != 1 {
		fmt.Fprintf(c.stderr, "notify: %d API keys match %q; use an ID from `notify keys`\n", len(matches), sel)
		return exitError
	}
	if err := client.DeleteKey(ctx, matches[0].ID); err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintln(c.stdout, "Deleted API key", matches[0].ID)
	return 0
}

func (c *cli) channels() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	channels, err := client.Channels(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	w := table(c.stdout, "ID\tNAME\tOWNED\tMEMBERS\tRECEIPTS")
	for _, ch := range channels {
		fmt.Fprintf(w, "%s\t%s\t%t\t%d\t%s\n", ch.ID, ch.Name, ch.Owned, ch.Members, receiptsLabel(ch))
	}
	w.Flush()
	return 0
}

// channel runs `notify channel ACTION ARGS...`; the command tree has
// checked the argument count.
func (c *cli) channel(action string, args []string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()

	switch action {
	case "create":
		ch, err := client.CreateChannel(ctx, args[0])
		if err != nil {
			return fail(c.stderr, err, "user token")
		}
		fmt.Fprintf(c.stdout, "Created channel %s (%s)\n", ch.Name, ch.ID)
		return 0
	case "join":
		ch, err := client.Join(ctx, args[0])
		if err != nil {
			return fail(c.stderr, err, "user token")
		}
		fmt.Fprintf(c.stdout, "Joined %s; choose which of your devices subscribe in the app\n", ch.Name)
		return 0
	}

	ch, code := c.findChannel(ctx, client, args[0], action != "leave")
	if code != 0 {
		return code
	}
	var err error
	switch action {
	case "delete":
		if err = client.DeleteChannel(ctx, ch.ID); err == nil {
			fmt.Fprintln(c.stdout, "Deleted channel", ch.Name)
		}
	case "invite":
		var inv ChannelInvite
		if inv, err = client.InviteToChannel(ctx, ch.ID); err == nil {
			how := c.printQR(inv.URI, "Scan with the Notifier app, or join with:", "Join with:")
			fmt.Fprintf(c.stdout, "%s\n  notify --url %s channel join %s\n  (single use, expires %s)\n",
				how, c.url, inv.Code, inv.ExpiresAt.Local().Format(time.DateTime))
		}
	case "members":
		var members []Member
		if members, err = client.Members(ctx, ch.ID); err == nil {
			w := table(c.stdout, "ID\tNAME\tJOINED")
			for _, m := range members {
				fmt.Fprintf(w, "%s\t%s\t%s\n", m.ID, m.Name, m.JoinedAt.Local().Format(time.DateTime))
			}
			w.Flush()
		}
	case "leave":
		if ch.Owned {
			fmt.Fprintf(c.stderr, "notify: you own %s; delete it instead (`notify channel delete %s`)\n", ch.Name, ch.Name)
			return exitError
		}
		var me User
		if me, err = client.Me(ctx); err == nil {
			if err = client.RemoveMember(ctx, ch.ID, me.ID); err == nil {
				fmt.Fprintln(c.stdout, "Left", ch.Name)
			}
		}
	case "remove":
		var members []Member
		if members, err = client.Members(ctx, ch.ID); err == nil {
			var matches []Member
			for _, m := range members {
				if m.ID == args[1] || m.Name == args[1] {
					matches = append(matches, m)
				}
			}
			if len(matches) != 1 {
				fmt.Fprintf(c.stderr, "notify: %d members match %q; use an ID from `notify channel members`\n", len(matches), args[1])
				return exitError
			}
			if err = client.RemoveMember(ctx, ch.ID, matches[0].ID); err == nil {
				fmt.Fprintf(c.stdout, "Removed %s from %s\n", matches[0].Name, ch.Name)
			}
		}
	}
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	return 0
}

// receiptsLabel says whether other members' devices report receipts on
// [ch], and on an optional one whether this member turned them on.
func receiptsLabel(ch Channel) string {
	switch {
	case ch.Owned || ch.Receipts != "optional":
		return ch.Receipts
	case ch.ReceiptsAccepted:
		return "optional, yours on"
	default:
		return "optional, yours off"
	}
}

// channelReceipts runs `notify channel receipts NAME [SETTING]`: the owner
// sets off, optional (or on) or required; a member turns them on or off
// where the owner made them optional.
func (c *cli) channelReceipts(args []string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	ch, code := c.findChannel(ctx, client, args[0], false)
	if code != 0 {
		return code
	}
	if len(args) == 1 {
		fmt.Fprintf(c.stdout, "%s: receipts %s\n", ch.Name, receiptsLabel(ch))
		return 0
	}
	setting := args[1]
	var err error
	switch {
	case ch.Owned:
		if setting == "on" {
			setting = "optional"
		}
		if !slices.Contains([]string{"off", "optional", "required"}, setting) {
			fmt.Fprintln(c.stderr, "notify: as the owner, set receipts off, optional (or on) or required")
			return exitUsage
		}
		if err = client.SetChannelReceipts(ctx, ch.ID, setting); err == nil {
			fmt.Fprintf(c.stdout, "%s: receipts %s\n", ch.Name, setting)
		}
	case setting == "on" || setting == "off":
		var got Channel
		if got, err = client.AcceptReceipts(ctx, ch.ID, setting == "on"); err == nil {
			ch.Receipts, ch.ReceiptsAccepted = got.Receipts, got.ReceiptsAccepted
			fmt.Fprintf(c.stdout, "%s: receipts %s\n", ch.Name, receiptsLabel(ch))
		}
	default:
		fmt.Fprintln(c.stderr, "notify: as a member, turn receipts on or off")
		return exitUsage
	}
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	return 0
}

// findChannel resolves a channel by ID or by name as the user sees it
// ("homelab", or "alice/homelab" when joined); [owned] requires ownership.
func (c *cli) findChannel(ctx context.Context, client *Client, sel string, owned bool) (Channel, int) {
	channels, err := client.Channels(ctx)
	if err != nil {
		return Channel{}, fail(c.stderr, err, "user token")
	}
	// Owner names are not unique, so two joined channels can share a label.
	var matches []Channel
	for _, ch := range channels {
		if ch.ID == sel {
			matches = []Channel{ch}
			break
		}
		if ch.Name == sel {
			matches = append(matches, ch)
		}
	}
	switch {
	case len(matches) == 0:
		fmt.Fprintf(c.stderr, "notify: no channel %q (see `notify channels`)\n", sel)
		return Channel{}, exitError
	case len(matches) > 1:
		fmt.Fprintf(c.stderr, "notify: %d channels are named %q; use an ID from `notify channels`\n", len(matches), sel)
		return Channel{}, exitError
	case owned && !matches[0].Owned:
		fmt.Fprintf(c.stderr, "notify: %s is not your channel\n", matches[0].Name)
		return Channel{}, exitError
	}
	return matches[0], 0
}

func table(w io.Writer, header string) *tabwriter.Writer {
	t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(t, header)
	return t
}

func (c *cli) heartbeats() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	hbs, err := client.Heartbeats(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	w := table(c.stdout, "ID\tNAME\tSTATE\tCHANNEL\tEVERY\tLAST CHECK-IN\tDUE")
	for _, hb := range hbs {
		last, due := "never", "-"
		if hb.LastPingAt != nil {
			last = hb.LastPingAt.Local().Format(time.DateTime)
		}
		// A down heartbeat has alerted; up and failed ones alert at their due time.
		if hb.DueAt != nil && hb.State != "down" {
			due = hb.DueAt.Local().Format(time.DateTime)
		}
		every := formatEvery(time.Duration(hb.Interval) * time.Second)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", hb.ID, hb.Name, hb.State, hb.Channel, every, last, due)
	}
	w.Flush()
	return 0
}

func (c *cli) heartbeatCreate(name, every, grace, channel string) int {
	interval, err := parseEvery(every)
	if err != nil {
		fmt.Fprintln(c.stderr, "notify: --every:", err)
		return exitUsage
	}
	var graceD time.Duration
	if grace != "" {
		if graceD, err = parseEvery(grace); err != nil {
			fmt.Fprintln(c.stderr, "notify: --grace:", err)
			return exitUsage
		}
	}
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	hb, err := client.CreateHeartbeat(ctx, name, interval, graceD, channel)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintf(c.stdout, "%s\n", hb.PingURL)
	fmt.Fprintf(c.stderr, "Created heartbeat %s (%s); its URL is shown only this once.\n"+
		"Ping it after each successful run, e.g. in a crontab:\n"+
		"  0 3 * * *  backup.sh && curl -fsS -m 10 --retry 3 %s >/dev/null\n"+
		"Report a failure with %s/fail (a POSTed body becomes the message).\n",
		hb.Name, hb.ID, hb.PingURL, hb.PingURL)
	return 0
}

func (c *cli) heartbeatDelete(sel string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	hbs, err := client.Heartbeats(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	matches := byIDOrName(hbs, sel, func(hb Heartbeat) (string, string) { return hb.ID, hb.Name })
	if len(matches) != 1 {
		fmt.Fprintf(c.stderr, "notify: %d heartbeats match %q; use an ID from `notify heartbeats`\n", len(matches), sel)
		return exitError
	}
	if err := client.DeleteHeartbeat(ctx, matches[0].ID); err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintln(c.stdout, "Deleted heartbeat", matches[0].ID)
	return 0
}

// byIDOrName returns the items [arg] names: an exact ID wins, even over
// an item named like it; otherwise every item of that name.
func byIDOrName[T any](items []T, arg string, idName func(T) (string, string)) []T {
	var matches []T
	for _, it := range items {
		id, name := idName(it)
		if id == arg {
			return []T{it}
		}
		if name == arg {
			matches = append(matches, it)
		}
	}
	return matches
}

func (c *cli) hooks() int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	hooks, err := client.Hooks(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	w := table(c.stdout, "ID\tNAME\tFORMAT\tCHANNEL\tLAST USED")
	for _, h := range hooks {
		last := "never"
		if h.LastUsedAt != nil {
			last = h.LastUsedAt.Local().Format(time.DateTime)
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", h.ID, h.Name, h.Format, h.Channel, last)
	}
	w.Flush()
	return 0
}

// hookHints tell how to point each format's tool at a hook's URL.
var hookHints = map[string]string{
	"alertmanager": "In alertmanager.yml:\n" +
		"  receivers:\n" +
		"    - name: notifier\n" +
		"      webhook_configs:\n" +
		"        - url: %s\n",
	"grafana": "In Grafana, add a contact point of type Webhook with this URL:\n  %s\n",
	"generic": "POST JSON to it:\n" +
		"  curl -fsS -m 10 -d '{\"title\":\"Deploy done\",\"body\":\"v1.2.3\",\"priority\":\"default\"}' %s\n",
}

func (c *cli) hookCreate(format, name, channel string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	h, err := client.CreateHook(ctx, name, format, channel)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintf(c.stdout, "%s\n", h.URL)
	fmt.Fprintf(c.stderr, "Created hook %s (%s); its URL is shown only this once.\n", h.Name, h.ID)
	if hint, ok := hookHints[h.Format]; ok {
		fmt.Fprintf(c.stderr, hint, h.URL)
	}
	return 0
}

func (c *cli) hookDelete(sel string) int {
	client, ok := c.userClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	hooks, err := client.Hooks(ctx)
	if err != nil {
		return fail(c.stderr, err, "user token")
	}
	matches := byIDOrName(hooks, sel, func(h Hook) (string, string) { return h.ID, h.Name })
	if len(matches) != 1 {
		fmt.Fprintf(c.stderr, "notify: %d hooks match %q; use an ID from `notify hooks`\n", len(matches), sel)
		return exitError
	}
	if err := client.DeleteHook(ctx, matches[0].ID); err != nil {
		return fail(c.stderr, err, "user token")
	}
	fmt.Fprintln(c.stdout, "Deleted hook", matches[0].ID)
	return 0
}

// parseEvery reads a duration in whole seconds, as time.ParseDuration does
// plus days ("1d", "1d12h").
func parseEvery(s string) (time.Duration, error) {
	if strings.ContainsAny(s, "+-") {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	var days time.Duration
	if i := strings.Index(s, "d"); i > 0 {
		n, err := strconv.Atoi(s[:i])
		if err != nil || n > 36500 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		days, s = time.Duration(n)*24*time.Hour, s[i+1:]
	}
	var rest time.Duration
	if s != "" {
		var err error
		if rest, err = time.ParseDuration(s); err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
	}
	d := days + rest
	if d <= 0 || d%time.Second != 0 {
		return 0, fmt.Errorf("must be a positive whole number of seconds, like 30m, 6h or 1d")
	}
	return d, nil
}

// formatEvery prints a heartbeat interval as parseEvery reads it, with
// only the units it needs: "1d", "1h30m", "1h0m1s" becomes "1h1s".
func formatEvery(d time.Duration) string {
	var b strings.Builder
	for _, u := range []struct {
		size time.Duration
		name string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if n := d / u.size; n > 0 {
			fmt.Fprintf(&b, "%d%s", n, u.name)
			d -= n * u.size
		}
	}
	return b.String()
}
