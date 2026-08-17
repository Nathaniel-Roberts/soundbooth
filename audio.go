package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MeterTick is one 50 ms slice of the incoming audio.
type MeterTick struct {
	RMS  float64 // 0..1 on a dB scale (floor -50 dBFS)
	Peak float64 // 0..1 on the same scale
	DB   float64 // raw peak in dBFS for this slice
	Clip bool    // any sample at/near full scale
}

const (
	meterRate      = 4000 // Hz for the metering stream
	meterChunk     = 200  // samples per tick -> 50 ms
	dbFloor        = -50.0
	clipThreshold  = 0.999
	recSampleRate  = 48000
)

// lvl maps a linear amplitude to 0..1 on a dB scale, floor -50 dBFS —
// the same curve the shell renderer used.
func lvl(v float64) float64 {
	if v <= 0 {
		return 0
	}
	db := 20 * math.Log10(v)
	out := (db - dbFloor) / -dbFloor
	return math.Min(1, math.Max(0, out))
}

// Recorder drives two sox processes against the same input device
// (coreaudio allows shared input): one writes the FLAC, one streams
// text samples (-t dat) for metering.
type Recorder struct {
	soxPath  string
	device   string // DefaultDevice or a coreaudio device name
	file     string
	channels int // 1 = mono, 2 = stereo (metering always mixes to mono)

	rec   *exec.Cmd
	meter *exec.Cmd

	Ticks chan MeterTick
	Err   chan error

	stopOnce sync.Once
	started  time.Time
}

func NewRecorder(device, file string, channels int) (*Recorder, error) {
	soxPath, err := findBin("sox")
	if err != nil {
		return nil, fmt.Errorf("sox not found — install it (brew install sox, or the nix transcribe module)")
	}
	if channels != 2 {
		channels = 1
	}
	return &Recorder{
		soxPath:  soxPath,
		device:   device,
		file:     file,
		channels: channels,
		Ticks:    make(chan MeterTick, 64),
		Err:      make(chan error, 2),
	}, nil
}

func (r *Recorder) inputArgs() []string {
	if r.device == DefaultDevice || r.device == "" {
		return []string{"-d"}
	}
	return []string{"-t", "coreaudio", r.device}
}

func (r *Recorder) Start() error {
	// Recording stream: 48k FLAC, mono or stereo per config.
	recArgs := append([]string{"-q"}, r.inputArgs()...)
	recArgs = append(recArgs, "-c", strconv.Itoa(r.channels), "-r", strconv.Itoa(recSampleRate), r.file)
	r.rec = exec.Command(r.soxPath, recArgs...)
	r.rec.Stderr = nil
	if err := r.rec.Start(); err != nil {
		return fmt.Errorf("starting recorder: %w", err)
	}

	// Metering stream: low-rate text samples on stdout.
	mArgs := append([]string{"-q"}, r.inputArgs()...)
	mArgs = append(mArgs, "-t", "dat", "-r", strconv.Itoa(meterRate), "-c", "1", "-")
	r.meter = exec.Command(r.soxPath, mArgs...)
	stdout, err := r.meter.StdoutPipe()
	if err != nil {
		r.stopProcess(r.rec)
		return err
	}
	r.meter.Stderr = nil
	if err := r.meter.Start(); err != nil {
		r.stopProcess(r.rec)
		return fmt.Errorf("starting meter: %w", err)
	}
	r.started = time.Now()

	go func() {
		defer close(r.Ticks)
		scanner := bufio.NewScanner(stdout)
		n := 0
		var sum2, peak float64
		clip := false
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, ";") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				continue
			}
			av := math.Abs(v)
			if av > peak {
				peak = av
			}
			if av >= clipThreshold {
				clip = true
			}
			sum2 += v * v
			if n++; n >= meterChunk {
				db := -99.0
				if peak > 0 {
					db = 20 * math.Log10(peak)
				}
				tick := MeterTick{
					RMS:  lvl(math.Sqrt(sum2 / float64(n))),
					Peak: lvl(peak),
					DB:   db,
					Clip: clip,
				}
				select {
				case r.Ticks <- tick:
				default: // UI stalled; drop rather than block capture
				}
				n, sum2, peak, clip = 0, 0, 0, false
			}
		}
	}()

	// Surface an early recorder death (bad device, permissions, disk).
	go func() {
		err := r.rec.Wait()
		if err != nil && !stoppedBySignal(err) {
			r.Err <- fmt.Errorf("recorder exited: %w", err)
		}
	}()

	return nil
}

// Elapsed returns the recording duration so far.
func (r *Recorder) Elapsed() time.Duration {
	if r.started.IsZero() {
		return 0
	}
	return time.Since(r.started)
}

// Stop signals both sox processes with SIGINT so the FLAC is finalised,
// then waits for the recorder to flush.
func (r *Recorder) Stop() {
	r.stopOnce.Do(func() {
		r.stopProcess(r.meter)
		if r.rec != nil && r.rec.Process != nil {
			_ = r.rec.Process.Signal(syscall.SIGINT)
			// rec.Wait() runs in the watcher goroutine; give the file a
			// moment to finalise before the caller reads it.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if r.rec.ProcessState != nil {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
		}
	})
}

func (r *Recorder) stopProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(syscall.SIGINT)
	}
}

// FileSize returns the current size of the output file.
func (r *Recorder) FileSize() int64 {
	info, err := os.Stat(r.file)
	if err != nil {
		return 0
	}
	return info.Size()
}

func stoppedBySignal(err error) bool {
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return true
		}
		// sox exits 2 on SIGINT after finalising the file.
		if exitErr.ExitCode() == 2 || exitErr.ExitCode() == 130 {
			return true
		}
	}
	return false
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}
