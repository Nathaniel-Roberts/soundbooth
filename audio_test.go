package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Hardware tests exercise the real sox/ffmpeg capture paths. They touch
// the microphone, so they only run when explicitly requested:
//
//	SOUNDBOOTH_HW_TEST=1 go test -v
func hwGate(t *testing.T) {
	t.Helper()
	if os.Getenv("SOUNDBOOTH_HW_TEST") != "1" {
		t.Skip("set SOUNDBOOTH_HW_TEST=1 to run microphone tests")
	}
}

func TestRecorderHardware(t *testing.T) {
	hwGate(t)
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
		case _, ok := <-rec.Meter.Ticks:
			if !ok {
				break collect
			}
			ticks++
		case <-deadline:
			break collect
		}
	}
	if err := rec.Stop(); err != nil {
		t.Fatal(err)
	}
	if ticks < 20 {
		t.Errorf("expected ~60 meter ticks in 3s, got %d", ticks)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() == 0 {
		t.Fatalf("recording missing or empty: %v", err)
	}
	t.Logf("ticks=%d size=%d", ticks, info.Size())
}

func TestRecorderPauseResumeHardware(t *testing.T) {
	hwGate(t)
	file := filepath.Join(t.TempDir(), "pausetest.flac")
	rec, err := NewRecorder(DefaultDevice, file, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	rec.Pause()
	if !rec.Paused() {
		t.Error("expected paused state")
	}
	pausedAt := rec.Elapsed()
	time.Sleep(1 * time.Second)
	if rec.Elapsed()-pausedAt > 100*time.Millisecond {
		t.Errorf("elapsed advanced while paused: %v -> %v", pausedAt, rec.Elapsed())
	}
	if err := rec.Resume(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	if err := rec.Stop(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() == 0 {
		t.Fatalf("concatenated recording missing or empty: %v", err)
	}
	// ~3s of audio across two segments; must not drift after Stop
	got := rec.Elapsed()
	if got < 2500*time.Millisecond || got > 3700*time.Millisecond {
		t.Errorf("elapsed = %v, want ~3s", got)
	}
	time.Sleep(300 * time.Millisecond)
	if rec.Elapsed() != got {
		t.Errorf("elapsed advanced after Stop: %v -> %v", got, rec.Elapsed())
	}
	t.Logf("elapsed=%v size=%d", rec.Elapsed(), info.Size())
}

func TestCrashRecoveryHardware(t *testing.T) {
	hwGate(t)
	file := filepath.Join(t.TempDir(), "crashed.flac")
	rec, err := NewRecorder(DefaultDevice, file, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := rec.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	// Simulate a crash: kill the capture hard, never call Stop.
	rec.mu.Lock()
	segDir := rec.segDir
	cur := rec.cur
	rec.mu.Unlock()
	rec.Meter.Stop()
	_ = cur.Process.Kill()
	time.Sleep(200 * time.Millisecond)

	orphans := findOrphans()
	var found *orphan
	for i := range orphans {
		if orphans[i].Dir == segDir {
			found = &orphans[i]
		}
	}
	if found == nil {
		t.Fatalf("crashed session not found among %d orphan(s)", len(orphans))
	}
	if found.Meta.File != file {
		t.Errorf("meta file = %q, want %q", found.Meta.File, file)
	}
	soxPath, _ := findBin("sox")
	out := filepath.Join(t.TempDir(), "recovered.flac")
	if err := concatFlac(soxPath, found.Segments, out); err != nil {
		// A SIGKILLed FLAC may be truncated but should still decode; a
		// hard failure here means recovery is broken.
		t.Fatalf("recovery concat failed: %v", err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatalf("recovered file missing or empty")
	}
	_ = os.RemoveAll(segDir)
	t.Logf("recovered %d bytes from crash", info.Size())
}

func TestSpoolerHardware(t *testing.T) {
	hwGate(t)
	spool, err := startSpooler(-1, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer spool.Cleanup()
	time.Sleep(3 * time.Second)
	spool.Trigger()
	time.Sleep(2 * time.Second)
	segs := spool.Stop()
	if len(segs) == 0 {
		t.Fatal("no segments after trigger+stop")
	}
	soxPath, err := findBin("sox")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "spooled.flac")
	if err := concatFlac(soxPath, segs, out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(out)
	if err != nil || info.Size() == 0 {
		t.Fatalf("spool concat missing or empty: %v", err)
	}
	t.Logf("segments=%d size=%d", len(segs), info.Size())
}
