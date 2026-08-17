use std::path::{Path, PathBuf};
use std::time::SystemTime;

use crate::state::is_own_recording;
use crate::transcribe::audio_stem;

#[derive(Clone)]
pub struct LibEntry {
    pub path: PathBuf,
    pub modified: SystemTime,
    pub size: u64,
    pub has_tx: bool,
}

pub fn load_library(dir: &str) -> Vec<LibEntry> {
    let Ok(entries) = std::fs::read_dir(dir) else {
        return Vec::new();
    };
    let mut out: Vec<LibEntry> = entries
        .flatten()
        .filter_map(|e| {
            let path = e.path();
            if path.extension().map(|x| x != "flac").unwrap_or(true) {
                return None;
            }
            let md = e.metadata().ok()?;
            let tx = path.with_extension("");
            Some(LibEntry {
                modified: md.modified().unwrap_or(SystemTime::UNIX_EPOCH),
                size: md.len(),
                has_tx: tx.is_dir(),
                path,
            })
        })
        .collect();
    out.sort_by_key(|e| std::cmp::Reverse(e.modified));
    out.truncate(200);
    out
}

/// One matched transcript line.
#[derive(Clone)]
pub struct SearchHit {
    pub audio: PathBuf,
    pub file: PathBuf,
    pub line_no: usize,
    pub snippet: String,
}

/// Best transcript text for a recording: the named markdown if present,
/// else whisper's plain txt.
pub fn transcript_file_for(audio: &Path) -> Option<PathBuf> {
    let dir = audio.with_extension("");
    let stem = audio_stem(audio);
    let md = dir.join(format!("{stem}-transcript.md"));
    if md.exists() {
        return Some(md);
    }
    let txt = dir.join(format!("{stem}.txt"));
    txt.exists().then_some(txt)
}

/// Case-insensitive grep across every transcript. Caps: 3 hits per
/// recording, 60 overall.
pub fn search_transcripts(entries: &[LibEntry], query: &str) -> Vec<SearchHit> {
    let query = query.trim().to_lowercase();
    if query.is_empty() {
        return Vec::new();
    }
    let mut hits = Vec::new();
    for e in entries {
        if !e.has_tx {
            continue;
        }
        let Some(tf) = transcript_file_for(&e.path) else { continue };
        let Ok(data) = std::fs::read_to_string(&tf) else { continue };
        let mut per_file = 0;
        for (i, line) in data.lines().enumerate() {
            let lower = line.to_lowercase();
            let Some(idx) = lower.find(&query) else { continue };
            let snippet = make_snippet(line.trim(), idx.min(line.trim().len()));
            hits.push(SearchHit { audio: e.path.clone(), file: tf.clone(), line_no: i + 1, snippet });
            per_file += 1;
            if per_file >= 3 {
                break;
            }
        }
        if hits.len() >= 60 {
            break;
        }
    }
    hits
}

fn make_snippet(line: &str, match_at: usize) -> String {
    let chars: Vec<char> = line.chars().collect();
    if chars.len() <= 120 {
        return line.to_string();
    }
    // approximate char index of the byte match position
    let approx = line[..match_at.min(line.len())].chars().count();
    let start = approx.saturating_sub(40);
    let end = (start + 120).min(chars.len());
    format!("…{}…", chars[start..end].iter().collect::<String>())
}

/// Lines around a hit for the done-screen preview.
pub fn hit_context(h: &SearchHit, around: usize) -> Vec<String> {
    let Ok(data) = std::fs::read_to_string(&h.file) else {
        return Vec::new();
    };
    let lines: Vec<&str> = data.lines().collect();
    let start = h.line_no.saturating_sub(1).saturating_sub(around);
    let end = (h.line_no + around).min(lines.len());
    lines[start..end]
        .iter()
        .map(|l| l.trim())
        .filter(|l| !l.is_empty())
        .map(|l| l.to_string())
        .collect()
}

/// Recording deletion: audio + transcript dir + markers.
pub fn delete_recording(audio: &Path) {
    let _ = std::fs::remove_file(audio);
    let _ = std::fs::remove_dir_all(audio.with_extension(""));
    let stem = audio_stem(audio);
    if let Some(dir) = audio.parent() {
        let _ = std::fs::remove_file(dir.join(format!("{stem}-markers.txt")));
    }
}

/// True when retention would consider this file (exposed for tests).
#[allow(dead_code)]
pub fn retention_applies(name: &str) -> bool {
    is_own_recording(name)
}
