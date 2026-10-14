// SPDX-License-Identifier: GPL-3.0-or-later

package sounds

import (
	"bytes"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSoundsAreShortAndUnclipped(t *testing.T) {
	for _, name := range Names {
		data, err := WAV(name)
		if err != nil {
			t.Fatal(err)
		}
		if string(data[:4]) != "RIFF" || string(data[8:16]) != "WAVEfmt " || string(data[36:40]) != "data" {
			t.Fatalf("%s: not a PCM WAV header", name)
		}
		samples := make([]int16, (len(data)-44)/2)
		if err := binary.Read(bytes.NewReader(data[44:]), binary.LittleEndian, samples); err != nil {
			t.Fatal(err)
		}
		length := time.Duration(len(samples)) * time.Second / rate
		if length < 300*time.Millisecond || length > 1500*time.Millisecond {
			t.Errorf("%s lasts %v", name, length)
		}
		var top int16
		for _, s := range samples {
			top = max(top, s, -s)
		}
		if want := int16(math.Round(peak * math.MaxInt16)); top != want {
			t.Errorf("%s peaks at %d, want %d", name, top, want)
		}
		if samples[0] != 0 || samples[len(samples)-1] != 0 {
			t.Errorf("%s does not fade in and out", name)
		}
		again, _ := WAV(name)
		if !bytes.Equal(data, again) {
			t.Errorf("%s differs between calls", name)
		}
	}
	if _, err := WAV("nope"); err == nil {
		t.Error("an unknown sound was made")
	}
}

// The Android app bundles copies made by ./gen; they must not drift.
func TestAndroidCopiesAreCurrent(t *testing.T) {
	for _, name := range Names {
		want, _ := WAV(name)
		got, err := os.ReadFile(filepath.Join("..", "..", "..", "android", "app", "src", "main", "res", "raw", "notifier_"+name+".wav"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("notifier_%s.wav is stale: run go run ./internal/sounds/gen in notifyd/", name)
		}
	}
}

func TestInstallRewritesOnlyWhatChanged(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sounds")
	paths, err := Install(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range Names {
		if paths[name] != filepath.Join(dir, name+".wav") {
			t.Fatalf("%s at %q", name, paths[name])
		}
	}
	old := time.Unix(1, 0)
	_ = os.Chtimes(paths["message"], old, old)
	_ = os.WriteFile(paths["urgent"], []byte("stale"), 0o644)
	if _, err := Install(dir); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(paths["message"]); !info.ModTime().Equal(old) {
		t.Error("an unchanged sound was rewritten")
	}
	want, _ := WAV("urgent")
	if got, _ := os.ReadFile(paths["urgent"]); !bytes.Equal(got, want) {
		t.Error("a stale sound was kept")
	}
	if _, err := os.Stat(paths["urgent"] + ".tmp"); !os.IsNotExist(err) {
		t.Error("a temporary file was left behind")
	}
}
