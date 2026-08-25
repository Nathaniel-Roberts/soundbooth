use crossbeam_channel::{bounded, Receiver};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};

use crate::audio::lvl;
use crate::state::find_bin;
use crate::waveform::WaveCol;

/// soxi duration in seconds.
pub fn probe_duration(file: &Path) -> Result<f64, String> {
    let soxi = find_bin("soxi").ok_or("soxi not found")?;
    let out = Command::new(soxi).arg("-D").arg(file).output().map_err(|e| e.to_string())?;
    String::from_utf8_lossy(&out.stdout).trim().parse().map_err(|_| "bad duration".into())
}

pub struct DecodedWave {
    pub file: PathBuf,
    pub cols: Vec<WaveCol>,
    pub dur: f64,
}

/// Decode a whole file into pooled waveform columns on a worker thread.
pub fn decode_wave(file: PathBuf, sub_cols: usize) -> Receiver<Result<DecodedWave, String>> {
    let (tx, rx) = bounded(1);
    std::thread::spawn(move || {
        let _ = tx.send(decode_wave_sync(&file, sub_cols).map(|(cols, dur)| DecodedWave { file, cols, dur }));
    });
    rx
}

fn decode_wave_sync(file: &Path, sub_cols: usize) -> Result<(Vec<WaveCol>, f64), String> {
    let dur = probe_duration(file)?;
    if dur <= 0.0 || sub_cols == 0 {
        return Err("empty file".into());
    }
    let sox = find_bin("sox").ok_or("sox not found")?;
    let rate = ((sub_cols as f64 * 100.0 / dur) as u32).clamp(200, 8000);
    let out = Command::new(sox)
        .arg(file)
        .args(["-t", "raw", "-e", "signed", "-b", "16", "-c", "1", "-r", &rate.to_string(), "-"])
        .output()
        .map_err(|e| e.to_string())?;
    if !out.status.success() {
        return Err("decode failed".into());
    }
    // note: clippy on CI (newer stable) insists on as_chunks here
    let (pairs, _) = out.stdout.as_chunks::<2>();
    let samples: Vec<i16> = pairs.iter().map(|b| i16::from_le_bytes(*b)).collect();
    if samples.is_empty() {
        return Err("empty decode".into());
    }
    let per = (samples.len() / sub_cols).max(1);
    let mut cols = Vec::with_capacity(sub_cols);
    let (mut sum2, mut peak, mut n) = (0f64, 0f64, 0usize);
    for &s in &samples {
        let v = s as f64 / 32768.0;
        let av = v.abs();
        if av > peak {
            peak = av;
        }
        sum2 += v * v;
        n += 1;
        if n >= per && cols.len() < sub_cols {
            cols.push(WaveCol {
                rms: lvl((sum2 / n as f64).sqrt()),
                peak: lvl(peak),
                ..Default::default()
            });
            n = 0;
            sum2 = 0.0;
            peak = 0.0;
        }
    }
    Ok((cols, dur))
}

/// Playback via sox to the default output device, from an offset.
pub struct Playback {
    child: Child,
    pub ended: Receiver<u64>,
}

pub fn start_playback(file: &Path, offset: f64, gen: u64) -> Result<Playback, String> {
    let sox = find_bin("sox").ok_or("sox not found")?;
    let child = Command::new(sox)
        .arg("-q")
        .arg(file)
        .args(["-d", "trim", &format!("{:.2}", offset.max(0.0))])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .map_err(|e| e.to_string())?;
    let (tx, rx) = bounded(1);
    let pid = child.id();
    std::thread::spawn(move || {
        // poll: we cannot wait() without owning the child
        loop {
            if unsafe { libc::kill(pid as i32, 0) } != 0 {
                let _ = tx.send(gen);
                return;
            }
            std::thread::sleep(std::time::Duration::from_millis(200));
        }
    });
    Ok(Playback { child, ended: rx })
}

impl Playback {
    pub fn stop(mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
    /// Reap the child after a natural end (avoids zombies).
    pub fn reap(mut self) {
        let _ = self.child.wait();
    }
}
