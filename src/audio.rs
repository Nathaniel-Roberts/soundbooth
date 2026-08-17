use cpal::traits::{DeviceTrait, HostTrait, StreamTrait};
use crossbeam_channel::{bounded, Receiver, Sender, TrySendError};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use crate::config::DEFAULT_DEVICE;
use crate::state::{concat_flac, find_bin, sessions_dir, write_meta, SessionMeta};

pub const TICK_HZ: u32 = 40; // 25 ms per meter tick / Braille sub-column
pub const DB_FLOOR: f64 = -50.0;
pub const CLIP_THRESHOLD: f64 = 0.999;

/// One 25 ms slice of incoming audio. Mono input mirrors into the right
/// channel fields.
#[derive(Clone, Copy, Default)]
pub struct MeterTick {
    pub rms: f64,
    pub peak: f64,
    pub rms_r: f64,
    pub peak_r: f64,
    pub db: f64,
    pub clip: bool,
}

/// Map linear amplitude to 0..1 on a dB scale, floor -50 dBFS.
pub fn lvl(v: f64) -> f64 {
    if v <= 0.0 {
        return 0.0;
    }
    let db = 20.0 * v.log10();
    ((db - DB_FLOOR) / -DB_FLOOR).clamp(0.0, 1.0)
}

/// Capture device names; the system default is always first.
pub fn list_input_devices() -> Vec<String> {
    let mut out = vec![DEFAULT_DEVICE.to_string()];
    let host = cpal::default_host();
    if let Ok(devices) = host.input_devices() {
        for d in devices {
            if let Ok(name) = d.name() {
                out.push(name);
            }
        }
    }
    out
}

fn pick_device(name: &str) -> Result<cpal::Device, String> {
    let host = cpal::default_host();
    if name == DEFAULT_DEVICE || name.is_empty() {
        return host.default_input_device().ok_or_else(|| "no default input device".into());
    }
    let devices = host.input_devices().map_err(|e| e.to_string())?;
    for d in devices {
        if d.name().map(|n| n == name).unwrap_or(false) {
            return Ok(d);
        }
    }
    // fall back to default rather than dying if the saved device is gone
    host.default_input_device()
        .ok_or_else(|| format!("device '{name}' not found and no default input"))
}

/// What a Capture should do beyond metering.
pub struct CaptureCfg {
    /// Final output file; None = meter-only (armed mode).
    pub encode_to: Option<PathBuf>,
    pub target_channels: u16,
    /// Also emit 16 kHz mono i16 chunks for the live-caption pipeline.
    pub captions: bool,
}

enum Cmd {
    Pause,
    Resume,
    Stop(Sender<Result<(), String>>),
}

struct Timing {
    recorded: Duration,
    seg_start: Option<Instant>,
}

/// Native in-process mic capture (cpal / CoreAudio). The realtime callback
/// only copies samples into a channel; a writer thread does metering,
/// caption decimation, and FLAC encoding (sox stdin) in segments so pause
/// and crash recovery work like the Go version.
pub struct Capture {
    _stream: cpal::Stream,
    pub ticks: Receiver<MeterTick>,
    pub errs: Receiver<String>,
    pub caption_pcm: Receiver<Vec<i16>>,
    cmd_tx: Sender<Cmd>,
    paused: Arc<AtomicBool>,
    timing: Arc<Mutex<Timing>>,
    seg_dir: Option<PathBuf>,
    segments: Arc<Mutex<Vec<PathBuf>>>,
    file: Option<PathBuf>,
}

