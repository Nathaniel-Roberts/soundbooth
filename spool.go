package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Spooler is the armed replay buffer: ffmpeg records the input device
// into gapless 60-second FLAC segments in a spool directory, and a
// janitor continuously deletes segments that fall outside the buffer
// window — nothing is retained unless the user triggers a save.
type Spooler struct {
	Dir string

	cmd      *exec.Cmd
	window   time.Duration
	mu       sync.Mutex
	keep     time.Time // segments ending before this are deleted
	frozen   bool      // save triggered: stop advancing the window
	stopOnce sync.Once
	done     chan struct{}
}

const spoolSegmentSeconds = 60

// startSpooler begins buffering. avIndex selects the avfoundation audio
// device (-1 = system default).
func startSpooler(avIndex, channels int, window time.Duration) (*Spooler, error) {
	ffmpeg, err := findBin("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("ffmpeg not found — armed mode needs it for gapless buffering")
	}
	if channels != 2 {
		channels = 1
	}
	dir, err := os.MkdirTemp("", "soundbooth-spool-*")
	if err != nil {
		return nil, err
	}
	input := ":default"
	if avIndex >= 0 {
		input = ":" + strconv.Itoa(avIndex)
	}
	cmd := exec.Command(ffmpeg,
		"-hide_banner", "-loglevel", "error",
		"-f", "avfoundation", "-i", input,
		"-ac", strconv.Itoa(channels), "-ar", strconv.Itoa(recSampleRate),
		"-f", "segment", "-segment_time", strconv.Itoa(spoolSegmentSeconds),
		"-reset_timestamps", "1", "-strftime", "1",
		filepath.Join(dir, "%Y%m%d-%H%M%S.flac"),
	)
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("starting buffer capture: %w", err)
	}
	s := &Spooler{
		Dir:    dir,
		cmd:    cmd,
		window: window,
		keep:   time.Now().Add(-window),
		done:   make(chan struct{}),
	}
	go s.janitor()
	go func() { _ = cmd.Wait() }()
	return s, nil
}

func (s *Spooler) janitor() {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.frozen {
				s.keep = time.Now().Add(-s.window)
			}
			keep := s.keep
			s.mu.Unlock()
			entries, err := os.ReadDir(s.Dir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				info, err := e.Info()
				// mtime approximates the segment's end time
				if err == nil && info.ModTime().Before(keep) {
					_ = os.Remove(filepath.Join(s.Dir, e.Name()))
				}
			}
		}
	}
}

// Trigger freezes the retention window at (now - buffer): everything from
// the last N minutes onward is kept from here until Stop.
func (s *Spooler) Trigger() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.frozen = true
	s.keep = time.Now().Add(-s.window)
	return s.keep
}

// BufferedSince reports how much of the window is filled.
func (s *Spooler) Buffered() time.Duration {
	entries, err := os.ReadDir(s.Dir)
	if err != nil || len(entries) == 0 {
		return 0
	}
	oldest := time.Now()
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			start := info.ModTime().Add(-spoolSegmentSeconds * time.Second)
			if start.Before(oldest) {
				oldest = start
			}
		}
	}
	d := time.Since(oldest)
	if d > s.window {
		d = s.window
	}
	return d
}

// Stop ends capture and returns the segments covering keep..now, oldest
// first. The caller owns concatenation and cleanup.
func (s *Spooler) Stop() []string {
	var segs []string
	s.stopOnce.Do(func() {
		close(s.done)
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Signal(syscall.SIGINT)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if s.cmd.ProcessState != nil {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
		}
		s.mu.Lock()
		keep := s.keep
		s.mu.Unlock()
		entries, err := os.ReadDir(s.Dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil || info.Size() == 0 {
				continue
			}
			if info.ModTime().After(keep) {
				segs = append(segs, filepath.Join(s.Dir, e.Name()))
			}
		}
		sort.Strings(segs)
	})
	return segs
}

// Cleanup removes the spool directory (call after concatenation, or on
// disarm — the buffer must never outlive the session).
func (s *Spooler) Cleanup() {
	_ = os.RemoveAll(s.Dir)
}
