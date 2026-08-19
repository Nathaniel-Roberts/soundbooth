use serde::{Deserialize, Serialize};
use std::path::{Path, PathBuf};
use std::time::{Duration, SystemTime};

/// Crash-recoverable session segments and the control socket live here.
pub fn state_dir() -> PathBuf {
    let base = std::env::var_os("XDG_STATE_HOME")
        .map(PathBuf::from)
        .unwrap_or_else(|| dirs::home_dir().unwrap_or_default().join(".local/state"));
    base.join("soundbooth")
}

pub fn sessions_dir() -> PathBuf {
    state_dir().join("sessions")
}

/// Enough about an in-flight recording to finish it after a crash.
#[derive(Serialize, Deserialize, Default, Clone)]
pub struct SessionMeta {
    #[serde(default)]
    pub file: String,
    #[serde(default)]
    pub channels: u16,
}

pub fn write_meta(dir: &Path, meta: &SessionMeta) {
    let _ = std::fs::write(
        dir.join("meta.json"),
        serde_json::to_string(meta).unwrap_or_default(),
    );
}

pub fn read_meta(dir: &Path) -> SessionMeta {
    std::fs::read_to_string(dir.join("meta.json"))
        .ok()
        .and_then(|s| serde_json::from_str(&s).ok())
        .unwrap_or_default()
}

/// An unfinished session left behind by a crash.
pub struct Orphan {
    pub dir: PathBuf,
    pub meta: SessionMeta,
    pub segments: Vec<PathBuf>,
    pub bytes: u64,
}

impl Orphan {
    /// Rough duration from FLAC size (~50 KB/s per channel of speech).
    pub fn est_duration(&self) -> Duration {
        let per_sec = 50 * 1024 * self.meta.channels.max(1) as u64;
        Duration::from_secs(self.bytes / per_sec)
    }
}

/// Scan for crashed sessions with audio in them; remove empty leftovers.
pub fn find_orphans() -> Vec<Orphan> {
    let Ok(entries) = std::fs::read_dir(sessions_dir()) else {
        return Vec::new();
    };
    let mut out = Vec::new();
    for e in entries.flatten() {
        let dir = e.path();
        if !dir.is_dir() {
            continue;
        }
        let mut o = Orphan { meta: read_meta(&dir), dir: dir.clone(), segments: Vec::new(), bytes: 0 };
        if let Ok(files) = std::fs::read_dir(&dir) {
            for f in files.flatten() {
                let p = f.path();
                let name = p.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
                if name.starts_with("seg-") && name.ends_with(".flac") {
                    if let Ok(md) = p.metadata() {
                        if md.len() > 0 {
                            o.bytes += md.len();
                            o.segments.push(p);
                        }
                    }
                }
            }
        }
        if o.segments.is_empty() {
            let _ = std::fs::remove_dir_all(&dir);
            continue;
        }
        // a session whose segments were written in the last two minutes is
        // almost certainly another live soundbooth instance, not a crash —
        // offering to "recover" (or delete!) it would corrupt a recording
        // in progress
        let newest = o
            .segments
            .iter()
            .filter_map(|p| std::fs::metadata(p).ok())
            .filter_map(|m| m.modified().ok())
            .max();
        if let Some(newest) = newest {
            if SystemTime::now().duration_since(newest).unwrap_or_default() < Duration::from_secs(120) {
                continue;
            }
        }
        o.segments.sort();
        out.push(o);
    }
    out
}

/// Remove stale armed-mode spools (the buffer must never survive a crash)
/// and day-old stray mic/system track parts.
pub fn sweep_stale() {
    if let Ok(entries) = std::fs::read_dir(std::env::temp_dir()) {
        for e in entries.flatten() {
            if e.file_name().to_string_lossy().starts_with("soundbooth-spool-") {
                let _ = std::fs::remove_dir_all(e.path());
            }
        }
    }
    let cutoff = SystemTime::now() - Duration::from_secs(24 * 3600);
    if let Ok(entries) = std::fs::read_dir(sessions_dir()) {
        for e in entries.flatten() {
            let name = e.file_name().to_string_lossy().into_owned();
            if (name.starts_with("mic-") || name.starts_with("sys-")) && name.ends_with(".flac") {
                if let Ok(md) = e.metadata() {
                    if md.modified().map(|m| m < cutoff).unwrap_or(false) {
                        let _ = std::fs::remove_file(e.path());
                    }
                }
            }
        }
    }
}

