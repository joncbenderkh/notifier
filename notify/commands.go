// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"
)

// subFlags parses a subcommand's own flags, which come before its arguments,
// and checks the argument count.
func (c *cli) subFlags(name, args string, argv []string, minArgs, maxArgs int, define func(*flag.FlagSet)) ([]string, bool) {
	fs := flag.NewFlagSet("notify "+name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() {
		fmt.Fprintf(c.stderr, "Usage: notify %s %s\n", name, args)
		fs.PrintDefaults()
	}
	if define != nil {
		define(fs)
	}
	if fs.Parse(argv) != nil {
		return nil, false
	}
	if fs.NArg() < minArgs || fs.NArg() > maxArgs {
		fs.Usage()
		return nil, false
	}
	return fs.Args(), true
}

func (c *cli) configCmd(argv []string) int {
	if len(argv) == 0 || argv[0] != "init" {
		fmt.Fprintln(c.stderr, "Usage: notify [-url URL] [-token TOKEN] [-key KEY] config init [-force]")
		return exitUsage
	}
	var force *bool
	if _, ok := c.subFlags("config init", "[-force]", argv[1:], 0, 0, func(fs *flag.FlagSet) {
		force = fs.Bool("force", false, "replace an existing config file")
	}); !ok {
		return exitUsage
	}
	if _, err := os.Stat(c.configPath); err == nil && !*force {
		fmt.Fprintf(c.stderr, "notify: %s already exists (pass -force to replace it)\n", c.configPath)
		return exitError
	}
	if c.url == "" {
		fmt.Fprintln(c.stderr, "notify: set NOTIFIER_URL or pass -url")
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

func (c *cli) signup(argv []string) int {
	var force *bool
	args, ok := c.subFlags("signup", "[-force] INVITE NAME", argv, 2, 2, func(fs *flag.FlagSet) {
		force = fs.Bool("force", false, "replace a user token already in the config file")
	})
	if !ok {
		return exitUsage
	}
	if c.config.Token != "" && !*force {
		fmt.Fprintf(c.stderr, "notify: %s already holds a user token (pass -force to replace it)\n", c.configPath)
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

func (c *cli) invite(argv []string) int {
	if _, ok := c.subFlags("invite", "", argv, 0, 0, nil); !ok {
		return exitUsage
	}
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
	fmt.Fprintf(c.stdout, "Invite %s (single use, expires %s). To sign up:\n  notify -url %s signup %s NAME\n",
		inv.Code, inv.ExpiresAt.Local().Format(time.DateTime), c.url, inv.Code)
	return 0
}

func (c *cli) devices(argv []string) int {
	if _, ok := c.subFlags("devices", "", argv, 0, 0, nil); !ok {
		return exitUsage
	}
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

func (c *cli) pair(argv []string) int {
	if _, ok := c.subFlags("pair", "", argv, 0, 0, nil); !ok {
		return exitUsage
	}
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

func (c *cli) keys(argv []string) int {
	if _, ok := c.subFlags("keys", "", argv, 0, 0, nil); !ok {
		return exitUsage
	}
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

func (c *cli) key(argv []string) int {
	action := ""
	if len(argv) > 0 {
		action, argv = argv[0], argv[1:]
	}
	switch action {
	case "create":
		var scope multiFlag
		args, ok := c.subFlags("key create", "[-c CHANNEL]... NAME", argv, 1, 1, func(fs *flag.FlagSet) {
			fs.Var(&scope, "c", "limit the key to this own `channel`; repeatable or comma-separated")
		})
		if !ok {
			return exitUsage
		}
		client, ok := c.userClient()
		if !ok {
			return exitUsage
		}
		ctx, cancel := c.context()
		defer cancel()
		k, err := client.CreateKey(ctx, args[0], scope)
		if err != nil {
			return fail(c.stderr, err, "user token")
		}
		fmt.Fprintf(c.stdout, "%s\n", k.Key)
		fmt.Fprintf(c.stderr, "Created API key %s (%s); it is shown only this once.\n", k.Name, k.ID)
		return 0
	case "delete":
		args, ok := c.subFlags("key delete", "NAME|ID", argv, 1, 1, nil)
		if !ok {
			return exitUsage
		}
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
			if k.ID == args[0] || k.Name == args[0] {
				matches = append(matches, k)
			}
		}
		if len(matches) != 1 {
			fmt.Fprintf(c.stderr, "notify: %d API keys match %q; use an ID from `notify keys`\n", len(matches), args[0])
			return exitError
		}
		if err := client.DeleteKey(ctx, matches[0].ID); err != nil {
			return fail(c.stderr, err, "user token")
		}
		fmt.Fprintln(c.stdout, "Deleted API key", matches[0].ID)
		return 0
	}
	fmt.Fprintln(c.stderr, "Usage: notify key create [-c CHANNEL]... NAME\n       notify key delete NAME|ID")
	return exitUsage
}

func (c *cli) channels(argv []string) int {
	if _, ok := c.subFlags("channels", "", argv, 0, 0, nil); !ok {
		return exitUsage
	}
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
	w := table(c.stdout, "ID\tNAME\tOWNED\tMEMBERS")
	for _, ch := range channels {
		fmt.Fprintf(w, "%s\t%s\t%t\t%d\n", ch.ID, ch.Name, ch.Owned, ch.Members)
	}
	w.Flush()
	return 0
}

const channelUsage = `Usage:
  notify channel create NAME
  notify channel delete NAME
  notify channel invite NAME
  notify channel members NAME
  notify channel leave NAME
  notify channel join CODE
  notify channel remove NAME MEMBER`

func (c *cli) channel(argv []string) int {
	action := ""
	if len(argv) > 0 {
		action, argv = argv[0], argv[1:]
	}
	nargs := map[string]int{"create": 1, "delete": 1, "invite": 1, "members": 1, "leave": 1, "join": 1, "remove": 2}[action]
	if nargs == 0 {
		fmt.Fprintln(c.stderr, channelUsage)
		return exitUsage
	}
	args, ok := c.subFlags("channel "+action, "...", argv, nargs, nargs, nil)
	if !ok {
		return exitUsage
	}
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
			fmt.Fprintf(c.stdout, "%s\n  notify -url %s channel join %s\n  (single use, expires %s)\n",
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
