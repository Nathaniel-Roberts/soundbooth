package main

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed helpers/systap.swift
var systapSource string

// systapBinary compiles the ScreenCaptureKit helper on first use and
// caches it by source hash in the state dir. Needs Xcode CLT (swiftc).
func systapBinary() (string, error) {
	sum := sha256.Sum256([]byte(systapSource))
	binDir := filepath.Join(stateDir(), "bin")
	bin := filepath.Join(binDir, fmt.Sprintf("systap-%x", sum[:6]))
	if info, err := os.Stat(bin); err == nil && info.Mode()&0o111 != 0 {
		return bin, nil
	}
	if _, err := exec.LookPath("xcrun"); err != nil {
		return "", fmt.Errorf("system audio needs the Xcode command line tools (xcode-select --install)")
	}
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(binDir, "systap.swift")
	if err := os.WriteFile(src, []byte(systapSource), 0o644); err != nil {
		return "", err
	}
	out, err := exec.Command("xcrun", "swiftc", "-O", "-o", bin, src).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("compiling system audio helper: %v: %s", err, firstLine(string(out)))
	}
	return bin, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// SysCapture records system audio (all apps) to a mono FLAC via the
// systap helper piped into sox, and meters it for the UI.
type SysCapture struct {
	Ticks chan MeterTick
	File  string

	tap      *exec.Cmd
	enc      *exec.Cmd
	encIn    io.WriteCloser
	stopOnce sync.Once
	started  chan error
}

// startSysCapture launches the tap. The returned error covers setup
// problems; TCC (Screen Recording) denial surfaces via WaitReady.
func startSysCapture(file string) (*SysCapture, error) {
	bin, err := systapBinary()
	if err != nil {
		return nil, err
	}
	soxPath, err := findBin("sox")
	if err != nil {
		return nil, err
	}

	tap := exec.Command(bin)
	tapOut, err := tap.StdoutPipe()
	if err != nil {
		return nil, err
	}
	tapErr, err := tap.StderrPipe()
	if err != nil {
		return nil, err
	}

	// stereo f32 in -> mono mixdown FLAC out
	enc := exec.Command(soxPath, "-q",
		"-t", "raw", "-e", "floating-point", "-b", "32", "-c", "2", "-r", "48000", "-",
		file, "remix", "1,2")
	encIn, err := enc.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := tap.Start(); err != nil {
		return nil, err
	}
	if err := enc.Start(); err != nil {
		_ = tap.Process.Kill()
		return nil, err
	}

	s := &SysCapture{
		Ticks:   make(chan MeterTick, 64),
		File:    file,
		tap:     tap,
		enc:     enc,
		encIn:   encIn,
		started: make(chan error, 1),
	}

	// readiness / TCC watcher
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := tapErr.Read(buf)
			if n > 0 {
				text := string(buf[:n])
				if strings.Contains(text, "SB_READY") {
					s.started <- nil
					continue
				}
				if strings.Contains(text, "SB_ERR") {
					s.started <- fmt.Errorf("%s", strings.TrimSpace(strings.TrimPrefix(firstLine(text), "SB_ERR:")))
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// PCM pump: tee to the encoder and fold into 50 ms meter ticks
	go func() {
		defer func() {
			_ = encIn.Close()
			_ = enc.Wait()
			close(s.Ticks)
		}()
		const framesPerTick = recSampleRate / tickHz
		buf := make([]byte, 32768)
		var carry []byte
		var sum2, peak float64
		frames := 0
		clip := false
		for {
			n, err := tapOut.Read(buf)
			if n > 0 {
				if _, werr := encIn.Write(buf[:n]); werr != nil {
					return
				}
				data := append(carry, buf[:n]...)
				usable := len(data) / 8 * 8 // one stereo f32 frame = 8 bytes
				for i := 0; i+8 <= usable; i += 8 {
					l := math.Float32frombits(binary.LittleEndian.Uint32(data[i:]))
					r := math.Float32frombits(binary.LittleEndian.Uint32(data[i+4:]))
					v := (float64(l) + float64(r)) / 2
					av := math.Abs(v)
					if av > peak {
						peak = av
					}
					if av >= clipThreshold {
						clip = true
					}
					sum2 += v * v
					if frames++; frames >= framesPerTick {
						db := -99.0
						if peak > 0 {
							db = 20 * math.Log10(peak)
						}
						tick := MeterTick{
							RMS: lvl(math.Sqrt(sum2 / float64(frames))), Peak: lvl(peak),
							RMSR: lvl(math.Sqrt(sum2 / float64(frames))), PeakR: lvl(peak),
							DB: db, Clip: clip,
						}
						select {
						case s.Ticks <- tick:
						default:
						}
						frames, sum2, peak, clip = 0, 0, 0, false
					}
				}
				carry = append(carry[:0], data[usable:]...)
			}
			if err != nil {
				return
			}
		}
	}()

	return s, nil
}

// WaitReady blocks until the tap confirms capture or fails (TCC denied).
func (s *SysCapture) WaitReady(timeout time.Duration) error {
	select {
	case err := <-s.started:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("system audio tap did not start (Screen Recording permission?)")
	}
}

// Stop finalises the system track.
func (s *SysCapture) Stop() {
	s.stopOnce.Do(func() {
		if s.tap != nil && s.tap.Process != nil {
			_ = s.tap.Process.Signal(syscall.SIGINT)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if s.tap.ProcessState != nil {
					break
				}
				time.Sleep(30 * time.Millisecond)
			}
			_ = s.tap.Process.Kill()
			_ = s.tap.Wait()
		}
		// the PCM pump closes encIn and waits for the encoder; give the
		// FLAC a moment to finalise
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if s.enc.ProcessState != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	})
}

// mergeMicSystem combines a mono mic track (left) and mono system track
// (right) into one stereo file.
func mergeMicSystem(micFile, sysFile, out string) error {
	soxPath, err := findBin("sox")
	if err != nil {
		return err
	}
	if b, err := exec.Command(soxPath, "-M", micFile, sysFile, out).CombinedOutput(); err != nil {
		return fmt.Errorf("merging tracks: %v: %s", err, firstLine(string(b)))
	}
	return nil
}
