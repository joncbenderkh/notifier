// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

// Snoozing (run --snooze) is bounded like a question's wait.
const (
	minSnooze = time.Minute
	maxSnooze = 24 * time.Hour
	// reminderRetry is how long a reminder waits for a desktop that could
	// not show it.
	reminderRetry = time.Minute
)

// snoozer shows snoozed messages again when their time comes. The time is
// kept in the history entry, so a reminder outlives the daemon; deleting
// the entry, or clearing the history, cancels it.
type snoozer struct {
	ctx     context.Context
	history *history
	desktop Desktop
	after   time.Duration // how long a snooze lasts
	now     func() time.Time
	logf    func(string, ...any)
	// afterFunc is time.AfterFunc, replaced in tests.
	afterFunc func(time.Duration, func()) *time.Timer

	mu     sync.Mutex
	timers map[string]reminder // by message ID
	// shown holds the reminders shown whose time could not be cleared, by
	// message ID, so the minute check does not show them every minute.
	shown map[string]int64
	// rescan: the reminders could not be read at start, so the minute
	// check reads them until it can.
	rescan bool
	arms   uint64 // timers armed so far
	// failing: the desktop could not show a reminder, so none is tried
	// before then; a desktop that hangs makes each call wait.
	failing time.Time
	// fireMu shows one reminder at a time, so a timer and the minute
	// check cannot both show the same one.
	fireMu sync.Mutex
}

// checkEvery is how often reminders are checked against the clock: a
// timer does not count time the machine spends suspended.
const checkEvery = time.Minute

type reminder struct {
	timer *time.Timer
	at    int64
	armed uint64 // s.arms when armed
}

func newSnoozer(ctx context.Context, h *history, after time.Duration, now func() time.Time, logf func(string, ...any)) *snoozer {
	return &snoozer{ctx: ctx, history: h, after: after, now: now, logf: logf, afterFunc: time.AfterFunc, timers: map[string]reminder{}, shown: map[string]int64{}}
}

// start arms the reminders the history holds, one already due at once,
// and checks them against the clock every minute until s.ctx ends.
func (s *snoozer) start() {
	due, err := s.history.reminders()
	if err != nil {
		s.logf("could not read the reminders: %v", err)
		s.mu.Lock()
		s.rescan = true
		s.mu.Unlock()
	}
	for _, e := range due {
		s.arm(e.ID, e.RemindAt, s.until(e.RemindAt))
	}
	go func() {
		tick := time.NewTicker(checkEvery)
		defer tick.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-tick.C:
				if s.waiting() {
					s.due()
				}
			}
		}
	}()
}

// waiting reports whether a reminder is armed, or the reminders are still
// to be read: every one the history holds is armed once they are, so
// without one the minute check has nothing to read.
func (s *snoozer) waiting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.timers) > 0 || s.rescan
}

// due shows the reminders whose time has come: after a suspend, their
// timers still wait out the time it lasted.
func (s *snoozer) due() {
	s.mu.Lock()
	before := s.arms
	s.mu.Unlock()
	due, err := s.history.reminders()
	if err != nil {
		s.logf("could not read the reminders: %v", err)
		return
	}
	s.mu.Lock()
	rescan := s.rescan
	s.rescan = false
	// Deleted, cleared or replaced since: the minute check need not wait
	// for it. One armed after the read began may be missing from it.
	for id, r := range s.timers {
		if r.armed <= before && !slices.ContainsFunc(due, func(e historyEntry) bool { return e.ID == id && e.RemindAt == r.at }) {
			r.timer.Stop()
			delete(s.timers, id)
			if s.shown[id] == r.at {
				delete(s.shown, id)
			}
		}
	}
	s.mu.Unlock()
	now := s.now().Unix()
	for _, e := range due {
		if e.RemindAt <= now {
			// Once the desktop fails, the rest are armed to try again
			// without calling it (s.failing).
			s.fire(e.ID, e.RemindAt)
		} else if rescan {
			s.armUnlessSince(e.ID, e.RemindAt, before)
		}
	}
}

// snooze has message id shown again in s.after. If it cannot, it returns
// what to show instead; else "".
func (s *snoozer) snooze(id string) (failed string) {
	at := s.now().Add(s.after).Unix()
	found, err := s.history.remind(id, at)
	switch {
	case errors.Is(err, histfile.ErrTooManyReminders):
		s.logf("could not snooze %s: %v", id, err)
		return "Could not snooze it: " + err.Error()
	case err != nil:
		s.logf("could not snooze %s: %v", id, err)
		return "Could not snooze it"
	case !found:
		s.logf("could not snooze %s: it is not in the history, or a newer message with its key replaced it", id)
		return "Could not snooze it: it is not in the history, or a newer message replaced it"
	}
	s.mu.Lock()
	delete(s.shown, id)
	s.mu.Unlock()
	s.arm(id, at, s.until(at))
	return ""
}

