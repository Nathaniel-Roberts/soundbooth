use ratatui::buffer::Buffer;
use ratatui::layout::Rect;
use ratatui::style::{Color, Style};
use ratatui::text::{Line, Span};

use crate::theme::{mix, Theme};

/// One rendered sub-column: one 25 ms meter tick = one Braille dot column.
#[derive(Clone, Copy, Default)]
pub struct WaveCol {
    pub rms: f64,
    pub peak: f64,
    pub rms_r: f64,
    pub peak_r: f64,
    pub clip: bool,
    pub paused: bool,
}

/// Bit for dot (x 0..1, y 0..3) inside a Braille cell.
const BRAILLE_BITS: [[u32; 4]; 2] = [[0x01, 0x02, 0x04, 0x40], [0x08, 0x10, 0x20, 0x80]];

/// Max-pool groups of z ticks so zoomed-out views keep peaks visible.
pub fn downsample(cols: &[WaveCol], z: usize) -> Vec<WaveCol> {
    if z <= 1 {
        return cols.to_vec();
    }
    let mut out = Vec::with_capacity(cols.len() / z + 1);
    let start = cols.len() % z;
    if start > 0 {
        out.push(pool(&cols[..start]));
    }
    let mut i = start;
    while i + z <= cols.len() {
        out.push(pool(&cols[i..i + z]));
        i += z;
    }
    out
}

fn pool(group: &[WaveCol]) -> WaveCol {
    let mut c = WaveCol::default();
    for g in group {
        c.peak = c.peak.max(g.peak);
        c.rms = c.rms.max(g.rms);
        c.peak_r = c.peak_r.max(g.peak_r);
        c.rms_r = c.rms_r.max(g.rms_r);
        c.clip |= g.clip;
        c.paused |= g.paused;
    }
    c
}

/// Draw an Audacity-style waveform into the buffer: mirrored around a
/// centre line, Braille dots for 2x4 sub-cell resolution, a vertical
/// Catppuccin gradient (lavender core to sapphire tips, RMS brightened),
/// red for clipped columns, grey for paused spans and the idle hairline.
/// `playhead` highlights one cell column (the player cursor).
// index loops mirror the Braille bit table layout; iterator chains here
// would obscure the dot maths
#[allow(clippy::needless_range_loop)]
pub fn render_wave(
    buf: &mut Buffer,
    area: Rect,
    cols: &[WaveCol],
    th: &Theme,
    playhead: Option<u16>,
    marks: &[u16],
) {
    let w_cells = area.width as usize;
    let h_cells = area.height as usize;
    if w_cells == 0 || h_cells == 0 {
        return;
    }
    let total_dots = h_cells * 4;
    let half = total_dots / 2;

    // newest 2*w sub-columns, right-aligned
    let need = w_cells * 2;
    let cols = if cols.len() > need { &cols[cols.len() - need..] } else { cols };
    let pad = need - cols.len();

    // per sub-column dot heights (envelope always >= 1: idle hairline)
    let mut env = vec![1usize; need];
    let mut core = vec![0usize; need];
    let mut clip = vec![false; need];
    let mut paused = vec![false; need];
    for (i, c) in cols.iter().enumerate() {
        let e = ((c.peak * half as f64 + 0.5) as usize).clamp(1, half);
        let k = ((c.rms * half as f64 + 0.5) as usize).min(e);
        env[pad + i] = e;
        core[pad + i] = k;
        clip[pad + i] = c.clip;
        paused[pad + i] = c.paused;
    }

    for row in 0..h_cells {
        // row gradient position: outermost dot's distance from centre
        let mut outer = 0usize;
        for dy in 0..4 {
            let y = row * 4 + dy;
            let dist = if y < half { half - 1 - y } else { y - half };
            outer = outer.max(dist + 1);
        }
        let env_colour = th.wave_ramp(outer as f64 / half as f64);
        let core_colour = mix(env_colour, th.text, 0.45);

        for cx in 0..w_cells {
            let mut bits = 0u32;
            let mut cell_core = true;
            let mut cell_clip = false;
            let mut cell_paused = false;
            let mut cell_max_env = 0usize;
            for sub in 0..2 {
                let ci = cx * 2 + sub;
                cell_clip |= clip[ci];
                cell_paused |= paused[ci];
                cell_max_env = cell_max_env.max(env[ci]);
                for dy in 0..4 {
                    let y = row * 4 + dy;
                    let dist = if y < half { half - 1 - y } else { y - half };
                    if dist < env[ci] {
                        bits |= BRAILLE_BITS[sub][dy];
                        if dist >= core[ci] {
                            cell_core = false;
                        }
                    }
                }
            }
            let x = area.x + cx as u16;
            let y = area.y + row as u16;
            let cell = &mut buf[(x, y)];
            if playhead == Some(cx as u16) {
                // player cursor: bright full-height dot column
                let c = char::from_u32(0x2800 + (bits | 0x01 | 0x02 | 0x04 | 0x40)).unwrap_or('⡇');
                cell.set_char(c).set_fg(th.text);
                continue;
            }
            if marks.contains(&(cx as u16)) {
                // marker: full-height mauve hairline through the wave
                let c = char::from_u32(0x2800 + (bits | 0x01 | 0x02 | 0x04 | 0x40)).unwrap_or('⡇');
                cell.set_char(c).set_fg(th.mauve);
                continue;
            }
            if bits == 0 {
                cell.set_char(' ');
                continue;
            }
            let colour = if cell_clip {
                th.red
            } else if cell_paused {
                th.overlay0
            } else if cell_max_env <= 1 {
                // idle hairline: fixed subtle grey
                mix(th.base, th.overlay0, 0.5)
            } else {
                // amplitude glow: loud columns burn bright, quiet ones
                // recede toward the background
                let base_colour = if cell_core { core_colour } else { env_colour };
                let boost = (0.35 + 0.65 * (cell_max_env as f64 / half as f64)).clamp(0.0, 1.0);
                mix(th.base, base_colour, boost)
            };
            let c = char::from_u32(0x2800 + bits).unwrap_or('⣿');
            cell.set_char(c).set_fg(colour);
        }
    }
}

