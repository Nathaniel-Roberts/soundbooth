package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRecorderHardware exercises the real sox capture path. It touches the
// microphone, so it only runs when explicitly requested:
//
//	SOUNDBOOTH_HW_TEST=1 go test -run Hardware -v
func TestRecorderHardware(t *testing.T) {
	if os.Getenv("SOUNDBOOTH_HW_TEST") != "1" {
		t.Skip("set SOUNDBOOTH_HW_TEST=1 to run the microphone test")
	}
	file := filepath.Join(t.TempDir(), "hwtest.flac")
	rec, err := NewRecorder(DefaultDevice, file, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Start(); err != nil {
		t.Fatal(err)
	}
	ticks := 0
	deadline := time.After(3 * time.Second)
collect:
	for {
		select {
		case _, ok := <-rec.Ticks:
			if !ok {
				break collect
			}
			ticks++
		case <-deadline:
			break collect
		}
	}
	rec.Stop()
	if ticks < 20 {
		t.Errorf("expected ~60 meter ticks in 3s, got %d", ticks)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() == 0 {
		t.Fatalf("recording missing or empty: %v", err)
	}
	t.Logf("ticks=%d size=%d", ticks, info.Size())
}