func (s *snoozer) until(at int64) time.Duration {
	return max(time.Unix(at, 0).Sub(s.now()), 0)
}

// arm shows message id after delay, if its reminder is still at.
func (s *snoozer) arm(id string, at int64, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armLocked(id, at, delay)
}

// rearm tries message id's reminder at again after delay, unless it was
// snoozed again meanwhile, whose timer stays.
func (s *snoozer) rearm(id string, at int64, delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.timers[id]; ok && r.at != at {
		return
	}
	s.armLocked(id, at, delay)
}

// armUnlessSince arms message id's reminder at, unless a timer was armed
// for it after before: a snooze made since the history was read.
func (s *snoozer) armUnlessSince(id string, at int64, before uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.timers[id]; ok && r.armed > before {
		return
	}
	s.armLocked(id, at, s.until(at))
}

func (s *snoozer) armLocked(id string, at int64, delay time.Duration) {
	if r, ok := s.timers[id]; ok {
		r.timer.Stop()
	}
	s.arms++
	s.timers[id] = reminder{s.afterFunc(delay, func() { s.fire(id, at) }), at, s.arms}
}

// fire shows message id's reminder at, if still due, and reports whether
// the history or the desktop failed, for which it tries again later.
func (s *snoozer) fire(id string, at int64) (failed bool) {
	s.fireMu.Lock()
	defer s.fireMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	s.mu.Lock()
	wait := s.failing.Sub(s.now())
	s.mu.Unlock()
	if wait > 0 {
		s.rearm(id, at, wait)
		return true
	}
	due, err := s.history.reminders()
	if err != nil {
		s.logf("could not read the reminder for %s: %v", id, err)
		s.rearm(id, at, reminderRetry)
		return true
	}
	var e *historyEntry
	for i := range due {
		if due[i].ID == id && due[i].RemindAt == at {
			e = &due[i]
		}
	}
	if e == nil {
		s.gone(id, at)
		return
	}
	// A timer counts elapsed time, so a clock set back since would fire early.
	if s.now().Unix() < at {
		s.rearm(id, at, s.until(at))
		return
	}
	s.mu.Lock()
	shown := s.shown[id] == at
	s.mu.Unlock()
	if shown {
		s.clear(id, at)
		return
	}
	body := e.Body
	// Without its answers, as the Android app says too.
	if e.Reply != nil && s.now().Unix() >= e.Reply.ExpiresAt {
		body = strings.TrimSuffix("The question has expired\n"+body, "\n")
	}
	p := Payload{
		ID: e.ID, Title: e.Title, Body: body, Priority: e.Priority, SentAt: e.SentAt,
		Channel: e.Channel, ChannelID: e.ChannelID, URL: e.URL, Actions: e.Actions, Key: e.Key,
	}
	// Read again as it shows: another process may delete it meanwhile.
	var showErr error
	waiting, err := s.history.whileWaiting(id, at, func() { showErr = s.desktop.Notify(s.ctx, p) })
	if err != nil {
		s.logf("could not read the reminder for %s: %v", id, err)
		s.rearm(id, at, reminderRetry)
		return true
	}
	if !waiting {
		s.gone(id, at)
		return
	}
	if err := showErr; errors.Is(err, errRejected) {
		s.logf("the desktop refused the reminder for %s, so it is dropped: %v", id, err)
	} else if err != nil {
		s.logf("could not show the reminder for %s: %v", id, err)
		s.mu.Lock()
		s.failing = s.now().Add(reminderRetry)
		s.mu.Unlock()
		s.rearm(id, at, reminderRetry)
		return true
	}
	s.clear(id, at)
	return false
}

// gone forgets message id's reminder at, which the history no longer
// holds: deleted, cleared, shown, replaced, or snoozed again since.
func (s *snoozer) gone(id string, at int64) {
	s.mu.Lock()
	if s.shown[id] == at {
		delete(s.shown, id)
	}
	s.mu.Unlock()
	s.forget(id, at)
}

// clear clears message id's reminder, shown, if it is still at: it may
// have been snoozed again from what just showed. One that cannot be is
// tried again at the next check, without showing it again.
func (s *snoozer) clear(id string, at int64) {
	_, err := s.history.reminded(id, at)
	s.mu.Lock()
	if err != nil {
		s.shown[id] = at
	} else {
		delete(s.shown, id)
	}
	s.mu.Unlock()
	if err != nil {
		s.logf("could not clear the reminder for %s: %v", id, err)
		return
	}
	s.forget(id, at)
}

// forget drops the timer that showed id's reminder at, unless it was
// snoozed again meanwhile.
func (s *snoozer) forget(id string, at int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.timers[id]; ok && r.at == at {
		r.timer.Stop() // the minute check may have shown it first
		delete(s.timers, id)
	}
}
