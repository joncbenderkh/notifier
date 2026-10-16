// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/joncbenderkh/notifier/notifyd/internal/histfile"
)

func TestOffersSnoozeWhenAsked(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	snoozed := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(id string) bool { snoozed <- id; return true }, snoozeFor: time.Hour,
	}
	show := func(p Payload) []string {
		t.Helper()
		if err := d.show(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		return bus.call(t)[5].([]string)
	}
	// With a default that does nothing, so dunst's middle click still dismisses it.
	if a := show(Payload{ID: "msg_1", Title: "Disk full"}); !slices.Equal(a, []string{openAction, "Dismiss", snoozeAction, "Snooze 1 h"}) {
		t.Fatalf("actions %q", a)
	}
	if a := show(Payload{ID: "msg_l", Title: "Build", URL: "https://ci.example/"}); !slices.Equal(a, []string{openAction, "Open", snoozeAction, "Snooze 1 h"}) {
		t.Fatalf("actions %q", a)
	}
	q := testQuestion
	if a := show(Payload{ID: "msg_2", Title: "Deploy?", Reply: &q}); slices.Contains(a, snoozeAction) { // 3
		t.Fatalf("an open question offers %q", a)
	}
	expired := histfile.Reply{Options: q.Options, Key: q.Key, ExpiresAt: testNow - 1}
	if a := show(Payload{ID: "msg_3", Title: "Deploy?", Reply: &expired}); !slices.Contains(a, snoozeAction) {
		t.Fatalf("an expired question offers %q", a)
	}
	invoke := func(nid uint32) {
		d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{nid, snoozeAction}})
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), openAction}})
	invoke(3) // not offered
	invoke(1)
	if id := <-snoozed; id != "msg_1" {
		t.Fatalf("snoozed %s", id)
	}
	// Closed, as some services keep a notification after an action.
	deadline := time.Now().Add(time.Second)
	for {
		bus.mu.Lock()
		closed := slices.Clone(bus.closed)
		bus.mu.Unlock()
		if slices.Equal(closed, []uint32{1}) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("closed %v", closed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	invoke(1) // gone with the click
	select {
	case id := <-snoozed:
		t.Fatalf("snoozed %s again", id)
	case <-time.After(50 * time.Millisecond):
	}

	d.snoozeFor = 0
	if a := show(Payload{ID: "msg_4", Title: "Disk full"}); len(a) != 0 {
		t.Fatalf("actions %q without --snooze", a)
	}
}

func TestSnoozeLabel(t *testing.T) {
	for d, want := range map[time.Duration]string{time.Hour: "1 h", 15 * time.Minute: "15 min", 90 * time.Minute: "90 min", 90 * time.Second: "1m30s"} {
		if got := snoozeLabel(d); got != want {
			t.Errorf("%s: %q", d, got)
		}
	}
}

// fakeTimers runs the functions [snoozer] schedules when told to,
// moving the clock, if given, to the last one's time first.
type fakeTimers struct {
	now    *time.Time
	mu     sync.Mutex
	queued []func()
	delays []time.Duration
	until  time.Time
}

func (f *fakeTimers) after(d time.Duration, fn func()) *time.Timer {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queued = append(f.queued, fn)
	f.delays = append(f.delays, d)
	if f.now != nil && f.now.Add(d).After(f.until) {
		f.until = f.now.Add(d)
	}
	return time.NewTimer(time.Hour) // stoppable, never fires in a test
}

func (f *fakeTimers) runAll() {
	f.mu.Lock()
	queued := f.queued
	f.queued = nil
	if f.now != nil && f.until.After(*f.now) {
		*f.now = f.until
	}
	f.mu.Unlock()
	for _, fn := range queued {
		fn()
	}
}

func newTestSnoozer(t *testing.T) (*snoozer, *fakeDesktop, *fakeTimers, *time.Time) {
	t.Helper()
	now := time.Unix(testNow, 0)
	h := newHistory(filepath.Join(t.TempDir(), "history.jsonl"), func() time.Time { return now })
	if _, err := h.record(Payload{ID: "msg_1", Title: "Disk full", Body: "98%", SentAt: testNow, URL: "https://nas.example/"}); err != nil {
		t.Fatal(err)
	}
	desktop := newFakeDesktop()
	timers := &fakeTimers{now: &now}
	s := newSnoozer(context.Background(), h, time.Hour, func() time.Time { return now }, t.Logf)
	s.desktop, s.afterFunc = desktop, timers.after
	return s, desktop, timers, &now
}

func TestSnoozeShowsTheMessageAgain(t *testing.T) {
	s, desktop, timers, _ := newTestSnoozer(t)
	s.snooze("msg_1")
	if timers.delays[0] != time.Hour {
		t.Fatalf("armed for %s", timers.delays[0])
	}
	due, _ := s.history.reminders()
	if len(due) != 1 || due[0].RemindAt != testNow+3600 {
		t.Fatalf("reminders %+v", due)
	}
	timers.runAll()
	p := desktop.next(t)
	if p.ID != "msg_1" || p.Body != "98%" || p.URL != "https://nas.example/" || p.Receipts || p.Reply != nil {
		t.Fatalf("shown %+v", p)
	}
	if due, _ := s.history.reminders(); len(due) != 0 {
		t.Fatalf("still due %+v", due)
	}
}

func TestReminderOutlivesTheDaemon(t *testing.T) {
	s, _, _, now := newTestSnoozer(t)
	s.snooze("msg_1")
	*now = now.Add(2 * time.Hour)
	// The next daemon shows it at once.
	next := newSnoozer(context.Background(), s.history, 0, func() time.Time { return *now }, t.Logf)
	desktop, timers := newFakeDesktop(), &fakeTimers{}
	next.desktop, next.afterFunc = desktop, timers.after
	next.start()
	if len(timers.delays) != 1 || timers.delays[0] != 0 {
		t.Fatalf("armed %v", timers.delays)
	}
	timers.runAll()
	if p := desktop.next(t); p.ID != "msg_1" {
		t.Fatalf("shown %+v", p)
	}
}

func TestDeletingTheEntryCancelsItsReminder(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	withHistoryLock(s.history.path, func() error { _, err := histfile.Delete(s.history.path, *now, "msg_1"); return err })
	timers.runAll()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown %+v", p)
	default:
	}
	s.snooze("msg_gone")
	if len(timers.delays) != 1 {
		t.Fatal("armed a reminder for a message not in the history")
	}
}

