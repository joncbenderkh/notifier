// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// exitCode ends a command with that exit status; what went wrong has
// already been reported.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// status turns a handler's exit code into the error Cobra returns.
func status(code int) error {
	if code == 0 {
		return nil
	}
	return exitCode(code)
}

// settingsFlags are the persistent flags. They default to empty and are
// resolved after parsing, so help never shows a secret.
type settingsFlags struct {
	url, token, key string
	timeout         time.Duration
}

// newRoot builds the command tree around [c].
func newRoot(c *cli) *cobra.Command {
	var sf settingsFlags
	// withSettings resolves the settings, then runs [fn]; [loadFile] is
	// false only for `config init`.
	withSettings := func(loadFile bool, fn func(args []string) int) func(*cobra.Command, []string) error {
		return func(_ *cobra.Command, args []string) error {
			if code := c.resolve(sf, loadFile); code != 0 {
				return exitCode(code)
			}
			return status(fn(args))
		}
	}
	// leaf is a command that resolves the settings and runs [fn].
	leaf := func(use, short string, args cobra.PositionalArgs, fn func(args []string) int) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: withSettings(true, fn)}
	}

	root := group(&cobra.Command{
		Use:   "notify",
		Short: "Send notifications to your devices through a notifier server",
		Long: "notify sends notifications to your paired phones and desktops through a\n" +
			"notifier server, and manages the account behind them.\n\n" +
			"The server and credentials come from flags, then NOTIFIER_URL,\n" +
			"NOTIFIER_API_KEY and NOTIFIER_TOKEN, then the config file\n" +
			"($XDG_CONFIG_HOME/notifier/config.json, or NOTIFIER_CONFIG).",
		SilenceErrors: true,
		SilenceUsage:  true,
	})
	pf := root.PersistentFlags()
	pf.StringVar(&sf.url, "url", "", "notifier server `URL` (default $NOTIFIER_URL, then the config file)")
	pf.StringVar(&sf.token, "token", "", "user `token` (default $NOTIFIER_TOKEN, then the config file)")
	pf.StringVar(&sf.key, "key", "", "API `key` for sending (default $NOTIFIER_API_KEY, then the config file)")
	pf.DurationVar(&sf.timeout, "timeout", 30*time.Second, "request timeout, counted from when the request starts")

	var to, channels []string
	var high, receipts bool
	var waitSeen seenWait
	send := &cobra.Command{
		Use:   "send [flags] TITLE [BODY | -]",
		Short: "Send a notification",
		Long: "Send a notification to the default channel, or to the given channels or\n" +
			"devices (the config file's \"to\" and \"channels\" when neither is given).\n\n" +
			"A BODY of - is read from standard input; only its last 8 KiB are kept.\n" +
			"Flags come before TITLE: everything from TITLE on is message text, so a\n" +
			"body from a variable can never act as a flag. Put -- before a TITLE that\n" +
			"starts with -.",
		Example: "  notify send \"Backup done\" \"42 GB in 3 min\"\n" +
			"  make 2>&1 | notify send --high -c builds \"Build failed\" -\n" +
			"  notify send --high --wait-seen=5m \"Disk full\" || page-someone",
		Args:        cobra.RangeArgs(1, 2),
		Annotations: map[string]string{flagsEndAtArgs: "yes"},
	}
	send.RunE = withSettings(true, func(args []string) int {
		var h *bool
		if send.Flags().Changed("high") {
			h = &high
		}
		var w *time.Duration
		if send.Flags().Changed("wait-seen") {
			w = &waitSeen.d
			// The duration is optional, so "--wait-seen 5m TITLE" reads 5m as
			// the title.
			if waitSeen.bare && send.ArgsLenAtDash() != 0 && looksLikeDuration(args[0]) {
				fmt.Fprintln(c.stderr, "notify: --wait-seen takes its duration after =, as in --wait-seen=5m; put -- before a title that looks like a duration")
				return exitUsage
			}
		}
		return c.sendCmd(args, splitList(to), splitList(channels), h, receipts, w)
	})
	send.Flags().SetInterspersed(false)
	send.Flags().StringArrayVar(&to, "to", nil, "device `name or ID` to notify; repeatable or comma-separated")
	send.Flags().StringArrayVarP(&channels, "channel", "c", nil, "own `channel` to publish to; repeatable or comma-separated (default: the config file's, else default)")
	send.Flags().BoolVar(&high, "high", false, "send as high priority (heads-up, may wake the phone)")
	send.Flags().BoolVar(&receipts, "receipts", false, "ask devices to report when they show and open it, and print its ID for notify status")
	send.Flags().Var(&waitSeen, "wait-seen", "wait up to `duration` (10s to 24h) until a device opens it, and exit 4 if none does; implies --receipts")
	send.Flags().Lookup("wait-seen").NoOptDefVal = bareSeenWait

	var askTo, askChannels, options []string
	var askHigh, anyone bool
	var wait time.Duration
	ask := &cobra.Command{
		Use:   "ask [flags] QUESTION [BODY | -]",
		Short: "Ask a question and print the answer",
		Long: "Send a question with up to three answers, as buttons on your devices,\n" +
			"and wait for the first one tapped. It prints the answer on standard\n" +
			"output and exits 0, or exits 4 when nobody answered in time (3 when\n" +
			"the question reached no device). Only your own devices may answer,\n" +
			"unless --anyone lets other members of the channels answer too.\n\n" +
			"Targets, BODY and flag order work as for send.",
		Example: "  if [ \"$(notify ask -o yes,no 'Deploy prod?')\" = yes ]; then make deploy; fi\n" +
			"  notify ask -o restart,ignore --wait 1h -c homelab --anyone \"Disk full\" -",
		Args:        cobra.RangeArgs(1, 2),
		Annotations: map[string]string{flagsEndAtArgs: "yes"},
	}
	ask.RunE = withSettings(true, func(args []string) int {
		var h *bool
		if ask.Flags().Changed("high") {
			h = &askHigh
		}
		return c.askCmd(args, splitList(askTo), splitList(askChannels), splitList(options), h, wait, anyone)
	})
	ask.Flags().SetInterspersed(false)
	ask.Flags().StringArrayVarP(&options, "options", "o", nil, "the `answers` to offer, at most 3; repeatable or comma-separated")
	ask.Flags().DurationVar(&wait, "wait", 10*time.Minute, "how long the question stays open (10s to 24h)")
	ask.Flags().BoolVar(&anyone, "anyone", false, "let other channel members' devices answer too")
	ask.Flags().StringArrayVar(&askTo, "to", nil, "device `name or ID` to ask; repeatable or comma-separated")
	ask.Flags().StringArrayVarP(&askChannels, "channel", "c", nil, "own `channel` to ask in; repeatable or comma-separated (default: the config file's, else default)")
	ask.Flags().BoolVar(&askHigh, "high", false, "send as high priority (heads-up, may wake the phone)")
	_ = ask.MarkFlagRequired("options")

	var force bool
	signup := leaf("signup INVITE NAME", "Create an account with an invite and save its token", cobra.ExactArgs(2), func(args []string) int {
		return c.signup(args, force)
	})
	signup.Flags().BoolVar(&force, "force", false, "replace a user token already in the config file")

	var forceConfig bool
	configInit := &cobra.Command{
		Use:   "init",
		Short: "Write the config file from the given --url, --token and --key",
		Args:  cobra.NoArgs,
		RunE:  withSettings(false, func([]string) int { return c.configInit(forceConfig) }),
	}
	configInit.Flags().BoolVar(&forceConfig, "force", false, "replace an existing config file")
	config := group(&cobra.Command{Use: "config", Short: "Manage the config file"})
	config.AddCommand(configInit)

	var keyScope []string
	keyCreate := leaf("create [-c CHANNEL]... NAME", "Create an API key, shown once", cobra.ExactArgs(1), func(args []string) int {
		return c.keyCreate(args[0], splitList(keyScope))
	})
	keyCreate.Flags().StringArrayVarP(&keyScope, "channel", "c", nil, "limit the key to this own `channel`; repeatable or comma-separated")
	key := group(&cobra.Command{Use: "key", Short: "Create or delete an API key"})
	key.AddCommand(keyCreate,
		leaf("delete NAME|ID", "Delete an API key", cobra.ExactArgs(1), func(args []string) int { return c.keyDelete(args[0]) }))

	channel := group(&cobra.Command{Use: "channel", Short: "Manage channels and their members"})
	for _, a := range []struct{ use, short string }{
		{"create NAME", "Create a channel"},
		{"delete NAME", "Delete a channel you own"},
		{"invite NAME", "Invite someone to a channel you own"},
		{"members NAME", "List a channel's members"},
		{"leave NAME", "Leave a channel you joined"},
		{"join CODE", "Join a channel with an invite code"},
		{"remove NAME MEMBER", "Remove a member from a channel you own"},
	} {
		action := strings.Fields(a.use)[0]
		n := len(strings.Fields(a.use)) - 1
		channel.AddCommand(leaf(a.use, a.short, cobra.ExactArgs(n), func(args []string) int { return c.channel(action, args) }))
	}

	var every, grace, hbChannel string
	hbCreate := leaf("create [--every 1d] [--grace 5m] [-c CHANNEL] NAME", "Create a heartbeat and print its ping URL, once", cobra.ExactArgs(1), func(args []string) int {
		return c.heartbeatCreate(args[0], every, grace, hbChannel)
	})
	hbCreate.Flags().StringVar(&every, "every", "1d", "expected time between check-ins (`duration`: 30m, 6h, 1d)")
	hbCreate.Flags().StringVar(&grace, "grace", "", "extra time allowed before alerting (`duration`; default 5m)")
	hbCreate.Flags().StringVarP(&hbChannel, "channel", "c", "", "alert this `channel`, which you own (default: default)")
	heartbeat := group(&cobra.Command{Use: "heartbeat", Short: "Create or delete a heartbeat (dead-man's switch)"})
	heartbeat.AddCommand(hbCreate,
		leaf("delete NAME|ID", "Delete a heartbeat", cobra.ExactArgs(1), func(args []string) int { return c.heartbeatDelete(args[0]) }))

	var hookChannel string
	hookCreate := leaf("create [-c CHANNEL] FORMAT NAME", "Create an inbound webhook and print its URL, once", cobra.ExactArgs(2), func(args []string) int {
		return c.hookCreate(args[0], args[1], hookChannel)
	})
	hookCreate.Long = "Create an inbound webhook and print its URL, once.\n\nFORMAT is alertmanager, grafana or generic."
	hookCreate.Flags().StringVarP(&hookChannel, "channel", "c", "", "publish to this `channel`, which you own (default: default)")
	hookCreate.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return []string{"alertmanager", "grafana", "generic"}, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	hook := group(&cobra.Command{Use: "hook", Short: "Create or delete an inbound webhook"})
	hook.AddCommand(hookCreate,
		leaf("delete NAME|ID", "Delete an inbound webhook", cobra.ExactArgs(1), func(args []string) int { return c.hookDelete(args[0]) }))

	noArgs := func(fn func() int) func([]string) int { return func([]string) int { return fn() } }
	root.AddCommand(
		send,
		ask,
		leaf("status ID", "Show when devices showed and opened a message sent with receipts", cobra.ExactArgs(1), func(args []string) int {
			return c.status(args[0])
		}),
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Args:  cobra.NoArgs,
			Run:   func(*cobra.Command, []string) { fmt.Fprintln(c.stdout, version) },
		},
		leaf("devices", "List your devices", cobra.NoArgs, noArgs(c.devices)),
		leaf("pair", "Pair a device: print a QR code and a code to enter", cobra.NoArgs, noArgs(c.pair)),
		leaf("invite", "Create a sign-up invite", cobra.NoArgs, noArgs(c.invite)),
		signup,
		config,
		leaf("keys", "List your API keys", cobra.NoArgs, noArgs(c.keys)),
		key,
		leaf("channels", "List your channels", cobra.NoArgs, noArgs(c.channels)),
		channel,
		leaf("heartbeats", "List your heartbeats", cobra.NoArgs, noArgs(c.heartbeats)),
		heartbeat,
		leaf("hooks", "List your inbound webhooks", cobra.NoArgs, noArgs(c.hooks)),
		hook,
	)
	return root
}

// group makes [cmd] a command that only holds others: alone, or with an
// unknown subcommand, it prints its usage and exits 2.
func group(cmd *cobra.Command) *cobra.Command {
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "notify: unknown command %q for %q\n", args[0], cmd.CommandPath())
		}
		fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
		return exitCode(exitUsage)
	}
	return cmd
}

// splitList splits the values of a repeatable, comma-separated flag,
// trimmed and without empty ones. A StringArray flag, not pflag's
// StringSlice, which parses values as CSV and so refuses quotes.
func splitList(values []string) []string {
	var out []string
	for _, v := range values {
		for part := range strings.SplitSeq(v, ",") {
			if part = strings.TrimSpace(part); part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}
