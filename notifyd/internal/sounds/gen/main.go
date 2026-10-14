// SPDX-License-Identifier: GPL-3.0-or-later

// Command gen writes notifier's sounds as notifier_<name>.wav into a
// directory, by default the Android app's raw resources:
//
//	go run ./internal/sounds/gen -o ../android/app/src/main/res/raw
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/joncbenderkh/notifier/notifyd/internal/sounds"
)

func main() {
	dir := flag.String("o", "../android/app/src/main/res/raw", "output directory")
	flag.Parse()
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, name := range sounds.Names {
		data, err := sounds.WAV(name)
		if err == nil {
			path := filepath.Join(*dir, "notifier_"+name+".wav")
			err = os.WriteFile(path, data, 0o644)
			fmt.Println(path)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}