func TestSnoozingAgainReplacesTheReminder(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	first := timers.queued[0]
	*now = now.Add(10 * time.Minute)
	s.snooze("msg_1")
	*now = now.Add(time.Hour)
	first() // the first time no longer holds
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown %+v", p)
	default:
	}
	timers.queued[1]()
	desktop.next(t)
}

func TestReminderWaitsForTheDesktop(t *testing.T) {
	s, desktop, timers, _ := newTestSnoozer(t)
	desktop.fail.Store(1)
	s.snooze("msg_1")
	timers.runAll()
	if len(timers.delays) != 2 || timers.delays[1] != reminderRetry {
		t.Fatalf("armed %v", timers.delays)
	}
	timers.runAll()
	desktop.next(t)
}

func TestRunChecksSnooze(t *testing.T) {
	for _, args := range [][]string{
		{"--snooze", "30s"}, {"--snooze", "25h"}, {"--snooze", "-1h"},
		// Nor restarted for a flag or an argument it cannot take.
		{"--snooze", "15"}, {"--snoze", "1h"}, {"now"}, {"-snooze", "1h"},
	} {
		a, _, stderr := newTestApp(t)
		if code := a.run(append([]string{"run"}, args...)); code != exitConfig {
			t.Errorf("%v: exit %d: %s", args, code, stderr)
		}
	}
}

