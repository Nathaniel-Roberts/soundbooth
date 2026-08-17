use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use crate::state::find_bin;

const SPOOL_SEGMENT_SECONDS: u32 = 60;

fn now_unix() -> i64 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap_or_default().as_secs() as i64
}

/// The armed replay buffer: ffmpeg spools gapless 60 s FLAC segments; a
/// janitor deletes anything older than the window until a save freezes
/// retention. Nothing is retained without an explicit trigger.
pub struct Spooler {
    pub dir: PathBuf,
    ffmpeg: Child,
    keep_after: Arc<AtomicI64>, // unix secs; segments ending before this die
    frozen: Arc<AtomicBool>,
    stop_flag: Arc<AtomicBool>,
    window: Duration,
    started: std::time::Instant,
}

/// avfoundation needs a device index; resolve it from ffmpeg's listing by
/// name (cpal names match avfoundation names). None = default.
pub fn av_index_for(device_name: &str) -> Option<u32> {
    let ffmpeg = find_bin("ffmpeg")?;
    let out = Command::new(ffmpeg)
        .args(["-f", "avfoundation", "-list_devices", "true", "-i", ""])
        .output()
        .ok()?;
    let text = String::from_utf8_lossy(&out.stderr);
    let mut in_audio = false;
    for line in text.lines() {
        if line.contains("AVFoundation audio devices") {
            in_audio = true;
            continue;
        }
        if line.contains("AVFoundation video devices") {
            in_audio = false;
            continue;
        }
        if !in_audio {
            continue;
        }
        // "... [0] MacBook Pro Microphone"
        if let Some(rb) = line.rfind(']') {
            if let Some(lb) = line[..rb].rfind('[') {
                let idx: Option<u32> = line[lb + 1..rb].parse().ok();
                let name = line[rb + 1..].trim();
                if let Some(idx) = idx {
                    if name == device_name {
                        return Some(idx);
                    }
                }
            }
        }
    }
    None
}

pub fn start_spooler(av_index: Option<u32>, channels: u16, window: Duration) -> Result<Spooler, String> {
    let ffmpeg = find_bin("ffmpeg").ok_or("ffmpeg not found — armed mode needs it for gapless buffering")?;
    let dir = std::env::temp_dir().join(format!("soundbooth-spool-{}", std::process::id()));
    std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
    let input = match av_index {
        Some(i) => format!(":{i}"),
        None => ":default".into(),
    };
    let child = Command::new(ffmpeg)
        .args(["-hide_banner", "-loglevel", "error", "-f", "avfoundation", "-i", &input])
        .args(["-ac", &channels.clamp(1, 2).to_string(), "-ar", "48000"])
        .args(["-f", "segment", "-segment_time", &SPOOL_SEGMENT_SECONDS.to_string()])
        .args(["-reset_timestamps", "1", "-strftime", "1"])
        .arg(dir.join("%Y%m%d-%H%M%S.flac"))
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| format!("starting buffer capture: {e}"))?;

    let keep_after = Arc::new(AtomicI64::new(now_unix() - window.as_secs() as i64));
    let frozen = Arc::new(AtomicBool::new(false));
    let stop_flag = Arc::new(AtomicBool::new(false));

    // janitor
    {
        let dir = dir.clone();
        let keep_after = keep_after.clone();
        let frozen = frozen.clone();
        let stop_flag = stop_flag.clone();
        let win = window.as_secs() as i64;
        std::thread::spawn(move || loop {
            for _ in 0..20 {
                if stop_flag.load(Ordering::SeqCst) {
                    return;
                }
                std::thread::sleep(Duration::from_secs(1));
            }
            if !frozen.load(Ordering::SeqCst) {
                keep_after.store(now_unix() - win, Ordering::SeqCst);
            }
            let keep = keep_after.load(Ordering::SeqCst);
            if let Ok(entries) = std::fs::read_dir(&dir) {
                for e in entries.flatten() {
                    if mtime_unix(&e.path()) < keep {
                        let _ = std::fs::remove_file(e.path());
                    }
                }
            }
        });
    }

    Ok(Spooler {
        dir,
        ffmpeg: child,
        keep_after,
        frozen,
        stop_flag,
        window,
        started: std::time::Instant::now(),
    })
}

fn mtime_unix(p: &Path) -> i64 {
    std::fs::metadata(p)
        .and_then(|m| m.modified())
        .ok()
        .and_then(|t| t.duration_since(UNIX_EPOCH).ok())
        .map(|d| d.as_secs() as i64)
        .unwrap_or(i64::MAX)
}

impl Spooler {
    /// Freeze retention at (now - window): everything from the last N
    /// minutes onward is kept from here until stop.
    pub fn trigger(&self) {
        self.frozen.store(true, Ordering::SeqCst);
        self.keep_after.store(now_unix() - self.window.as_secs() as i64, Ordering::SeqCst);
    }

    pub fn buffered(&self) -> Duration {
        self.started.elapsed().min(self.window)
    }

    /// Stop capture; return kept segments oldest-first. Caller owns
    /// concatenation and cleanup.
    pub fn stop(mut self) -> Vec<PathBuf> {
        self.stop_flag.store(true, Ordering::SeqCst);
        unsafe { libc::kill(self.ffmpeg.id() as i32, libc::SIGINT) };
        let deadline = std::time::Instant::now() + Duration::from_secs(3);
        loop {
            match self.ffmpeg.try_wait() {
                Ok(Some(_)) => break,
                _ if std::time::Instant::now() > deadline => {
                    let _ = self.ffmpeg.kill();
                    let _ = self.ffmpeg.wait();
                    break;
                }
                _ => std::thread::sleep(Duration::from_millis(30)),
            }
        }
        let keep = self.keep_after.load(Ordering::SeqCst);
        let mut segs: Vec<PathBuf> = std::fs::read_dir(&self.dir)
            .map(|entries| {
                entries
                    .flatten()
                    .map(|e| e.path())
                    .filter(|p| {
                        std::fs::metadata(p).map(|m| m.len() > 0).unwrap_or(false)
                            && mtime_unix(p) >= keep
                    })
                    .collect()
            })
            .unwrap_or_default();
        segs.sort();
        segs
    }

    pub fn cleanup_dir(dir: &Path) {
        let _ = std::fs::remove_dir_all(dir);
    }
}
