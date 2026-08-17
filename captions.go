package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Live captions: a persistent python daemon keeps a small whisper model
// warm on the GPU while a sox tap feeds it 5-second mic chunks. Rough and
// a few seconds behind by design — the real transcript still comes from
// the full pass at stop.
const captionRepo = "mlx-community/whisper-base-mlx"

const captionScript = `
import json, sys
import mlx_whisper
sys.stderr.write("SB_CAPTIONS_READY\n"); sys.stderr.flush()
for line in sys.stdin:
    path = line.strip()
    if not path:
        continue
    try:
        r = mlx_whisper.transcribe(path, path_or_hf_repo="` + captionRepo + `", language="en")
        print(json.dumps({"text": r.get("text", "").strip()}), flush=True)
    except Exception as e:
        print(json.dumps({"err": str(e)}), flush=True)
`

const (
	captionRate    = 16000
	captionSeconds = 5
	// chunks quieter than this peak are skipped: whisper hallucinates
	// plausible text on silence
	captionMinPeak = 260 // int16 units, ~ -42 dBFS
)

type Captioner struct {
	Lines chan string

	daemon   *exec.Cmd
	daemonIn io.WriteCloser
	chunker  *exec.Cmd
	tmpDir   string
	stopOnce sync.Once

	mu    sync.Mutex
	queue []string // chunk files awaiting results, FIFO
}

func captionPython() (string, error) {
	home, _ := os.UserHomeDir()
	p := filepath.Join(home, ".local", "share", "uv", "tools", "whispermlx", "bin", "python")
	if info, err := os.Stat(p); err == nil && info.Mode()&0o111 != 0 {
		return p, nil
	}
	return "", fmt.Errorf("whispermlx python env not found (install whispermlx first)")
}

func startCaptioner(device string) (*Captioner, error) {
	python, err := captionPython()
	if err != nil {
		return nil, err
	}
	soxPath, err := findBin("sox")
	if err != nil {
		return nil, err
	}
	tmpDir, err := os.MkdirTemp("", "soundbooth-cap-*")
	if err != nil {
		return nil, err
	}

	daemon := exec.Command(python, "-c", captionScript)
	daemonIn, err := daemon.StdinPipe()
	if err != nil {
		return nil, err
	}
	daemonOut, err := daemon.StdoutPipe()
	if err != nil {
		return nil, err
	}
	daemon.Stderr = nil
	if err := daemon.Start(); err != nil {
		return nil, fmt.Errorf("starting caption model: %w", err)
	}

	args := append([]string{"-q"}, soxInputArgs(device)...)
	args = append(args, "-t", "raw", "-e", "signed", "-b", "16",
		"-c", "1", "-r", strconv.Itoa(captionRate), "-")
	chunker := exec.Command(soxPath, args...)
	chunkOut, err := chunker.StdoutPipe()
	if err != nil {
		_ = daemon.Process.Kill()
		return nil, err
	}
	if err := chunker.Start(); err != nil {
		_ = daemon.Process.Kill()
		return nil, err
	}

	c := &Captioner{
		Lines:    make(chan string, 16),
		daemon:   daemon,
		daemonIn: daemonIn,
		chunker:  chunker,
		tmpDir:   tmpDir,
	}

	// chunker: accumulate 5 s of PCM, gate on silence, hand to the daemon
	go func() {
		defer func() { _ = daemonIn.Close() }()
		chunkBytes := captionRate * 2 * captionSeconds
		buf := make([]byte, 16384)
		acc := make([]byte, 0, chunkBytes)
		n := 0
		for {
			r, err := chunkOut.Read(buf)
			if r > 0 {
				acc = append(acc, buf[:r]...)
				for len(acc) >= chunkBytes {
					chunk := acc[:chunkBytes]
					if peakInt16(chunk) >= captionMinPeak {
						path := filepath.Join(c.tmpDir, fmt.Sprintf("chunk-%d.wav", n))
						n++
						if writeWAV(path, chunk, captionRate) == nil {
							c.mu.Lock()
							c.queue = append(c.queue, path)
							c.mu.Unlock()
							if _, werr := fmt.Fprintln(daemonIn, path); werr != nil {
								return
							}
						}
					}
					acc = append(acc[:0], acc[chunkBytes:]...)
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// results: JSON lines back from the daemon
	go func() {
		defer close(c.Lines)
		defer func() { _ = daemon.Wait() }()
		scanner := bufio.NewScanner(daemonOut)
		for scanner.Scan() {
			var res struct {
				Text string `json:"text"`
				Err  string `json:"err"`
			}
			if json.Unmarshal(scanner.Bytes(), &res) != nil {
				continue
			}
			c.mu.Lock()
			if len(c.queue) > 0 {
				_ = os.Remove(c.queue[0])
				c.queue = c.queue[1:]
			}
			c.mu.Unlock()
			if res.Text == "" {
				continue
			}
			select {
			case c.Lines <- res.Text:
			default:
			}
		}
	}()

	return c, nil
}

func peakInt16(pcm []byte) int {
	peak := 0
	for i := 0; i+2 <= len(pcm); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i:])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// writeWAV wraps mono 16-bit PCM in a minimal WAV header.
func writeWAV(path string, pcm []byte, rate int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var h [44]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVE")
	copy(h[12:], "fmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))
	if _, err := f.Write(h[:]); err != nil {
		return err
	}
	_, err = f.Write(pcm)
	return err
}

func (c *Captioner) Stop() {
	c.stopOnce.Do(func() {
		if c.chunker != nil && c.chunker.Process != nil {
			_ = c.chunker.Process.Signal(syscall.SIGINT)
			go func() { _ = c.chunker.Wait() }()
		}
		// closing stdin (chunker goroutine) ends the daemon loop; give it
		// a moment then make sure
		go func() {
			time.Sleep(3 * time.Second)
			if c.daemon != nil && c.daemon.Process != nil && c.daemon.ProcessState == nil {
				_ = c.daemon.Process.Kill()
			}
			_ = os.RemoveAll(c.tmpDir)
		}()
	})
}