func TestDoesNotShowASnoozedMessageDeliveredAgain(t *testing.T) {
	s, _, _, _ := newTestSnoozer(t)
	s.snooze("msg_1")
	if snoozed, err := s.history.record(Payload{ID: "msg_1", Title: "Disk full"}); err != nil || !snoozed {
		t.Fatalf("snoozed %v, %v", snoozed, err)
	}
	if snoozed, _ := s.history.record(Payload{ID: "msg_2", Title: "Other"}); snoozed {
		t.Fatal("a new message counted as snoozed")
	}
}

func TestOpeningKeepsSnoozeOffered(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	snoozed := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(id string) bool { snoozed <- id; return true }, snoozeFor: time.Hour,
		open: func(string) {},
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Build", URL: "https://ci.example/"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t)
	for _, key := range []string{openAction, snoozeAction} {
		d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), key}})
	}
	if id := <-snoozed; id != "msg_1" {
		t.Fatalf("snoozed %s", id)
	}
}

func TestSnoozeAloneDoesNotStopReplacingInPlace(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(string) bool { return true }, snoozeFor: time.Hour,
	}
	for i, title := range []string{"Disk 80%", "Disk 90%"} {
		if err := d.show(context.Background(), Payload{ID: fmt.Sprint("msg_", i), Title: title, Key: "disk"}); err != nil {
			t.Fatal(err)
		}
		if args := bus.call(t); args[1] != uint32(i) { // 0, then 1
			t.Fatalf("%s replaces %v", title, args[1])
		}
	}
	if len(bus.closed) != 0 {
		t.Fatalf("closed %v", bus.closed)
	}
}

func TestReminderSnoozedAgainWhileShowingStays(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	first := timers.queued[0]
	timers.queued = nil
	*now = now.Add(time.Hour)
	// Snoozed again from the reminder before the first one clears its time.
	desktop.onShow = func() { s.snooze("msg_1") }
	first()
	desktop.next(t)
	due, _ := s.history.reminders()
	if len(due) != 1 || due[0].RemindAt != testNow+2*3600 {
		t.Fatalf("reminders %+v", due)
	}
	if len(s.timers) != 1 {
		t.Fatalf("timers %v", s.timers)
	}
}

func TestForgetsTheTimerOfACancelledReminder(t *testing.T) {
	s, _, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	withHistoryLock(s.history.path, func() error { _, err := histfile.Delete(s.history.path, *now, "msg_1"); return err })
	timers.runAll()
	if len(s.timers) != 0 {
		t.Fatalf("timers %v", s.timers)
	}
}

func TestKeepsASnoozedMessagePastTheAgeLimit(t *testing.T) {
	s, _, _, now := newTestSnoozer(t)
	s.snooze("msg_1")
	*now = now.Add(histfile.MaxAge + time.Hour)
	if due, _ := s.history.reminders(); len(due) != 1 {
		t.Fatalf("reminders %+v", due)
	}
}

func TestSnoozeAfterRepairing(t *testing.T) {
	s, _, timers, _ := newTestSnoozer(t)
	var logged []string
	s.logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	s.history.current = func() bool { return false }
	s.snooze("msg_1")
	if len(timers.delays) != 0 || len(logged) != 1 || !strings.Contains(logged[0], "paired again") {
		t.Fatalf("armed %v, logged %q", timers.delays, logged)
	}
}

func TestSaysASnoozeFailed(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	tried := make(chan string, 2)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(id string) bool { tried <- id; return false }, snoozeFor: time.Hour,
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Disk full"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t)
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), snoozeAction}})
	<-tried
	// In its place, and not closed.
	args := bus.call(t)
	if args[1] != uint32(1) || args[3] != "Could not snooze it" || args[4] != "Disk full" {
		t.Fatalf("outcome %v", args)
	}
	if len(bus.closed) != 0 {
		t.Fatalf("closed %v", bus.closed)
	}
}

