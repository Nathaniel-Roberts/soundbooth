# soundbooth

A Bubble Tea TUI for recording and transcribing meetings on a Mac: pick a
mic, watch a live Audacity-style Braille waveform with gain/clip advice
while you record, then hand the audio straight to whispermlx (Whisper on
the Apple GPU) with pyannote speaker diarisation.

Catppuccin Mocha throughout.

## Requirements

- Apple silicon Mac
- `sox` (capture) and `ffmpeg` (device listing) — via nix or brew
- `whispermlx` for transcription (`uv tool install --python 3.13 --with 'numba>=0.61' whispermlx`)
- A Hugging Face read token at `~/.cache/huggingface/token` with the
  pyannote `speaker-diarization-community-1` terms accepted (diarisation)

## Run

```
go build -o soundbooth . && ./soundbooth
```

`soundbooth --devices` lists capture devices without starting the TUI.

## Screens

1. **Setup** — microphone, save directory, name, mono/stereo, transcribe
   on/off, whisper model, speaker count, language. Choices persist in
   `~/.config/soundbooth/config.json`.
2. **Recording** — live waveform (blue peak envelope, lavender RMS core,
   red clipped columns, Braille dots for 2x4 sub-cell resolution), elapsed
   time, decaying peak dB with gain advice (level OK / quiet / hot /
   CLIPPING). Enter stops and transcribes; `x` stops and skips.
3. **Transcribing** — streamed whispermlx output.
4. **Done** — file and transcript paths; `n` new recording, `o` open folder.

## Architecture

Pure orchestration — no cgo. Two sox processes share the input device
(coreaudio allows shared input): one writes 48 kHz FLAC, one streams
low-rate text samples (`-t dat`) that a goroutine folds into 50 ms
RMS/peak/clip ticks for the renderer. Transcription shells out to
whispermlx with `PYTHONWARNINGS=ignore` (torchcodec in that env can't load
against torch 2.8; pyannote falls back to its own loader).

## Tests

```
go test ./...                          # render + mapping tests
SOUNDBOOTH_HW_TEST=1 go test -v ./...  # plus a real 3 s microphone capture
```
