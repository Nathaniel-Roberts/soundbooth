package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// MeterTick is one 50 ms slice of the incoming audio. For mono input the
// right-channel fields mirror the left.
type MeterTick struct {
	RMS   float64 // left, 0..1 on a dB scale (floor -50 dBFS)
	Peak  float64
	RMSR  float64 // right
	PeakR float64
	DB    float64 // loudest channel's raw peak in dBFS
	Clip  bool    // any sample on any channel at/near full scale
}

const (
	meterRate     = 4000 // Hz for the metering stream
	meterChunk    = 200  // samples per tick -> 50 ms
	dbFloor       = -50.0
	clipThreshold = 0.999
	recSampleRate = 48000
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

func soxInputArgs(device string) []string {
	if device == DefaultDevice || device == "" {
		return []string{"-d"}
	}
	return []string{"-t", "coreaudio", device}
}

// --- Meter ---

// Meter streams low-rate text samples (sox -t dat) from the input device
// and folds them into 50 ms RMS/peak/clip ticks. It opens its own handle
// on the device; coreaudio allows shared input, so it runs alongside
// whatever is actually recording.
type Meter struct {
	Ticks    chan MeterTick
	cmd      *exec.Cmd
	stopOnce sync.Once
}

func startMeter(soxPath, device string, channels int) (*Meter, error) {
	if channels != 2 {
		channels = 1
	}
	args := append([]string{"-q"}, soxInputArgs(device)...)
	args = append(args, "-t", "dat", "-r", strconv.Itoa(meterRate), "-c", strconv.Itoa(channels), "-")
	cmd := exec.Command(soxPath, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting meter: %w", err)
	}
	m := &Meter{Ticks: make(chan MeterTick, 64), cmd: cmd}

	go func() {
		defer close(m.Ticks)
		defer func() { _ = cmd.Wait() }()
		scanner := bufio.NewScanner(stdout)
		n := 0
		var sum2L, peakL, sum2R, peakR float64
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
			vL, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				continue
			}
			vR := vL
			if channels == 2 && len(fields) > 2 {
				if r, err := strconv.ParseFloat(fields[2], 64); err == nil {
					vR = r
				}
			}
			aL, aR := math.Abs(vL), math.Abs(vR)
			if aL > peakL {
				peakL = aL
			}
			if aR > peakR {
				peakR = aR
			}
			if aL >= clipThreshold || aR >= clipThreshold {
				clip = true
			}
			sum2L += vL * vL
			sum2R += vR * vR
			if n++; n >= meterChunk {
				maxPeak := math.Max(peakL, peakR)
				db := -99.0
				if maxPeak > 0 {
					db = 20 * math.Log10(maxPeak)
				}
				tick := MeterTick{
					RMS:   lvl(math.Sqrt(sum2L / float64(n))),
					Peak:  lvl(peakL),
					RMSR:  lvl(math.Sqrt(sum2R / float64(n))),
					PeakR: lvl(peakR),
					DB:    db,
					Clip:  clip,
				}
				select {
				case m.Ticks <- tick:
				default: // UI stalled; drop rather than block capture
				}
				n, sum2L, peakL, sum2R, peakR, clip = 0, 0, 0, 0, 0, false
			}
		}
	}()
	return m, nil
}

func (m *Meter) Stop() {
	m.stopOnce.Do(func() {
		if m.cmd != nil && m.cmd.Process != nil {
			_ = m.cmd.Process.Signal(syscall.SIGINT)
		}
	})
}

// --- Recorder ---

// Recorder captures the input device to FLAC via sox, in segments so it
// can pause and resume cleanly. Stop concatenates the segments into the
// final file. A Meter runs alongside for the UI.
type Recorder struct {
	Meter *Meter
	Err   chan error

	soxPath  string
	device   string
	file     string
	channels int

	mu         sync.Mutex
	segDir     string
	segments   []string
	cur        *exec.Cmd
	curStop    bool // SIGINT was ours (pause/stop), not a crash
	paused     bool
	recorded   time.Duration
	segStart   time.Time
	stopOnce   sync.Once
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
		Err:      make(chan error, 2),
	}, nil
}