func TestSnoozingAKeyedMessageLetsTheNextShowAnew(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	release := make(chan struct{})
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(string) bool { <-release; return true }, snoozeFor: time.Hour,
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Disk 90%", Key: "disk"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t) // 1
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), snoozeAction}})
	// The update comes before the snoozed one is closed: it must not take its place.
	if err := d.show(context.Background(), Payload{ID: "msg_2", Title: "Disk 95%", Key: "disk"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(0) {
		t.Fatalf("replaces %v", args[1])
	}
	close(release)
}

func TestKeepsReminderPastTheEntryLimit(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	for i := range histfile.MaxEntries + 100 {
		*now = now.Add(time.Second)
		if _, err := s.history.record(Payload{ID: fmt.Sprint("msg_n", i), Title: "noise"}); err != nil {
			t.Fatal(err)
		}
	}
	timers.runAll()
	if p := desktop.next(t); p.ID != "msg_1" {
		t.Fatalf("shown %+v", p)
	}
}

func TestShowsDueRemindersAfterASuspend(t *testing.T) {
	s, desktop, _, now := newTestSnoozer(t)
	s.snooze("msg_1")
	// The timer has not fired: it did not count the suspend.
	s.due()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown early %+v", p)
	default:
	}
	*now = now.Add(time.Hour)
	s.due()
	if p := desktop.next(t); p.ID != "msg_1" {
		t.Fatalf("shown %+v", p)
	}
	s.due() // once
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown twice %+v", p)
	default:
	}
}

func TestWaitsForTheTimeAfterTheClockWentBack(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	s.snooze("msg_1")
	fire := timers.queued[0]
	timers.queued = nil
	// The timer ran its hour, but the clock was set back half of it.
	*now = now.Add(30 * time.Minute)
	fire()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown early %+v", p)
	default:
	}
	if len(timers.delays) != 2 || timers.delays[1] != 30*time.Minute {
		t.Fatalf("armed %v", timers.delays)
	}
	timers.runAll()
	desktop.next(t)
}

func TestShowsAReminderOnceWhenItsTimeCannotBeCleared(t *testing.T) {
	s, desktop, timers, _ := newTestSnoozer(t)
	s.snooze("msg_1")
	markReminded = func(string, time.Time, string, int64) (bool, error) { return false, errors.New("disk full") }
	t.Cleanup(func() { markReminded = histfile.Reminded })
	timers.runAll()
	desktop.next(t)
	if !s.waiting() {
		t.Fatal("the minute check would not try again")
	}
	s.due()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown again %+v", p)
	default:
	}
	markReminded = histfile.Reminded
	s.due()
	if due, _ := s.history.reminders(); len(due) != 0 || s.waiting() {
		t.Fatalf("still due %+v", due)
	}
}

