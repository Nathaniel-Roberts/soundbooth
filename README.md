# soundbooth

A Bubble Tea TUI for recording and transcribing meetings on a Mac: pick a
mic, watch a live Audacity-style Braille waveform with gain/clip advice
while you record, then hand the audio straight to whispermlx (Whisper on
the Apple GPU) with pyannote speaker diarisation.

The headline feature is **armed mode**, a replay buffer for conversations:
soundbooth sits buffering the last N minutes (nothing retained beyond
that), and when someone says "we should have been recording this", one
keypress saves the last N minutes and keeps recording until you stop —
then transcribes the lot.

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

1. **Setup** — microphone, save directory, name, mono/stereo, mode
   (record now / armed replay buffer), buffer length, transcribe on/off,
   whisper model, speaker count, language. Choices persist in
   `~/.config/soundbooth/config.json`.
2. **Recording** — live waveform (blue peak envelope, lavender RMS core,
   red clipped columns, Braille dots for 2x4 sub-cell resolution; split
   L/R lanes in stereo), elapsed time, decaying peak dB with gain advice
   (level OK / quiet / hot / CLIPPING). `p` pauses (segments concatenate
   seamlessly, paused spans show grey in the waveform); enter stops and
   transcribes; `x` stops and skips.
3. **Armed** — the replay buffer. Live waveform while buffering; enter
   saves the last N minutes and keeps recording; `x` disarms and discards.
4. **Transcribing** — streamed whispermlx output.
5. **Done** — file and transcript paths with a transcript preview;
   `t` (re)transcribes if it was skipped or failed, `n` new recording,
   `o` open folder.

## Architecture

Pure orchestration — no cgo. Multiple processes share the input device
(coreaudio allows shared input):

- **Record mode**: sox writes 48 kHz FLAC segments (segments make pause
  cheap and safe; stop concatenates them).
- **Armed mode**: ffmpeg's segment muxer spools gapless 60 s FLAC
  segments to a temp dir; a janitor deletes anything older than the
  buffer window until a save is triggered. Disarming wipes the spool —
  nothing is retained without an explicit save.
- **Metering**: a sox `-t dat` text-sample stream folded into 50 ms
  RMS/peak/clip ticks per channel for the renderer.

Transcription shells out to whispermlx with `PYTHONWARNINGS=ignore`
(torchcodec in that env can't load against torch 2.8; pyannote falls back
to its own loader).

## Tests

```
go test ./...                          # render + mapping tests
SOUNDBOOTH_HW_TEST=1 go test -v ./...  # plus a real 3 s microphone capture
```
