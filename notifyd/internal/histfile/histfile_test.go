// SPDX-License-Identifier: GPL-3.0-or-later

package histfile

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestReminders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1_800_000_000, 0)
	if err := Write(path, []Entry{{ID: "b", ReceivedAt: now.Unix()}, {ID: "a", ReceivedAt: now.Unix() - 1}}); err != nil {
		t.Fatal(err)
	}
	remindAt := func(id string) int64 {
		t.Helper()
		entries, err := Read(path, now)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.ID == id {
				return e.RemindAt
			}
		}
		t.Fatalf("no entry %s", id)
		return 0
	}
	if found, err := SetReminder(path, now, "a", 100); !found || err != nil {
		t.Fatalf("found %v, %v", found, err)
	}
	if found, _ := SetReminder(path, now, "gone", 100); found {
		t.Fatal("found a missing entry")
	}
	// Kept by changes to other entries.
	if _, err := Delete(path, now, "b"); err != nil || remindAt("a") != 100 {
		t.Fatalf("after Delete: %d, %v", remindAt("a"), err)
	}
	if cleared, _ := Reminded(path, now, "a", 99); cleared || remindAt("a") != 100 {
		t.Fatal("cleared a reminder set for another time")
	}
	if cleared, _ := Reminded(path, now, "a", 100); !cleared || remindAt("a") != 0 {
		t.Fatal("did not clear the reminder")
	}
	// A snoozed entry outlives the age limit until it shows, and then its
	// age counts from then.
	SetReminder(path, now, "a", 100)
	later := now.Add(MaxAge + time.Hour)
	if entries, _ := Read(path, later); len(entries) != 1 {
		t.Fatalf("entries %+v", entries)
	}
	if shown, _ := Reminded(path, later, "a", 100); !shown {
		t.Fatal("not shown")
	}
	if entries, _ := Read(path, later.Add(time.Hour)); len(entries) != 1 || entries[0].ReceivedAt != later.Unix() {
		t.Fatalf("entries %+v", entries)
	}
}

func TestRemindersOutsideTheEntryLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1_800_000_000, 0)
	entries := []Entry{}
	for i := range MaxEntries + 10 {
		entries = append(entries, Entry{ID: fmt.Sprint("m", i), ReceivedAt: now.Unix() - int64(i)})
	}
	entries[len(entries)-1].RemindAt = now.Unix() + 60 // the oldest
	if err := Write(path, entries); err != nil {
		t.Fatal(err)
	}
	got, _ := Read(path, now)
	if len(got) != MaxEntries+1 || got[len(got)-1].RemindAt == 0 {
		t.Fatalf("%d entries, last %+v", len(got), got[len(got)-1])
	}
}

func TestPrunesAReplacedMessageDespiteItsReminder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1_800_000_000, 0)
	old := now.Add(-MaxAge - time.Hour).Unix()
	// Its reminder was not dropped when B came.
	err := Write(path, []Entry{
		{ID: "b", Key: "disk", ChannelID: "ch_1", ReceivedAt: old, Seq: 2},
		{ID: "a", Key: "disk", ChannelID: "ch_1", ReceivedAt: old, Seq: 1, RemindAt: now.Unix()},
		{ID: "c", ReceivedAt: old, RemindAt: now.Unix()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := Read(path, now); len(entries) != 1 || entries[0].ID != "c" {
		t.Fatalf("entries %+v", entries)
	}
}

func TestForgetsAReplacedReminderOnceTheReplacementIsPruned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1_800_000_000, 0)
	// The clock was set back when B came, so B ages out first.
	err := Write(path, []Entry{
		{ID: "a", Key: "disk", ChannelID: "ch_1", ReceivedAt: now.Unix() - 60, Seq: 1, RemindAt: now.Unix()},
		{ID: "b", Key: "disk", ChannelID: "ch_1", ReceivedAt: now.Add(-MaxAge - time.Hour).Unix(), Seq: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := Read(path, now)
	if len(entries) != 1 || entries[0].ID != "a" || entries[0].RemindAt != 0 {
		t.Fatalf("entries %+v", entries)
	}
}

func TestAReplayReplacesNothingSentLater(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	now := time.Unix(1_800_000_000, 0)
	b := Entry{ID: "b", Key: "disk", ChannelID: "ch_1", SentAt: 200, ReceivedAt: now.Unix(), Seq: 1, RemindAt: now.Unix() + 60}
	if err := Write(path, []Entry{b}); err != nil {
		t.Fatal(err)
	}
	// A, sent before B, delivered again once pruned from the history.
	a := Entry{ID: "a", Key: "disk", ChannelID: "ch_1", SentAt: 100, ReceivedAt: now.Unix(), Seq: 2}
	if rewrote, err := DropReplacedReminders(path, []Entry{b}, a); rewrote || err != nil {
		t.Fatalf("rewrote %v, %v", rewrote, err)
	}
	entries := []Entry{a, b}
	if Replaced(entries, 1) || !Replaced(entries, 0) {
		t.Fatal("the message sent later lost its place")
	}
}