func TestDropsAReminderANewerMessageReplaced(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	if _, err := s.history.record(Payload{ID: "msg_k1", Title: "Disk 90%", Key: "disk", ChannelID: "ch_1"}); err != nil {
		t.Fatal(err)
	}
	s.snooze("msg_k1")
	*now = now.Add(time.Second)
	// Another channel's key does not count.
	if _, err := s.history.record(Payload{ID: "msg_k2", Title: "Load", Key: "disk", ChannelID: "ch_2"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.history.reminders(); len(due) != 1 {
		t.Fatalf("reminders %+v", due)
	}
	if _, err := s.history.record(Payload{ID: "msg_k3", Title: "Disk 95%", Key: "disk", ChannelID: "ch_1"}); err != nil {
		t.Fatal(err)
	}
	if due, _ := s.history.reminders(); len(due) != 0 {
		t.Fatalf("reminders %+v", due)
	}
	timers.runAll()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown %+v", p)
	default:
	}
	// Nor can it be snoozed now.
	if s.snooze("msg_k1") {
		t.Fatal("snoozed a replaced message")
	}
	if !s.snooze("msg_k3") {
		t.Fatal("could not snooze the newest")
	}
	timers.runAll()
	if p := desktop.next(t); p.ID != "msg_k3" || p.Key != "disk" {
		t.Fatalf("shown %+v", p)
	}
}

func TestReadsTheRemindersUntilItCan(t *testing.T) {
	s, desktop, timers, _ := newTestSnoozer(t)
	s.snooze("msg_1")
	next := newSnoozer(context.Background(), s.history, 0, s.now, t.Logf)
	next.desktop, next.afterFunc = desktop, timers.after
	readHistory = func(string, time.Time) ([]historyEntry, error) { return nil, errors.New("unreadable") }
	t.Cleanup(func() { readHistory = histfile.Read })
	timers.queued, timers.delays = nil, nil
	next.start()
	if len(timers.delays) != 0 || !next.waiting() {
		t.Fatalf("armed %v, waiting %v", timers.delays, next.waiting())
	}
	readHistory = histfile.Read
	next.due()
	if len(timers.delays) != 1 || timers.delays[0] != time.Hour {
		t.Fatalf("armed %v", timers.delays)
	}
	timers.runAll()
	desktop.next(t)
}

func TestNeverShowsAReminderOverItsReplacement(t *testing.T) {
	s, desktop, timers, now := newTestSnoozer(t)
	// As if dropping A's reminder failed when B came.
	entries := []historyEntry{
		{ID: "msg_b", Title: "Disk 95%", Key: "disk", ChannelID: "ch_1", ReceivedAt: now.Unix()},
		{ID: "msg_a", Title: "Disk 90%", Key: "disk", ChannelID: "ch_1", ReceivedAt: now.Unix() - 1, RemindAt: now.Unix()},
	}
	if err := histfile.Write(s.history.path, entries); err != nil {
		t.Fatal(err)
	}
	s.start()
	timers.runAll()
	select {
	case p := <-desktop.shown:
		t.Fatalf("shown %+v", p)
	default:
	}
}

func TestOrdersReplacementsByArrivalNotTheClock(t *testing.T) {
	s, _, _, now := newTestSnoozer(t)
	if _, err := s.history.record(Payload{ID: "msg_a", Title: "Disk 90%", Key: "disk", ChannelID: "ch_1"}); err != nil {
		t.Fatal(err)
	}
	// The clock was set back before B came.
	*now = now.Add(-time.Hour)
	if _, err := s.history.record(Payload{ID: "msg_b", Title: "Disk 95%", Key: "disk", ChannelID: "ch_1"}); err != nil {
		t.Fatal(err)
	}
	if s.snooze("msg_a") {
		t.Fatal("snoozed the replaced message")
	}
	if !s.snooze("msg_b") {
		t.Fatal("could not snooze its replacement")
	}
}

func TestForgetsTheTimerOfADeletedReminder(t *testing.T) {
	s, _, _, now := newTestSnoozer(t)
	s.snooze("msg_1")
	withHistoryLock(s.history.path, func() error { _, err := histfile.Delete(s.history.path, *now, "msg_1"); return err })
	s.due()
	if s.waiting() {
		t.Fatal("the minute check still reads the history")
	}
}

func TestALoneLinkStaysTheMiddleClick(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	opened := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(string) bool { return true }, snoozeFor: time.Hour,
		open: func(url string) { opened <- url },
	}
	p := Payload{ID: "msg_1", Title: "Build failed", Actions: []histfile.Action{{Label: "Logs", URL: "https://ci.example/1"}}}
	if err := d.show(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	want := []string{linkAction + "0", "Logs", openAction, "Logs", snoozeAction, "Snooze 1 h"}
	if a := bus.call(t)[5].([]string); !slices.Equal(a, want) {
		t.Fatalf("actions %q", a)
	}
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), openAction}})
	if url := <-opened; url != "https://ci.example/1" {
		t.Fatalf("opened %s", url)
	}
}

func TestDoesNotCloseASnoozedNotificationTheServiceClosed(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	release, saved := make(chan struct{}), make(chan struct{})
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(string) bool { <-release; defer close(saved); return true }, snoozeFor: time.Hour,
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Disk full"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t)
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), snoozeAction}})
	// Its ID may now be another notification's.
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".NotificationClosed", Body: []any{uint32(1), uint32(2)}})
	close(release)
	<-saved
	time.Sleep(50 * time.Millisecond)
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.closed) != 0 {
		t.Fatalf("closed %v", bus.closed)
	}
}

