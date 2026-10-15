// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Receipts (`notify send --receipts`, `--wait-seen`, `notify status`):
// devices report when they show and open a message sent with receipts, and
// the server keeps the times for a day.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"time"
)

const (
	exitNotSeen = 4

	defaultSeenWait = 10 * time.Minute

	// noReceipts is the server's answer for a message it has no receipts of
	// (NO_RECEIPTS in server/src/app.ts), unlike a missing route's.
	noReceipts = "no receipts for this message"

	noTrackedHint = "no device reports receipts for this message: none of yours got it, " +
		"and members' devices report only where the channel's owner requires receipts, " +
		"or makes them optional and the member turns them on"
)

// bareSeenWait is what --wait-seen holds when given without a duration;
// help shows it, and nobody types it.
const bareSeenWait = "(10m)"

// seenWait is --wait-seen's value.
type seenWait struct{ d time.Duration }

func (w *seenWait) Set(s string) error {
	if s == bareSeenWait {
		// Without a duration it keeps one given before.
		if w.d == 0 {
			w.d = defaultSeenWait
		}
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	w.d = d
	return nil
}

func (w *seenWait) String() string {
	if w.d == 0 {
		return ""
	}
	return w.d.String()
}

func (w *seenWait) Type() string { return "duration" }

// seenClock is the clock --wait-seen counts down by; tests speed it up.
var seenClock = time.Now

// awaitSeen polls the receipts of [id] until a device opens it or [wait]
// has passed.
func (c *cli) awaitSeen(client *Client, id string, wait time.Duration) int {
	deadline := seenClock().Add(wait)
	code, settled, lastErr := c.longPoll(wait, func(ctx context.Context) (int, bool, error) {
		left := int(math.Ceil(deadline.Sub(seenClock()).Seconds()))
		r, err := client.Receipts(ctx, id, min(maxPollSeconds, max(0, left)))
		if err != nil {
			return 0, false, err
		}
		// An answer that came in time counts every opening it shows: they
		// all happened before it. One that came late, as a poll can, counts
		// only the sender's devices that opened it by the deadline; other
		// members' openings carry no time. The server's clock stamps them,
		// so clocks apart by more than the wait can still mislead it.
		late := !seenClock().Before(deadline)
		by := int64(math.MaxInt64)
		if late {
			by = deadline.Unix()
		}
		if name, ok := openedBy(r, by); ok {
			fmt.Fprintln(c.stdout, name)
			return 0, true, nil
		}
		if r.Members.Opened > 0 {
			if !late {
				fmt.Fprintln(c.stdout, "opened on another member's device")
				return 0, true, nil
			}
			// Seen, perhaps in time: neither "seen" nor "not seen" is known.
			fmt.Fprintf(c.stderr, "notify: opened on another member's device, but perhaps not within %s; see notify status %s\n", wait, clean(id))
			return exitError, true, nil
		}
		if late {
			fmt.Fprintf(c.stderr, "notify: not opened within %s\n", wait)
			return exitNotSeen, true, nil
		}
		return 0, false, nil
	})
	if settled {
		return code
	}
	if lastErr != nil {
		// It may have been opened; only the server can say.
		fmt.Fprintf(c.stderr, "notify: could not get the receipts of %s: %v\n", clean(id), lastErr)
		return exitError
	}
	fmt.Fprintf(c.stderr, "notify: not opened within %s\n", wait)
	return exitNotSeen
}

// durationLike matches what someone may write for a duration: 300, 5m,
// 5min, 1.5 h; not a title such as "3 failed".
var durationLike = regexp.MustCompile(`(?i)^\d+(\.\d+)?\s*(ns|us|µs|ms|s|secs?|seconds?|m|mins?|minutes?|h|hrs?|hours?|d|days?)?$`)

// waitSeenTakesTitle says whether a bare --wait-seen comes right before
// [title] in the command line [raw], so the title may be its duration.
func waitSeenTakesTitle(raw []string, title string) bool {
	for i, a := range raw {
		if a == "--" {
			return false
		}
		if a == "--wait-seen" && i+1 < len(raw) && raw[i+1] == title {
			return true
		}
	}
	return false
}

// looksLikeDuration says whether a title [s] was perhaps meant as a
// duration for --wait-seen.
func looksLikeDuration(s string) bool {
	_, err := time.ParseDuration(s)
	return err == nil || durationLike.MatchString(s)
}

// openedBy names the first of the sender's devices to open the message by
// [by] (Unix seconds).
func openedBy(r Receipts, by int64) (string, bool) {
	var first *DeviceReceipt
	for i, d := range r.Devices {
		if d.OpenedAt != 0 && d.OpenedAt <= by && (first == nil || d.OpenedAt < first.OpenedAt) {
			first = &r.Devices[i]
		}
	}
	if first == nil {
		return "", false
	}
	return "opened on " + clean(first.Name), true
}

// status prints when each device showed and opened the message [id].
func (c *cli) status(id string) int {
	client, ok := c.sendClient()
	if !ok {
		return exitUsage
	}
	ctx, cancel := c.context()
	defer cancel()
	r, err := client.Receipts(ctx, id, 0)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && apiErr.Message == noReceipts {
		fmt.Fprintf(c.stderr, "notify: no receipts for %s: it was sent without --receipts, over a day ago, or by someone else\n", clean(id))
		return exitError
	}
	if err != nil {
		return fail(c.stderr, err, c.sendCredName)
	}
	for _, d := range r.Devices {
		line := clean(d.Name) + ": "
		switch {
		case d.ShownAt == 0:
			line += "not shown"
		case d.OpenedAt == 0:
			line += "shown " + stamp(d.ShownAt)
		default:
			line += "shown " + stamp(d.ShownAt) + ", opened " + stamp(d.OpenedAt)
		}
		fmt.Fprintln(c.stdout, line)
	}
	if m := r.Members; m.Shown+m.Opened > 0 {
		fmt.Fprintf(c.stdout, "other members' devices: %d shown, %d opened\n", m.Shown, m.Opened)
	} else if len(r.Devices) == 0 && m.Tracked == 0 {
		fmt.Fprintln(c.stdout, noTrackedHint)
	} else if len(r.Devices) == 0 {
		fmt.Fprintln(c.stdout, "not shown on any device yet")
	}
	return 0
}

func stamp(unix int64) string { return time.Unix(unix, 0).Format(time.DateTime + " MST") }