impl Capture {
    pub fn start(device_name: &str, cfg: CaptureCfg) -> Result<Capture, String> {
        let device = pick_device(device_name)?;
        let dconf = device.default_input_config().map_err(|e| e.to_string())?;
        let sample_rate = dconf.sample_rate().0;
        let in_channels = dconf.channels() as usize;
        let format = dconf.sample_format();

        let (raw_tx, raw_rx) = bounded::<Vec<f32>>(256);
        let (tick_tx, tick_rx) = bounded::<MeterTick>(256);
        let (err_tx, err_rx) = bounded::<String>(8);
        let (cap_tx, cap_rx) = bounded::<Vec<i16>>(64);
        let (cmd_tx, cmd_rx) = bounded::<Cmd>(4);

        let paused = Arc::new(AtomicBool::new(false));
        let timing = Arc::new(Mutex::new(Timing { recorded: Duration::ZERO, seg_start: Some(Instant::now()) }));
        let segments = Arc::new(Mutex::new(Vec::<PathBuf>::new()));

        let mut seg_dir = None;
        if let Some(final_file) = &cfg.encode_to {
            let dir = mk_session_dir()?;
            write_meta(&dir, &SessionMeta { file: final_file.to_string_lossy().into_owned(), channels: cfg.target_channels });
            seg_dir = Some(dir);
        }

        // realtime callback: copy out and leave
        let err_cb = err_tx.clone();
        let stream_conf: cpal::StreamConfig = dconf.clone().into();
        let stream = match format {
            cpal::SampleFormat::F32 => device.build_input_stream(
                &stream_conf,
                {
                    let raw_tx = raw_tx.clone();
                    move |data: &[f32], _: &cpal::InputCallbackInfo| {
                        if let Err(TrySendError::Disconnected(_)) = raw_tx.try_send(data.to_vec()) {}
                    }
                },
                move |e| { let _ = err_cb.try_send(format!("input stream: {e}")); },
                None,
            ),
            cpal::SampleFormat::I16 => device.build_input_stream(
                &stream_conf,
                {
                    let raw_tx = raw_tx.clone();
                    move |data: &[i16], _: &cpal::InputCallbackInfo| {
                        let v: Vec<f32> = data.iter().map(|&s| s as f32 / 32768.0).collect();
                        if let Err(TrySendError::Disconnected(_)) = raw_tx.try_send(v) {}
                    }
                },
                move |e| { let _ = err_cb.try_send(format!("input stream: {e}")); },
                None,
            ),
            other => return Err(format!("unsupported sample format {other:?}")),
        }
        .map_err(|e| format!("opening input stream: {e}"))?;
        stream.play().map_err(|e| format!("starting input stream: {e}"))?;

        // writer thread
        let w = Writer {
            raw_rx,
            tick_tx,
            err_tx,
            cap_tx: if cfg.captions { Some(cap_tx) } else { None },
            cmd_rx,
            paused: paused.clone(),
            segments: segments.clone(),
            seg_dir: seg_dir.clone(),
            sample_rate,
            in_channels,
            target_channels: cfg.target_channels.clamp(1, 2) as usize,
        };
        std::thread::spawn(move || w.run());

        Ok(Capture {
            _stream: stream,
            ticks: tick_rx,
            errs: err_rx,
            caption_pcm: cap_rx,
            cmd_tx,
            paused,
            timing,
            seg_dir,
            segments,
            file: cfg.encode_to,
        })
    }

    pub fn pause(&self) {
        if !self.paused.swap(true, Ordering::SeqCst) {
            let mut t = self.timing.lock().unwrap();
            if let Some(s) = t.seg_start.take() {
                t.recorded += s.elapsed();
            }
            let _ = self.cmd_tx.send(Cmd::Pause);
        }
    }

    pub fn resume(&self) {
        if self.paused.swap(false, Ordering::SeqCst) {
            self.timing.lock().unwrap().seg_start = Some(Instant::now());
            let _ = self.cmd_tx.send(Cmd::Resume);
        }
    }

    pub fn paused(&self) -> bool {
        self.paused.load(Ordering::SeqCst)
    }

    pub fn elapsed(&self) -> Duration {
        let t = self.timing.lock().unwrap();
        t.recorded + t.seg_start.map(|s| s.elapsed()).unwrap_or_default()
    }

    pub fn file_size(&self) -> u64 {
        if let Some(f) = &self.file {
            if let Ok(md) = std::fs::metadata(f) {
                return md.len();
            }
        }
        self.segments
            .lock()
            .unwrap()
            .iter()
            .filter_map(|p| std::fs::metadata(p).ok())
            .map(|m| m.len())
            .sum()
    }