func TestAFailedSnoozeKeepsTheKey(t *testing.T) {
	bus := &replacingBus{fakeBus: fakeBus{calls: make(chan []any, 10)}}
	tried := make(chan string, 1)
	d := &dbusDesktop{
		object: func(string) dbus.BusObject { return bus }, actions: true, owner: ":1.5", pending: map[uint32]question{},
		keyed:  map[string]keyedNote{},
		now:    func() time.Time { return time.Unix(testNow, 0) },
		snooze: func(id string) bool { tried <- id; return false }, snoozeFor: time.Hour,
	}
	if err := d.show(context.Background(), Payload{ID: "msg_1", Title: "Disk 90%", Key: "disk"}); err != nil {
		t.Fatal(err)
	}
	bus.call(t)
	d.signal(0, &dbus.Signal{Sender: ":1.5", Name: notificationsInterface + ".ActionInvoked", Body: []any{uint32(1), snoozeAction}})
	<-tried
	if args := bus.call(t); args[3] != "Could not snooze it" {
		t.Fatalf("outcome %v", args)
	}
	// The next message with its key takes the outcome's place.
	if err := d.show(context.Background(), Payload{ID: "msg_2", Title: "Disk 95%", Key: "disk"}); err != nil {
		t.Fatal(err)
	}
	if args := bus.call(t); args[1] != uint32(1) {
		t.Fatalf("replaces %v", args[1])
	}
}

func TestRecordsAMessageAsNewestWhenTheHistoryCannotBeRead(t *testing.T) {
	now := time.Unix(testNow, 0)
	path := filepath.Join(t.TempDir(), "history.jsonl")
	h := newHistory(path, func() time.Time { return now })
	for _, id := range []string{"msg_1", "msg_2"} {
		if _, err := h.record(Payload{ID: id, Title: "Disk", Key: "disk", SentAt: testNow}); err != nil {
			t.Fatal(err)
		}
	}
	readHistory = func(string, time.Time) ([]historyEntry, error) { return nil, errors.New("unreadable") }
	t.Cleanup(func() { readHistory = histfile.Read })
	h.record(Payload{ID: "msg_3", Title: "Disk", Key: "disk", SentAt: testNow})
	readHistory = histfile.Read
	entries, err := histfile.Read(path, now)
	if err != nil {
		t.Fatal(err)
	}
	i := slices.IndexFunc(entries, func(e historyEntry) bool { return e.ID == "msg_3" })
	if i < 0 || histfile.Replaced(entries, i) {
		t.Fatalf("msg_3 replaced in %+v", entries)
	}
}

func TestARetryLeavesASnoozeMadeSince(t *testing.T) {
	s, _, _, now := newTestSnoozer(t)
	s.snooze("msg_1")
	first := now.Add(time.Hour).Unix()
	*now = now.Add(time.Minute)
	s.snooze("msg_1")
	// The first reminder's retry, after its read or showing failed.
	s.rearm("msg_1", first, reminderRetry)
	s.mu.Lock()
	r := s.timers["msg_1"]
	s.mu.Unlock()
	if r.at != first+60 {
		t.Fatalf("armed for %d, want %d", r.at, first+60)
	}
}

func TestMarksAShownReminderAsNotDelayed(t *testing.T) {
	s, desktop, timers, _ := newTestSnoozer(t)
	s.snooze("msg_1")
	timers.runAll()
	desktop.next(t)
	entries, err := histfile.Read(s.history.path, s.now())
	if err != nil {
		t.Fatal(err)
	}
	if !entries[0].Reminded || strings.Contains(formatEntry(entries[0]), "(sent") {
		t.Fatalf("entry %+v: %q", entries[0], formatEntry(entries[0]))
	}
}
