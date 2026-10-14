// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// exitCode ends a command with that exit status; what went wrong has
// already been reported.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// newRoot builds the command tree around [a].
func (a *app) newRoot() *cobra.Command {
	// leaf resolves the state and history paths, then runs [fn].
	leaf := func(use, short string, args cobra.PositionalArgs, fn func(args []string) int) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: args, RunE: func(_ *cobra.Command, args []string) error {
			code := a.paths()
			if code == 0 {
				code = fn(args)
			}
			if code != 0 {
				return exitCode(code)
			}
			return nil
		}}
	}
	none := func(fn func() int) func([]string) int { return func([]string) int { return fn() } }

	root := group(&cobra.Command{
		Use:   "notifyd",
		Short: "Show notifier messages on this Linux desktop",
		Long: "notifyd shows notifier messages on this Linux desktop.\n\n" +
			"Pair with the notifier://pair link (or the code) from `notify pair`, then\n" +
			"`notifyd run`, usually as a systemd user service. The pairing is kept in\n" +
			"$XDG_CONFIG_HOME/notifier/notifyd.json (override with NOTIFYD_STATE), and\n" +
			"what it received in $XDG_STATE_HOME/notifier/notifyd-history.jsonl (override\n" +
			"with NOTIFYD_HISTORY).",
		SilenceErrors: true,
		SilenceUsage:  true,
	})

	var po pairOptions
	pair := leaf("pair [--name NAME] [--force] LINK | --server URL CODE", "Pair with a server", cobra.ExactArgs(1), func(args []string) int {
		return a.pair(args[0], po)
	})
	pair.Long = "Pair with the notifier://pair link from `notify pair`, or with --server\nand the code it prints."
	pair.Flags().StringVar(&po.server, "server", "", "server URL, when pairing with a bare `code`")
	pair.Flags().StringVar(&po.name, "name", "", "device `name` (default: this machine's hostname)")
	pair.Flags().BoolVar(&po.force, "force", false, "replace an existing pairing, unpairing it first")

	var count int
	var asJSON bool
	history := leaf("history [-n COUNT] [--json]", "Print the messages received", cobra.NoArgs, func([]string) int {
		return a.history(count, asJSON)
	})
	history.Flags().IntVarP(&count, "count", "n", 20, "show the `count` newest messages; 0 shows all")
	history.Flags().BoolVar(&asJSON, "json", false, "print the entries as a JSON array, newest first")
	history.AddCommand(
		leaf("clear", "Forget the messages received", cobra.NoArgs, none(a.historyClear)),
		leaf("delete ID...", "Forget the messages with these IDs", cobra.MinimumNArgs(1), a.historyDelete),
	)

	// Answers are the sender's text and may start with a dash.
	reply := leaf("reply ID ANSWER", "Answer a question from the history", cobra.ExactArgs(2), func(args []string) int {
		return a.reply(args[0], args[1])
	})
	reply.Flags().SetInterspersed(false)
	reply.Annotations = map[string]string{flagsEndAtArgs: "yes"}

	root.AddCommand(
		pair,
		// Never the default: a second daemon would replace the service's.
		leaf("run", "Receive and show messages until stopped (the systemd service)", cobra.NoArgs, none(a.listen)),
		leaf("test", "Show a test notification through the server", cobra.NoArgs, none(a.test)),
		reply,
		history,
		leaf("unpair", "Unpair this desktop and forget its pairing", cobra.NoArgs, none(a.unpair)),
		&cobra.Command{
			Use:   "version",
			Short: "Print the version",
			Args:  cobra.NoArgs,
			Run:   func(*cobra.Command, []string) { fmt.Fprintln(a.stdout, "notifyd", version) },
		},
	)
	return root
}

// group makes [cmd] a command that only holds others: alone, or with an
// unknown subcommand, it prints its usage and exits 2. Bare notifyd must
// not run (issue #22).
func group(cmd *cobra.Command) *cobra.Command {
	cmd.Args = cobra.ArbitraryArgs
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "notifyd: unknown command %q for %q\n", args[0], cmd.CommandPath())
		}
		fmt.Fprint(cmd.ErrOrStderr(), cmd.UsageString())
		return exitCode(exitUsage)
	}
	return cmd
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