    /// Stop capture and assemble the final file from segments.
    pub fn stop(self) -> Result<(), String> {
        {
            let mut t = self.timing.lock().unwrap();
            if let Some(s) = t.seg_start.take() {
                t.recorded += s.elapsed();
            }
        }
        drop(self._stream); // ends the callback; writer drains and sees EOF
        let (ack_tx, ack_rx) = bounded(1);
        let _ = self.cmd_tx.send(Cmd::Stop(ack_tx));
        let enc_result = ack_rx
            .recv_timeout(Duration::from_secs(5))
            .unwrap_or_else(|_| Err("encoder did not finish".into()));
        let Some(file) = &self.file else { return Ok(()) }; // meter-only
        enc_result?;
        let segs = self.segments.lock().unwrap().clone();
        let res = concat_flac(&segs, file);
        if let Some(dir) = &self.seg_dir {
            let _ = std::fs::remove_dir_all(dir);
        }
        res
    }
}

fn mk_session_dir() -> Result<PathBuf, String> {
    let base = sessions_dir();
    std::fs::create_dir_all(&base).map_err(|e| e.to_string())?;
    let dir = base.join(format!("rec-{}", chrono::Local::now().format("%Y%m%d-%H%M%S%.3f")));
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    Ok(dir)
}

struct Writer {
    raw_rx: Receiver<Vec<f32>>,
    tick_tx: Sender<MeterTick>,
    err_tx: Sender<String>,
    cap_tx: Option<Sender<Vec<i16>>>,
    cmd_rx: Receiver<Cmd>,
    paused: Arc<AtomicBool>,
    segments: Arc<Mutex<Vec<PathBuf>>>,
    seg_dir: Option<PathBuf>,
    sample_rate: u32,
    in_channels: usize,
    target_channels: usize,
}

struct Encoder {
    child: Child,
}

impl Encoder {
    fn spawn(sox: &Path, rate: u32, channels: usize, out: &Path) -> Result<Encoder, String> {
        let child = Command::new(sox)
            .args(["-q", "-t", "raw", "-e", "floating-point", "-b", "32"])
            .args(["-c", &channels.to_string(), "-r", &rate.to_string(), "-"])
            .arg(out)
            .stdin(Stdio::piped())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .map_err(|e| format!("starting encoder: {e}"))?;
        Ok(Encoder { child })
    }

    fn write(&mut self, bytes: &[u8]) -> Result<(), String> {
        self.child
            .stdin
            .as_mut()
            .ok_or("encoder stdin gone")?
            .write_all(bytes)
            .map_err(|e| format!("encoder write: {e}"))
    }

    fn finish(mut self) -> Result<(), String> {
        drop(self.child.stdin.take());
        match self.child.wait() {
            Ok(s) if s.success() => Ok(()),
            Ok(s) => Err(format!("encoder exited: {s}")),
            Err(e) => Err(format!("encoder wait: {e}")),
        }
    }
}