/// Two stacked lanes (mic/L over system/R).
pub fn render_wave_stereo(
    buf: &mut Buffer,
    area: Rect,
    cols: &[WaveCol],
    th: &Theme,
    playhead: Option<u16>,
    marks: &[u16],
) {
    let lane = area.height / 2;
    if lane < 3 {
        render_wave(buf, area, cols, th, playhead, marks);
        return;
    }
    let right: Vec<WaveCol> = cols
        .iter()
        .map(|c| WaveCol { rms: c.rms_r, peak: c.peak_r, clip: c.clip, paused: c.paused, ..Default::default() })
        .collect();
    let top = Rect { height: lane, ..area };
    let bottom = Rect { y: area.y + lane, height: lane, ..area };
    render_wave(buf, top, cols, th, playhead, marks);
    render_wave(buf, bottom, &right, th, playhead, marks);
}

/// DAW-style time ruler: marks every 5/15 s back from the right edge,
/// marker arrows in mauve. Returns [marks, labels] lines.
pub fn ruler_lines(w_cells: usize, cell_ms: usize, marker_cells: &[usize], th: &Theme) -> [Line<'static>; 2] {
    let cell_ms = cell_ms.max(1);
    let step_sec = if cell_ms >= 200 { 15 } else { 5 };
    let step_cells = (step_sec * 1000 / cell_ms).max(1);

    let mut marks = vec!['╌'; w_cells];
    let mut labels = vec![' '; w_cells];
    let place = |pos: usize, s: &str, labels: &mut Vec<char>| {
        if s.len() > w_cells {
            return;
        }
        let start = pos.saturating_sub(s.len() / 2).min(w_cells - s.len());
        for (i, ch) in s.chars().enumerate() {
            labels[start + i] = ch;
        }
    };
    let mut k = 0usize;
    loop {
        let back = k * step_cells;
        if back >= w_cells {
            break;
        }
        let pos = w_cells - 1 - back;
        marks[pos] = '┴';
        if k == 0 {
            place(pos, "now", &mut labels);
        } else {
            place(pos, &format!("-{}s", k * step_sec), &mut labels);
        }
        k += 1;
    }
    let mut is_marker = vec![false; w_cells];
    for &back in marker_cells {
        if back < w_cells {
            let pos = w_cells - 1 - back;
            marks[pos] = '▼';
            is_marker[pos] = true;
        }
    }
    let dim = Style::default().fg(th.overlay0);
    let mauve = Style::default().fg(th.mauve);
    let mark_spans: Vec<Span> = marks
        .iter()
        .zip(&is_marker)
        .map(|(c, m)| Span::styled(c.to_string(), if *m { mauve } else { dim }))
        .collect();
    [
        Line::from(mark_spans),
        Line::from(Span::styled(labels.into_iter().collect::<String>(), dim)),
    ]
}

/// Gradient VU bar with a peak-hold marker; level and hold are 0..1.
/// Slim dotted track so it reads as a meter, not a slab.
pub fn vu_line(width: usize, level: f64, hold: f64, th: &Theme) -> Line<'static> {
    let width = width.max(10);
    let fill = (level * width as f64 + 0.5) as usize;
    let hold_pos = ((hold * width as f64) as usize).min(width - 1);
    let mut spans: Vec<Span> = Vec::with_capacity(width + 1);
    for i in 0..width {
        let t = i as f64 / (width - 1) as f64;
        let (ch, colour) = if i == hold_pos && hold > 0.01 {
            ("▌", th.text)
        } else if i < fill {
            ("█", th.vu_ramp(t))
        } else {
            ("·", th.surface0)
        };
        spans.push(Span::styled(ch, Style::default().fg(colour)));
    }
    let db = crate::audio::DB_FLOOR * (1.0 - hold);
    spans.push(Span::styled(format!(" {db:4.0} dB"), Style::default().fg(th.overlay0)));
    Line::from(spans)
}

/// Gradient progress bar spans, frac 0..1.
pub fn progress_spans(width: usize, frac: f64, th: &Theme) -> Vec<Span<'static>> {
    let frac = frac.clamp(0.0, 1.0);
    let fill = (frac * width as f64 + 0.5) as usize;
    (0..width)
        .map(|i| {
            if i < fill {
                let t = i as f64 / (width.max(2) - 1) as f64;
                Span::styled("█", Style::default().fg(th.wave_ramp(t)))
            } else {
                Span::styled("░", Style::default().fg(th.surface0))
            }
        })
        .collect()
}

/// Word-wrap to width columns.
pub fn wrap_text(s: &str, width: usize) -> Vec<String> {
    let words: Vec<&str> = s.split_whitespace().collect();
    if words.is_empty() {
        return Vec::new();
    }
    let mut out = Vec::new();
    let mut line = words[0].to_string();
    for w in &words[1..] {
        if line.chars().count() + 1 + w.chars().count() > width {
            out.push(std::mem::take(&mut line));
            line = w.to_string();
        } else {
            line.push(' ');
            line.push_str(w);
        }
    }
    out.push(line);
    out
}

/// Colour helper for the VU/status accents.
pub fn _unused(_: Color) {}
