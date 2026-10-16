// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
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
			s.fire(e.ID, e.RemindAt)
		} else if rescan {
			s.arm(e.ID, e.RemindAt, s.until(e.RemindAt))
		}
	}
}

// snooze has message id shown again in s.after, and reports whether it will be.
func (s *snoozer) snooze(id string) bool {
	at := s.now().Add(s.after).Unix()
	found, err := s.history.remind(id, at)
	switch {
	case err != nil:
		s.logf("could not snooze %s: %v", id, err)
	case !found:
		s.logf("could not snooze %s: it is not in the history, or a newer message with its key replaced it", id)
	default:
		s.mu.Lock()
		delete(s.shown, id)
		s.mu.Unlock()
		s.arm(id, at, s.until(at))
	}
	return err == nil && found
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

func (s *snoozer) armLocked(id string, at int64, delay time.Duration) {
	if r, ok := s.timers[id]; ok {
		r.timer.Stop()
	}
	s.arms++
	s.timers[id] = reminder{s.afterFunc(delay, func() { s.fire(id, at) }), at, s.arms}
}

func (s *snoozer) fire(id string, at int64) {
	s.fireMu.Lock()
	defer s.fireMu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	due, err := s.history.reminders()
	if err != nil {
		s.logf("could not read the reminder for %s: %v", id, err)
		s.rearm(id, at, reminderRetry)
		return
	}
	var e *historyEntry
	for i := range due {
		if due[i].ID == id && due[i].RemindAt == at {
			e = &due[i]
		}
	}
	if e == nil { // deleted, cleared, shown, replaced, or snoozed again since
		s.mu.Lock()
		if s.shown[id] == at {
			delete(s.shown, id)
		}
		s.mu.Unlock()
		s.forget(id, at)
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
	p := Payload{
		ID: e.ID, Title: e.Title, Body: e.Body, Priority: e.Priority, SentAt: e.SentAt,
		Channel: e.Channel, ChannelID: e.ChannelID, URL: e.URL, Actions: e.Actions, Key: e.Key,
	}
	if err := s.desktop.Notify(s.ctx, p); errors.Is(err, errRejected) {
		s.logf("the desktop refused the reminder for %s, so it is dropped: %v", id, err)
	} else if err != nil {
		s.logf("could not show the reminder for %s: %v", id, err)
		s.rearm(id, at, reminderRetry)
		return
	}
	s.clear(id, at)
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
