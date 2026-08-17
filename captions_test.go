package main

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteWAVAndPeak(t *testing.T) {
	pcm := make([]byte, 3200)
	binary.LittleEndian.PutUint16(pcm[100:], uint16(int16(12000)))
	if p := peakInt16(pcm); p != 12000 {
		t.Errorf("peak = %d, want 12000", p)
	}
	path := filepath.Join(t.TempDir(), "t.wav")
	if err := writeWAV(path, pcm, 16000); err != nil {
		t.Fatal(err)
	}
	soxi, err := findBin("soxi")
	if err != nil {
		t.Skip("sox not installed")
	}
	out, err := exec.Command(soxi, "-r", path).Output()
	if err != nil || strings.TrimSpace(string(out)) != "16000" {
		t.Errorf("wav rate readback = %q err=%v", out, err)
	}
}

// TestCaptionDaemon feeds synthesised speech through the persistent
// python daemon and expects recognisable text back.
func TestCaptionDaemon(t *testing.T) {
	hwGate(t)
	python, err := captionPython()
	if err != nil {
		t.Skip(err)
	}
	soxPath, err := findBin("sox")
	if err != nil {
		t.Skip("sox not installed")
	}
	dir := t.TempDir()
	aiff := filepath.Join(dir, "s.aiff")
	wav := filepath.Join(dir, "s.wav")
	if b, err := exec.Command("say", "-o", aiff, "testing live captions one two three").CombinedOutput(); err != nil {
		t.Skipf("say unavailable: %v %s", err, b)
	}
	if b, err := exec.Command(soxPath, aiff, "-r", "16000", "-c", "1", "-b", "16", wav).CombinedOutput(); err != nil {
		t.Fatalf("convert: %v %s", err, b)
	}

	cmd := exec.Command(python, "-c", captionScript)
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	fmt.Fprintln(stdin, wav)
	_ = stdin.Close()

	done := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			done <- scanner.Text()
		}
	}()
	select {
	case line := <-done:
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "caption") && !strings.Contains(lower, "one") {
			t.Errorf("unexpected caption result: %s", line)
		}
		t.Logf("caption: %s", line)
	case <-time.After(60 * time.Second):
		t.Fatal("caption daemon produced nothing within 60s")
	}
}