/// True for soundbooth's own `<name>-YYYYMMDD-HHMMSS.flac` filenames —
/// retention must never touch anything else.
pub fn is_own_recording(name: &str) -> bool {
    let Some(stem) = name.strip_suffix(".flac") else { return false };
    let b = stem.as_bytes();
    if b.len() < 17 {
        return false;
    }
    let tail = &b[b.len() - 16..];
    // -YYYYMMDD-HHMMSS
    tail[0] == b'-'
        && tail[1..9].iter().all(u8::is_ascii_digit)
        && tail[9] == b'-'
        && tail[10..16].iter().all(u8::is_ascii_digit)
}

/// Delete own recordings older than `days`; transcripts and markers stay.
pub fn sweep_retention(dir: &str, days: u32) -> usize {
    if days == 0 {
        return 0;
    }
    let cutoff = SystemTime::now() - Duration::from_secs(days as u64 * 24 * 3600);
    let Ok(entries) = std::fs::read_dir(dir) else { return 0 };
    let mut n = 0;
    for e in entries.flatten() {
        let name = e.file_name().to_string_lossy().into_owned();
        if !is_own_recording(&name) {
            continue;
        }
        let Ok(md) = e.metadata() else { continue };
        if md.modified().map(|m| m < cutoff).unwrap_or(false) && std::fs::remove_file(e.path()).is_ok() {
            n += 1;
        }
    }
    n
}

/// Available disk space at path, or None if unknowable.
pub fn free_bytes(path: &str) -> Option<u64> {
    use std::ffi::CString;
    let c = CString::new(path).ok()?;
    let mut st: libc::statvfs = unsafe { std::mem::zeroed() };
    if unsafe { libc::statvfs(c.as_ptr(), &mut st) } != 0 {
        return None;
    }
    Some(st.f_bavail as u64 * st.f_frsize as u64)
}

/// Resolve a helper binary from PATH plus the usual mac locations.
pub fn find_bin(name: &str) -> Option<PathBuf> {
    if let Some(paths) = std::env::var_os("PATH") {
        for dir in std::env::split_paths(&paths) {
            let p = dir.join(name);
            if is_executable(&p) {
                return Some(p);
            }
        }
    }
    let home = dirs::home_dir().unwrap_or_default();
    let user = std::env::var("USER").unwrap_or_default();
    for dir in [
        PathBuf::from("/opt/homebrew/bin"),
        PathBuf::from("/usr/local/bin"),
        PathBuf::from("/usr/bin"),
        PathBuf::from("/run/current-system/sw/bin"),
        PathBuf::from(format!("/etc/profiles/per-user/{user}/bin")),
        home.join(".local/bin"),
    ] {
        let p = dir.join(name);
        if is_executable(&p) {
            return Some(p);
        }
    }
    None
}

fn is_executable(p: &Path) -> bool {
    use std::os::unix::fs::PermissionsExt;
    p.metadata().map(|m| m.is_file() && m.permissions().mode() & 0o111 != 0).unwrap_or(false)
}

/// Join FLAC parts into one file (rename when there is only one).
pub fn concat_flac(parts: &[PathBuf], out: &Path) -> Result<(), String> {
    let mut parts: Vec<&PathBuf> = parts.iter().collect();
    parts.sort();
    match parts.len() {
        0 => Err("no audio captured".into()),
        1 => {
            if std::fs::rename(parts[0], out).is_ok() {
                return Ok(());
            }
            std::fs::copy(parts[0], out).map(|_| ()).map_err(|e| e.to_string())
        }
        _ => {
            let sox = find_bin("sox").ok_or("sox not found")?;
            let output = std::process::Command::new(sox)
                .args(parts.iter().map(|p| p.as_os_str()))
                .arg(out)
                .output()
                .map_err(|e| e.to_string())?;
            if !output.status.success() {
                return Err(format!(
                    "joining segments: {}",
                    String::from_utf8_lossy(&output.stderr).lines().next().unwrap_or("")
                ));
            }
            Ok(())
        }
    }
}

pub fn human_size(n: u64) -> String {
    match n {
        n if n > 1 << 30 => format!("{:.1} GB", n as f64 / (1u64 << 30) as f64),
        n if n > 1 << 20 => format!("{:.1} MB", n as f64 / (1u64 << 20) as f64),
        n if n > 1 << 10 => format!("{:.0} KB", n as f64 / 1024.0),
        n => format!("{n} B"),
    }
}
