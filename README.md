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
- `sox` (capture) and `ffmpeg` (device listing, armed-mode buffer) — via
  nix or brew (the nix flake package wraps both onto PATH)
- `whispermlx` for transcription (`uv tool install --python 3.13 --with 'numba>=0.61' whispermlx`)
- A Hugging Face read token at `~/.cache/huggingface/token` with the
  pyannote `speaker-diarization-community-1` terms accepted (diarisation)

Run `soundbooth doctor` to check all of the above.

## Install

```
nix profile install .#soundbooth    # or add the flake to your config
# or
go build -o soundbooth . && ./soundbooth
```

## CLI

```
soundbooth            # the TUI
soundbooth doctor     # toolchain check
soundbooth devices    # list capture devices
soundbooth trigger    # remote: save the armed buffer / start from standby
soundbooth marker     # remote: drop a marker in the current recording
soundbooth stop       # remote: stop and transcribe
```

The remote commands talk to a running instance over a unix socket
(`~/.local/state/soundbooth/control.sock`) — bind them to Stream Deck
keys or a global hotkey so armed mode never needs the terminal focused.

## A note on recording people

soundbooth assumes you know and follow your local rules on recording
conversations (in NSW that is the Surveillance Devices Act 2007, which
generally requires the consent of the people in the conversation). Armed
mode is designed to be consent-friendly — the buffer is continuously
discarded and nothing is kept without an explicit save — but the law
cares about the recording, not the retention policy. Tell people.

## Screens

1. **Setup** — microphone, save directory, name, mono/stereo, mode
   (record now / armed replay buffer / armed with no buffer), buffer
   length, transcribe on/off, whisper model, speaker count, language,
   theme (all four Catppuccin flavours, live preview; per-colour
   overrides via `theme_colors` in the config). Choices persist in
   `~/.config/soundbooth/config.json`. `b` opens the recording library.
   If a previous session crashed, a recovery banner offers to salvage it.
2. **Recording** — live gradient waveform (Braille dots, vertical
   lavender-to-sapphire ramp, red clipped columns; split L/R lanes in
   stereo), DAW-style time ruler with marker arrows, VU meter with
   peak-hold, decaying peak dB with gain advice, low-disk warning.
   `m` drops a marker, `+`/`-` zooms the timebase, `p` pauses (paused
   spans render grey; segments concatenate seamlessly), enter stops and
   transcribes, `x` stops and skips. If the input device dies mid-take,
   capture recovers onto the default mic and drops an automatic marker.
3. **Armed** — the replay buffer. Live waveform while buffering; enter
   saves the last N minutes and keeps recording; `x` disarms and
   discards. With buffer off it is a pure standby screen: metered, but
   nothing written until triggered.
4. **Transcribing** — stage checklist with a live progress bar and a
   preview of segments as they stream in; `l` shows the raw log.
5. **Speakers** — diarised speakers ranked by talk time with their
   longest quote; assign real names, which land in a merged
   `-transcript.md`.
6. **Done** — stats (speech time, words, markers, clips), talk-time
   bars, transcript preview. `p` plays the audio, `t` (re)transcribes,
   `n` new recording, `o` open folder. A configurable `post_command`
   runs here with SB_AUDIO / SB_TRANSCRIPT_MD / SB_TRANSCRIPT_DIR /
   SB_MARKERS in the environment — point it at an LLM summariser, a
   copy step, whatever.
7. **Library** — browse past recordings, open, delete, re-transcribe.

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

## Licence

MIT — see [LICENSE](LICENSE).