impl Writer {
    fn run(self) {
        let sox = find_bin("sox");
        let mut enc: Option<Encoder> = None;
        let mut enc_failed = false;
        if let Some(dir) = &self.seg_dir {
            match self.new_segment(sox.as_deref(), dir, 0) {
                Ok(e) => enc = Some(e),
                Err(e) => {
                    let _ = self.err_tx.try_send(e);
                    enc_failed = true;
                }
            }
        }
        let mut seg_n = 1usize;

        let samples_per_tick = (self.sample_rate / TICK_HZ).max(1) as usize;
        let (mut n, mut sum2_l, mut peak_l, mut sum2_r, mut peak_r, mut clip) =
            (0usize, 0f64, 0f64, 0f64, 0f64, false);

        // caption decimation to 16 kHz mono
        let cap_step = self.sample_rate as f64 / 16000.0;
        let mut cap_phase = 0f64;
        let mut cap_buf: Vec<i16> = Vec::with_capacity(4096);

        let mut out_bytes: Vec<u8> = Vec::with_capacity(1 << 15);

        loop {
            // control first so pause/stop act promptly
            while let Ok(cmd) = self.cmd_rx.try_recv() {
                match cmd {
                    Cmd::Pause => {
                        if let Some(e) = enc.take() {
                            let _ = e.finish();
                        }
                    }
                    Cmd::Resume => {
                        if let Some(dir) = &self.seg_dir.clone() {
                            match self.new_segment(sox.as_deref(), dir, seg_n) {
                                Ok(e) => {
                                    enc = Some(e);
                                    seg_n += 1;
                                }
                                Err(e) => { let _ = self.err_tx.try_send(e); }
                            }
                        }
                    }
                    Cmd::Stop(ack) => {
                        let res = match enc.take() {
                            Some(e) => e.finish(),
                            None if enc_failed => Err("encoder never started".into()),
                            None => Ok(()),
                        };
                        let _ = ack.send(res);
                        return;
                    }
                }
            }

            let chunk = match self.raw_rx.recv_timeout(Duration::from_millis(50)) {
                Ok(c) => c,
                Err(crossbeam_channel::RecvTimeoutError::Timeout) => continue,
                Err(crossbeam_channel::RecvTimeoutError::Disconnected) => {
                    // stream gone; wait for Stop
                    if let Ok(Cmd::Stop(ack)) = self.cmd_rx.recv_timeout(Duration::from_secs(5)) {
                        let res = match enc.take() {
                            Some(e) => e.finish(),
                            None => Ok(()),
                        };
                        let _ = ack.send(res);
                    }
                    return;
                }
            };

            let frames = chunk.len() / self.in_channels.max(1);
            let paused = self.paused.load(Ordering::SeqCst);
            out_bytes.clear();

            for f in 0..frames {
                let base = f * self.in_channels;
                let l = chunk[base] as f64;
                let r = if self.in_channels > 1 { chunk[base + 1] as f64 } else { l };

                // meter
                let (al, ar) = (l.abs(), r.abs());
                if al > peak_l { peak_l = al }
                if ar > peak_r { peak_r = ar }
                if al >= CLIP_THRESHOLD || ar >= CLIP_THRESHOLD { clip = true }
                sum2_l += l * l;
                sum2_r += r * r;
                n += 1;
                if n >= samples_per_tick {
                    let maxp = peak_l.max(peak_r);
                    let tick = MeterTick {
                        rms: lvl((sum2_l / n as f64).sqrt()),
                        peak: lvl(peak_l),
                        rms_r: lvl((sum2_r / n as f64).sqrt()),
                        peak_r: lvl(peak_r),
                        db: if maxp > 0.0 { 20.0 * maxp.log10() } else { -99.0 },
                        clip,
                    };
                    let _ = self.tick_tx.try_send(tick);
                    n = 0; sum2_l = 0.0; peak_l = 0.0; sum2_r = 0.0; peak_r = 0.0; clip = false;
                }

                // captions (mono mixdown, decimated)
                if let Some(cap_tx) = &self.cap_tx {
                    cap_phase += 1.0;
                    if cap_phase >= cap_step {
                        cap_phase -= cap_step;
                        let mono = ((l + r) / 2.0).clamp(-1.0, 1.0);
                        cap_buf.push((mono * 32767.0) as i16);
                        if cap_buf.len() >= 1600 { // 100 ms batches
                            let _ = cap_tx.try_send(std::mem::take(&mut cap_buf));
                        }
                    }
                }

                // encode
                if enc.is_some() && !paused {
                    if self.target_channels == 1 {
                        let mono = ((l + r) / 2.0) as f32;
                        out_bytes.extend_from_slice(&mono.to_le_bytes());
                    } else {
                        out_bytes.extend_from_slice(&(l as f32).to_le_bytes());
                        out_bytes.extend_from_slice(&(r as f32).to_le_bytes());
                    }
                }
            }

            if !out_bytes.is_empty() {
                if let Some(e) = enc.as_mut() {
                    if let Err(err) = e.write(&out_bytes) {
                        let _ = self.err_tx.try_send(err);
                        enc = None;
                        enc_failed = true;
                    }
                }
            }
        }
    }

    fn new_segment(&self, sox: Option<&Path>, dir: &Path, idx: usize) -> Result<Encoder, String> {
        let sox = sox.ok_or("sox not found — install it (brew install sox, or nix)")?;
        let seg = dir.join(format!("seg-{idx:03}.flac"));
        let enc = Encoder::spawn(sox, self.sample_rate, self.target_channels, &seg)?;
        self.segments.lock().unwrap().push(seg);
        Ok(enc)
    }
}