func (r *Recorder) Start() error {
	segDir, err := os.MkdirTemp("", "soundbooth-seg-*")
	if err != nil {
		return err
	}
	r.segDir = segDir

	meter, err := startMeter(r.soxPath, r.device, r.channels)
	if err != nil {
		return err
	}
	r.Meter = meter

	if err := r.startSegment(); err != nil {
		meter.Stop()
		return err
	}
	return nil
}

func (r *Recorder) startSegment() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	seg := filepath.Join(r.segDir, fmt.Sprintf("seg-%03d.flac", len(r.segments)))
	args := append([]string{"-q"}, soxInputArgs(r.device)...)
	args = append(args, "-c", strconv.Itoa(r.channels), "-r", strconv.Itoa(recSampleRate), seg)
	cmd := exec.Command(r.soxPath, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting recorder: %w", err)
	}
	r.segments = append(r.segments, seg)
	r.cur = cmd
	r.curStop = false
	r.paused = false
	r.segStart = time.Now()

	go func(c *exec.Cmd) {
		err := c.Wait()
		r.mu.Lock()
		intended := r.curStop || c != r.cur
		r.mu.Unlock()
		if err != nil && !intended && !stoppedBySignal(err) {
			select {
			case r.Err <- fmt.Errorf("recorder exited: %w", err):
			default:
			}
		}
	}(cmd)
	return nil
}

func (r *Recorder) stopSegment() {
	r.mu.Lock()
	cmd := r.cur
	r.curStop = true
	r.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// Pause finalises the current segment; the meter keeps running so the UI
// stays live.
func (r *Recorder) Pause() {
	r.mu.Lock()
	if r.paused {
		r.mu.Unlock()
		return
	}
	r.recorded += time.Since(r.segStart)
	r.mu.Unlock()
	r.stopSegment()
	r.mu.Lock()
	r.paused = true
	r.mu.Unlock()
}

func (r *Recorder) Resume() error {
	r.mu.Lock()
	if !r.paused {
		r.mu.Unlock()
		return nil
	}
	r.mu.Unlock()
	return r.startSegment()
}

func (r *Recorder) Paused() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.paused
}

// Elapsed returns recorded audio duration (pauses excluded).
func (r *Recorder) Elapsed() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paused || r.segStart.IsZero() {
		return r.recorded
	}
	return r.recorded + time.Since(r.segStart)
}

// Stop finalises capture and assembles the final file from the segments.
func (r *Recorder) Stop() error {
	var err error
	r.stopOnce.Do(func() {
		r.mu.Lock()
		paused := r.paused
		r.mu.Unlock()
		if !paused {
			r.mu.Lock()
			r.recorded += time.Since(r.segStart)
			r.paused = true // freeze Elapsed at the recorded total
			r.mu.Unlock()
			r.stopSegment()
		}
		if r.Meter != nil {
			r.Meter.Stop()
		}
		err = concatFlac(r.soxPath, r.nonEmptySegments(), r.file)
		_ = os.RemoveAll(r.segDir)
	})
	return err
}

func (r *Recorder) nonEmptySegments() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, s := range r.segments {
		if info, err := os.Stat(s); err == nil && info.Size() > 0 {
			out = append(out, s)
		}
	}
	return out
}

// FileSize reports bytes captured so far (across segments while
// recording, the final file after Stop).
func (r *Recorder) FileSize() int64 {
	if info, err := os.Stat(r.file); err == nil {
		return info.Size()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var total int64
	for _, s := range r.segments {
		if info, err := os.Stat(s); err == nil {
			total += info.Size()
		}
	}
	return total
}

// concatFlac joins FLAC parts into one file (rename when there is only one).
func concatFlac(soxPath string, parts []string, out string) error {
	sort.Strings(parts)
	switch len(parts) {
	case 0:
		return fmt.Errorf("no audio captured")
	case 1:
		if err := os.Rename(parts[0], out); err == nil {
			return nil
		}
		// cross-device rename fallback
		data, err := os.ReadFile(parts[0])
		if err != nil {
			return err
		}
		return os.WriteFile(out, data, 0o644)
	default:
		args := append(append([]string{}, parts...), out)
		if b, err := exec.Command(soxPath, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("joining segments: %v: %s", err, strings.TrimSpace(string(b)))
		}
		return nil
	}
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
