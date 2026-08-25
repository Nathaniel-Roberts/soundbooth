use ratatui::layout::Rect;
use ratatui::style::{Modifier, Style};
use ratatui::text::{Line, Span};
use ratatui::widgets::{Block, BorderType, Borders, Paragraph};
use ratatui::Frame;
use std::time::Duration;

use crate::app::{App, Screen, F_BUFFER, F_THEME};
use crate::audio::DB_FLOOR;
use crate::search::LibEntry;
use crate::speakers::{display_name, fmt_clock};
use crate::state::human_size;
use crate::theme::Theme;
use crate::transcribe::STAGE_NAMES;
use crate::waveform::{downsample, progress_spans, render_wave, render_wave_stereo, ruler_lines, vu_line, wrap_text, WaveCol};

const SPINNER: [&str; 10] = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];

fn spinner(app: &App) -> &'static str {
    SPINNER[(app.frame / 3) as usize % SPINNER.len()]
}

pub fn fmt_dur(d: Duration) -> String {
    let s = d.as_secs();
    format!("{:02}:{:02}", s / 60, s % 60)
}

fn dim(th: &Theme) -> Style {
    Style::default().fg(th.overlay0)
}
fn label(th: &Theme) -> Style {
    Style::default().fg(th.subtext0)
}
fn value(th: &Theme) -> Style {
    Style::default().fg(th.text)
}
fn focus(th: &Theme) -> Style {
    Style::default().fg(th.blue).add_modifier(Modifier::BOLD)
}
fn ok(th: &Theme) -> Style {
    Style::default().fg(th.green)
}
fn warn(th: &Theme) -> Style {
    Style::default().fg(th.yellow)
}
fn err(th: &Theme) -> Style {
    Style::default().fg(th.red)
}

fn key_hints(th: &Theme, pairs: &[(&str, &str)]) -> Line<'static> {
    let mut spans = Vec::new();
    for (i, (k, d)) in pairs.iter().enumerate() {
        if i > 0 {
            spans.push(Span::styled("  ·  ", dim(th)));
        }
        spans.push(Span::styled(k.to_string(), Style::default().fg(th.sapphire)));
        spans.push(Span::raw(" "));
        spans.push(Span::styled(d.to_string(), Style::default().fg(th.overlay1)));
    }
    Line::from(spans)
}

fn build_rev() -> &'static str {
    option_env!("SB_REV").unwrap_or(env!("CARGO_PKG_VERSION"))
}

pub fn draw(f: &mut Frame, app: &mut App) {
    let th = app.theme;
    let area = f.area();
    // paint the theme's own background so the app looks right in any
    // terminal (and the Catppuccin flavours are actually visible)
    f.render_widget(
        Block::default().style(Style::default().bg(th.base).fg(th.text)),
        area,
    );
    let header = Line::from(vec![
        Span::styled("soundbooth", Style::default().fg(th.mauve).add_modifier(Modifier::BOLD)),
        Span::styled(format!("  record · meter · transcribe  ·  {}", build_rev()), dim(&th)),
    ]);
    f.render_widget(Paragraph::new(header), Rect { height: 1.min(area.height), ..area });
    let body = Rect {
        x: area.x,
        y: area.y + 2.min(area.height),
        width: area.width,
        height: area.height.saturating_sub(2),
    };
    if body.height == 0 {
        return;
    }
    match app.screen {
        Screen::Setup => draw_setup(f, app, body),
        Screen::Doctor => draw_doctor(f, app, body),
        Screen::Armed | Screen::Recording => draw_live(f, app, body),
        Screen::Transcribing => draw_transcribing(f, app, body),
        Screen::Speakers => draw_speakers(f, app, body),
        Screen::Library => draw_library(f, app, body),
        Screen::Done => draw_done(f, app, body),
    }
}

fn panel_block(th: &Theme) -> Block<'static> {
    Block::default()
        .borders(Borders::ALL)
        .border_type(BorderType::Rounded)
        .border_style(Style::default().fg(th.surface0))
}

// ----------------------------------------------------------------------
// setup

fn setup_rows(app: &App) -> Vec<(String, String)> {
    let cfg = &app.cfg;
    let on_off = |b: bool, on: &str, off: &str| if b { on.to_string() } else { off.to_string() };
    let mode = if cfg.mode == "armed" { "armed (replay buffer)" } else { "record now" };
    let buffer = if cfg.buffer_min == 0 {
        "off — record from trigger (armed mode)".to_string()
    } else {
        format!("last {} min (armed mode)", cfg.buffer_min)
    };
    let retention = if cfg.retention_days == 0 {
        "keep audio forever".to_string()
    } else {
        format!("delete audio after {} days (transcripts kept)", cfg.retention_days)
    };
    let speakers = if cfg.speakers == 0 { "auto".into() } else { cfg.speakers.to_string() };
    let start = if cfg.mode == "armed" { "[ Arm replay buffer ]" } else { "[ Start recording ]" };
    let missing = crate::doctor::missing_essential(&app.reqs);
    let requirements = if missing == 0 {
        "all good ✓  (enter to view)".to_string()
    } else {
        format!("{missing} missing — enter to set up")
    };
    let whisper_missing = app.reqs.iter().any(|r| {
        r.key == crate::doctor::ReqKey::Whispermlx && r.status == crate::doctor::ReqStatus::Missing
    });
    let model_value = if whisper_missing {
        "missing — set up in Requirements below".to_string()
    } else {
        cfg.model.clone()
    };
    let captions_value = if whisper_missing && cfg.live_captions {
        "on — but whispermlx is missing (see Requirements)".to_string()
    } else {
        on_off(cfg.live_captions, "on — rough captions while recording", "off")
    };
    vec![
        ("Microphone".into(), app.devices.get(app.dev_idx).cloned().unwrap_or_default()),
        ("Save to".into(), app.out_input.value.clone()),
        ("Name".into(), app.name_input.value.clone()),
        ("Channels".into(), on_off(cfg.channels == 2, "stereo", "mono")),
        ("System audio".into(), on_off(cfg.system_audio, "on — capture calls/apps too (record mode)", "off — mic only")),
        ("Mode".into(), mode.into()),
        ("Buffer".into(), buffer),
        ("Transcribe".into(), on_off(cfg.transcribe, "on", "off")),
        ("Live captions".into(), captions_value),
        ("Whisper model".into(), model_value),
        ("Speakers".into(), speakers),
        ("Language".into(), cfg.language.clone()),
        ("Theme".into(), cfg.theme.clone()),
        ("Retention".into(), retention),
        ("Requirements".into(), requirements),
        (String::new(), start.into()),
    ]
}

fn draw_setup(f: &mut Frame, app: &mut App, body: Rect) {
    let th = app.theme;
    let rows = setup_rows(app);
    let mut lines: Vec<Line> = Vec::new();
    for (i, (lab, val)) in rows.iter().enumerate() {
        let focused = i == app.cursor;
        let cursor = if focused { Span::styled("> ", focus(&th)) } else { Span::raw("  ") };
        let mut vstyle = if focused { focus(&th) } else { value(&th) };
        if i == F_BUFFER && app.cfg.mode != "armed" && !focused {
            vstyle = dim(&th);
        }
        if i == crate::app::F_SETUP && !focused {
            vstyle = if crate::doctor::missing_essential(&app.reqs) == 0 { ok(&th) } else { warn(&th) };
        }
        let whisper_gone = app.reqs.iter().any(|r| {
            r.key == crate::doctor::ReqKey::Whispermlx && r.status == crate::doctor::ReqStatus::Missing
        });
        if whisper_gone
            && !focused
            && (i == crate::app::F_MODEL || (i == crate::app::F_CAPTIONS && app.cfg.live_captions))
        {
            vstyle = warn(&th);
        }
        let mut val = val.clone();
        if focused && app.editing {
            val.push('▏');
        }
        if lab.is_empty() {
            lines.push(Line::from(vec![cursor, Span::styled(val, vstyle)]));
        } else {
            lines.push(Line::from(vec![
                cursor,
                Span::styled(format!("{lab:<14}"), label(&th)),
                Span::raw("  "),
                Span::styled(val, vstyle),
            ]));
        }
    }
    let panel_h = (rows.len() + 2) as u16;
    let panel = Rect { height: panel_h.min(body.height), ..body };
    f.render_widget(Paragraph::new(lines).block(panel_block(&th)), panel);
    let mut y = panel.y + panel.height;

    if app.cursor == F_THEME && body.height > panel.height + 9 {
        let pv = Rect { x: body.x, y, width: body.width.min(60), height: 9 };
        draw_theme_preview(f, app, pv);
        y += 9;
    }

    let mut notes: Vec<Line> = Vec::new();
    if !app.setup_note.is_empty() {
        notes.push(Line::from(Span::styled(app.setup_note.clone(), dim(&th))));
    }
    if let Some(o) = app.orphans.first() {
        notes.push(Line::from(Span::styled(
            format!(
                "unfinished session found (~{}, {} segment(s)) — r recover · d discard",
                fmt_dur(o.est_duration()),
                o.segments.len()
            ),
            warn(&th),
        )));
    }
    if app.cfg.transcribe && !crate::config::hf_token_present() {
        notes.push(Line::from(Span::styled(
            "no Hugging Face token (~/.cache/huggingface/token) — transcription will fail",
            warn(&th),
        )));
    }
    if !app.setup_err.is_empty() {
        notes.push(Line::from(Span::styled(app.setup_err.clone(), err(&th))));
    }
    notes.push(Line::default());
    notes.push(key_hints(&th, &[
        ("↑↓", "move"),
        ("←→", "change"),
        ("enter", "edit/start"),
        ("b", "library"),
        ("q", "quit"),
    ]));
    let rest = Rect { x: body.x, y, width: body.width, height: body.height.saturating_sub(y - body.y) };
    f.render_widget(Paragraph::new(notes), rest);
}

fn draw_theme_preview(f: &mut Frame, app: &App, area: Rect) {
    let th = app.theme;
    f.render_widget(panel_block(&th), area);
    let inner = Rect { x: area.x + 2, y: area.y + 1, width: area.width.saturating_sub(4), height: area.height.saturating_sub(2) };
    if inner.height < 6 || inner.width < 20 {
        return;
    }
    // synthetic waveform with a clip burst
    let n = inner.width as usize * 2;
    let mut cols: Vec<WaveCol> = (0..n)
        .map(|i| {
            let x = i as f64;
            let p = 0.15 + 0.85 * (x / 6.5).sin().abs() * (x / 23.0).sin().abs();
            WaveCol { peak: p, rms: p * 0.55, ..Default::default() }
        })
        .collect();
    for c in cols.iter_mut().skip(n * 3 / 4).take(3) {
        c.clip = true;
    }
    let wave_area = Rect { height: 4, ..inner };
    render_wave(f.buffer_mut(), wave_area, &cols, &th, None, &[]);
    let vu = vu_line(inner.width.saturating_sub(9) as usize, 0.62, 0.8, &th);
    f.render_widget(Paragraph::new(vu), Rect { y: inner.y + 5, height: 1, ..inner });
    let mut sw: Vec<Span> = [th.lavender, th.blue, th.sapphire, th.green, th.yellow, th.red, th.mauve, th.text, th.overlay0]
        .iter()
        .map(|c| Span::styled("● ", Style::default().fg(*c)))
        .collect();
    sw.push(Span::raw("  "));
    sw.push(Span::styled(format!(" {} ", th.name), Style::default().fg(th.text).bg(th.base)));
    f.render_widget(Paragraph::new(Line::from(sw)), Rect { y: inner.y + 6, height: 1, ..inner });
}

// ----------------------------------------------------------------------
// requirements wizard

fn draw_doctor(f: &mut Frame, app: &App, body: Rect) {
    use crate::doctor::{hf_token_steps, ReqKey, ReqStatus};
    let th = app.theme;
    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::from(vec![
        Span::styled("requirements", value(&th)),
        Span::styled("  everything soundbooth needs, fixed from here", dim(&th)),
    ]));
    lines.push(Line::default());

    for (i, r) in app.reqs.iter().enumerate() {
        let focused = i == app.req_cursor;
        let cursor = if focused { Span::styled("> ", focus(&th)) } else { Span::raw("  ") };
        let installing = app.installer.as_ref().map(|x| x.target == r.key).unwrap_or(false);
        let icon = if installing || r.status == ReqStatus::Checking {
            Span::styled(spinner(app), Style::default().fg(th.blue))
        } else if r.status == ReqStatus::Ok {
            Span::styled("✓", ok(&th))
        } else {
            Span::styled("✗", err(&th))
        };
        let lstyle = if focused { focus(&th) } else { value(&th) };
        let mut spans = vec![cursor, icon, Span::raw(" "), Span::styled(format!("{:<36}", r.key.label()), lstyle)];
        if installing {
            spans.push(Span::styled("installing…", warn(&th)));
        } else if !r.detail.is_empty() {
            spans.push(Span::styled(r.detail.clone(), dim(&th)));
        }
        lines.push(Line::from(spans));
        if focused && r.status == ReqStatus::Missing && !installing {
            lines.push(Line::from(Span::styled(format!("      {}", r.key.action_hint()), warn(&th))));
            if r.key == ReqKey::HfToken {
                for step in hf_token_steps() {
                    lines.push(Line::from(Span::styled(format!("      {step}"), dim(&th))));
                }
            }
        }
    }

    if let Some(e) = &app.install_err {
        lines.push(Line::default());
        lines.push(Line::from(Span::styled(format!("install failed: {e}"), err(&th))));
    }
    if app.installer.is_some() && !app.install_log.is_empty() {
        lines.push(Line::default());
        let keep = app.install_log.len().saturating_sub(8);
        for l in &app.install_log[keep..] {
            let mut l = l.clone();
            if l.chars().count() > 110 {
                l = l.chars().take(110).collect::<String>() + "…";
            }
            lines.push(Line::from(Span::styled(format!("  {l}"), dim(&th))));
        }
    }

    lines.push(Line::default());
    lines.push(key_hints(&th, &[
        ("↑↓", "move"),
        ("enter", "fix selected"),
        ("r", "re-check all"),
        ("esc", "back"),
    ]));
    f.render_widget(Paragraph::new(lines).block(panel_block(&th)), body);
}

// ----------------------------------------------------------------------
// live (armed + recording)

fn draw_live(f: &mut Frame, app: &mut App, body: Rect) {
    let th = app.theme;
    let w_cells = (body.width.saturating_sub(4)).max(20) as usize;

    let caption_rows: u16 = if app.captioner.is_some() { 6 } else { 0 };
    let mut wave_h = body.height.saturating_sub(9 + caption_rows);
    wave_h = wave_h.clamp(5, 14);

    let z = crate::app::ZOOM_LEVELS[app.zoom_idx];
    let cell_ms = z * 2000 / crate::audio::TICK_HZ as usize;
    let cols = downsample(&app.wave, z);

    let now = app.live_elapsed();
    let marker_cells: Vec<usize> = app
        .markers
        .iter()
        .filter_map(|m| now.checked_sub(*m))
        .map(|back| back.as_millis() as usize / cell_ms.max(1))
        .filter(|&c| c < w_cells)
        .collect();

    // badge + detail live in the panel's frame; the elapsed time renders
    // bold and the REC dot pulses
    let (badge, mut badge_style, strong, detail): (String, Style, String, String) =
        if app.screen == Screen::Armed && app.spool.is_none() {
            ("● STANDBY".into(), warn(&th), String::new(),
                "no buffer — nothing is recorded until you press enter".into())
        } else if app.screen == Screen::Armed {
            let sp = app.spool.as_ref().unwrap();
            ("● ARMED".into(), warn(&th), fmt_dur(sp.buffered()),
                format!("of last {} min buffered — nothing kept unless you save", app.cfg.buffer_min))
        } else if app.spool.is_some() {
            ("● REC".into(), err(&th),
                fmt_dur(app.mark.map(|m| m.elapsed()).unwrap_or_default()),
                format!(
                    "+ the {} min before the trigger · {}",
                    app.cfg.buffer_min,
                    app.file.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()
                ))
        } else {
            let paused = app.capture.as_ref().map(|c| c.paused()).unwrap_or(false);
            let size = app.capture.as_ref().map(|c| c.file_size()).unwrap_or(0);
            let name = app.file.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
            let elapsed = fmt_dur(app.capture.as_ref().map(|c| c.elapsed()).unwrap_or_default());
            if paused {
                ("● PAUSED".into(), warn(&th), elapsed, format!("recorded · {name} · {}", human_size(size)))
            } else {
                ("● REC".into(), err(&th), elapsed, format!("· {name} · {}", human_size(size)))
            }
        };
    // slow pulse on the live REC dot (~1 s cycle)
    if badge == "● REC" && (app.frame / 20) % 2 == 1 {
        badge_style = Style::default().fg(crate::theme::mix(th.base, th.red, 0.45));
    }

    // panel: wave + 2 ruler rows, framed with a level-reactive border
    let panel_h = wave_h + 2 + 2;
    let panel = Rect { height: panel_h.min(body.height), ..body };
    let border = if app.clip_ticks > 0 {
        th.red
    } else {
        crate::theme::mix(th.surface0, th.blue, app.vu_level * 0.6)
    };
    let block = panel_block(&th)
        .border_style(Style::default().fg(border))
        .title(Line::from(vec![
            Span::styled(format!(" {badge} "), badge_style.add_modifier(Modifier::BOLD)),
            Span::styled(format!("{strong} "), value(&th).add_modifier(Modifier::BOLD)),
            Span::styled(format!("{detail} "), dim(&th)),
        ]))
        .title_bottom(Line::from(Span::styled(
            format!(" {cell_ms} ms/cell · +/- zoom "),
            dim(&th),
        )).right_aligned());
    let inner = Rect {
        x: panel.x + 2,
        y: panel.y + 1,
        width: panel.width.saturating_sub(4).min(w_cells as u16),
        height: panel.height.saturating_sub(2),
    };
    f.render_widget(block, panel);
    let wave_area = Rect { height: wave_h.min(inner.height), ..inner };
    // markers as full-height mauve hairlines through the wave
    let marks: Vec<u16> = marker_cells
        .iter()
        .filter(|&&b| (b as u16) < inner.width)
        .map(|&b| inner.width - 1 - b as u16)
        .collect();
    let stereo = app.cfg.channels == 2 || app.sys.is_some();
    if stereo {
        render_wave_stereo(f.buffer_mut(), wave_area, &cols, &th, None, &marks);
    } else {
        render_wave(f.buffer_mut(), wave_area, &cols, &th, None, &marks);
    }
    // lane labels
    if stereo && wave_area.height >= 6 {
        let (top_lab, bot_lab) = if app.sys.is_some() { ("mic ▲", "system ▼") } else { ("L", "R") };
        let lane = wave_area.height / 2;
        let style = Style::default().fg(th.overlay1);
        f.buffer_mut().set_string(wave_area.x + 1, wave_area.y, top_lab, style);
        f.buffer_mut().set_string(wave_area.x + 1, wave_area.y + lane, bot_lab, style);
    }
    let [marks, labels] = ruler_lines(inner.width as usize, cell_ms, &marker_cells, &th);
    f.render_widget(
        Paragraph::new(vec![marks, labels]),
        Rect { y: inner.y + wave_h, height: 2.min(inner.height.saturating_sub(wave_h)), ..inner },
    );

    // level meter + advice + captions + hints below the panel
    let mut lines: Vec<Line> = Vec::new();
    let (level_style, advice) = if app.clip_ticks > 0 {
        (err(&th), format!("CLIPPING ({}) — gain down", app.clips))
    } else if app.rmaxdb > -5.0 {
        (warn(&th), "hot — nudge gain down".into())
    } else if app.rmaxdb < -30.0 {
        (warn(&th), "quiet — gain up?".into())
    } else {
        (ok(&th), "level OK".into())
    };
    let hold = ((app.rmaxdb - DB_FLOOR) / -DB_FLOOR).clamp(0.0, 1.0);
    let vu_w = (w_cells / 2).clamp(16, 48);
    let mut vu = vu_line(vu_w, app.vu_level, hold, &th);
    vu.spans.push(Span::raw("  "));
    vu.spans.push(Span::styled(advice, level_style));
    lines.push(vu);

    if app.captioner.is_some() {
        let width = (body.width as usize).saturating_sub(10).max(30);
        let mut cap_lines: Vec<String> = Vec::new();
        for c in &app.captions {
            cap_lines.extend(wrap_text(c, width));
        }
        let keep = cap_lines.len().saturating_sub(6);
        let cap_lines = &cap_lines[keep..];
        if cap_lines.is_empty() {
            lines.push(Line::from(vec![
                Span::styled("live  ", label(&th)),
                Span::styled("(captions warming up…)", dim(&th)),
            ]));
        } else {
            for (i, cl) in cap_lines.iter().enumerate() {
                let lab = if i == 0 { "live  " } else { "      " };
                let style = if i + 1 == cap_lines.len() { value(&th) } else { label(&th) };
                lines.push(Line::from(vec![Span::styled(lab, label(&th)), Span::styled(cl.clone(), style)]));
            }
        }
    }

    if let Some(free) = app.disk_free {
        if free < 2 << 30 && app.screen == Screen::Recording {
            let hours = free as f64 / (50.0 * 1024.0 * app.cfg.channels as f64) / 3600.0;
            lines.push(Line::from(Span::styled(
                format!("low disk: {} free (~{hours:.1} h of audio)", human_size(free)),
                warn(&th),
            )));
        }
    }
    if !app.notice.is_empty() {
        lines.push(Line::from(Span::styled(app.notice.clone(), warn(&th))));
    }
    if app.show_perf {
        let fps = if app.perf_avg_ms > 0.0 { 1000.0 / app.perf_avg_ms } else { 0.0 };
        lines.push(Line::from(Span::styled(
            format!(
                "perf: {fps:.1} fps · frame avg {:.1} ms · max {:.0} ms · ticks/frame {:.2} · draw {:.2} ms",
                app.perf_avg_ms, app.perf_max_ms, app.perf_ticks, app.perf_draw_ms
            ),
            dim(&th),
        )));
    }
    lines.push(Line::default());

    let marker_hint = format!("marker ({})", app.markers.len());
    let stop_hint = if app.cfg.transcribe { "stop + transcribe" } else { "stop" };
    let hints: Vec<(&str, &str)> = if app.screen == Screen::Armed && app.spool.is_none() {
        vec![("enter", "start recording"), ("x", "back to setup"), ("ctrl+c", "quit")]
    } else if app.screen == Screen::Armed {
        vec![("enter", "save buffer + keep recording"), ("x", "disarm (discard)"), ("ctrl+c", "quit")]
    } else if app.sys.is_some() {
        vec![("m", &marker_hint), ("+/-", "zoom"), ("enter", stop_hint), ("x", "skip transcribe")]
    } else {
        vec![("p", "pause"), ("m", &marker_hint), ("+/-", "zoom"), ("enter", stop_hint), ("x", "skip transcribe")]
    };
    lines.push(key_hints(&th, &hints));

    let rest = Rect {
        x: body.x,
        y: panel.y + panel.height,
        width: body.width,
        height: body.height.saturating_sub(panel.height),
    };
    f.render_widget(Paragraph::new(lines), rest);
}

// ----------------------------------------------------------------------
// transcribing

fn draw_transcribing(f: &mut Frame, app: &App, body: Rect) {
    let th = app.theme;
    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::from(vec![
        Span::styled(spinner(app), Style::default().fg(th.blue)),
        Span::raw(" "),
        Span::styled(
            format!("transcribing {}", app.file.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()),
            value(&th),
        ),
        Span::styled(
            format!("  {} · {} · Apple GPU", app.cfg.model, fmt_dur(app.trans_start.elapsed())),
            dim(&th),
        ),
    ]));
    lines.push(Line::default());

    if app.show_log {
        let max = body.height.saturating_sub(6) as usize;
        let keep = app.trans_log.len().saturating_sub(max);
        for l in &app.trans_log[keep..] {
            lines.push(Line::from(Span::styled(l.clone(), dim(&th))));
        }
        lines.push(Line::default());
        lines.push(key_hints(&th, &[("l", "hide log"), ("ctrl+c", "abort")]));
        f.render_widget(Paragraph::new(lines), body);
        return;
    }

    for (i, name) in STAGE_NAMES.iter().enumerate() {
        let mut spans = vec![Span::raw("  ")];
        if i < app.stage {
            spans.push(Span::styled("✓ ", ok(&th)));
            spans.push(Span::styled(*name, dim(&th)));
        } else if i == app.stage {
            spans.push(Span::styled(spinner(app), Style::default().fg(th.blue)));
            spans.push(Span::raw(" "));
            spans.push(Span::styled(*name, focus(&th)));
            if i == 2 && app.stage_pct > 0.0 {
                spans.push(Span::raw("  "));
                spans.extend(progress_spans(24, app.stage_pct, &th));
                spans.push(Span::styled(format!(" {:3.0}%", app.stage_pct * 100.0), dim(&th)));
            }
        } else {
            spans.push(Span::styled(format!("· {name}"), dim(&th)));
        }
        lines.push(Line::from(spans));
    }
    if !app.live_segs.is_empty() {
        lines.push(Line::default());
        lines.push(Line::from(Span::styled("  hearing:", label(&th))));
        for s in &app.live_segs {
            let mut s = s.clone();
            if s.chars().count() > 90 {
                s = s.chars().take(90).collect::<String>() + "…";
            }
            lines.push(Line::from(Span::styled(format!("    {s}"), dim(&th))));
        }
    }
    lines.push(Line::default());
    lines.push(key_hints(&th, &[("l", "show log"), ("ctrl+c", "abort")]));
    f.render_widget(Paragraph::new(lines), body);
}

// ----------------------------------------------------------------------
// speakers

fn draw_speakers(f: &mut Frame, app: &App, body: Rect) {
    let th = app.theme;
    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::from(vec![
        Span::styled("who was talking?", value(&th)),
        Span::styled("  assign names — they land in the transcript", dim(&th)),
    ]));
    lines.push(Line::default());
    for (i, s) in app.stats.iter().enumerate() {
        let focused = i == app.spk_cursor;
        let cursor = if focused { Span::styled("> ", focus(&th)) } else { Span::raw("  ") };
        let name_span = if focused && app.spk_edit {
            Span::styled(format!("{}▏", app.spk_input.value), value(&th))
        } else if s.name.is_empty() {
            Span::styled(format!("Speaker {}", i + 1), dim(&th))
        } else {
            Span::styled(s.name.clone(), value(&th))
        };
        let mut spans = vec![cursor, Span::styled(format!("{:<10}", s.id), label(&th)), Span::raw("  ")];
        spans.extend(progress_spans(20, s.share, &th));
        spans.push(Span::styled(format!(" {:3.0}%  {}  ", s.share * 100.0, fmt_clock(s.dur)), dim(&th)));
        spans.push(name_span);
        lines.push(Line::from(spans));
        let mut quote = s.quote.clone();
        if quote.chars().count() > 84 {
            quote = quote.chars().take(84).collect::<String>() + "…";
        }
        lines.push(Line::from(Span::styled(format!("  “{quote}”"), dim(&th))));
    }
    lines.push(Line::default());
    lines.push(key_hints(&th, &[("↑↓", "move"), ("enter", "name"), ("c", "continue")]));
    f.render_widget(Paragraph::new(lines), body);
}

// ----------------------------------------------------------------------
// library

fn lib_name(e: &LibEntry) -> String {
    e.path.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()
}

fn draw_library(f: &mut Frame, app: &App, body: Rect) {
    let th = app.theme;
    let mut lines: Vec<Line> = Vec::new();

    if app.lib_searching {
        lines.push(Line::from(vec![
            Span::styled("library", value(&th)),
            Span::styled(format!("  {}", app.cfg.out_dir), dim(&th)),
        ]));
        lines.push(Line::default());
        lines.push(Line::from(Span::styled(format!("/ {}▏", app.search_input.value), value(&th))));
        lines.push(Line::default());
        lines.push(key_hints(&th, &[("enter", "search"), ("esc", "cancel")]));
        f.render_widget(Paragraph::new(lines), body);
        return;
    }

    if app.lib_importing {
        lines.push(Line::from(vec![
            Span::styled("import a recording", value(&th)),
            Span::styled("  m4a, mp3, wav, aiff… converted to FLAC and transcribed", dim(&th)),
        ]));
        lines.push(Line::default());
        lines.push(Line::from(Span::styled(
            format!("path: {}▏", app.import_input.value),
            value(&th),
        )));
        lines.push(Line::from(Span::styled(
            "      tip: drag the file from Finder onto this window to paste its path",
            dim(&th),
        )));
        lines.push(Line::default());
        lines.push(key_hints(&th, &[("enter", "import"), ("esc", "cancel")]));
        f.render_widget(Paragraph::new(lines), body);
        return;
    }

    if app.show_hits {
        lines.push(Line::from(vec![
            Span::styled("library", value(&th)),
            Span::styled(
                format!("  ·  {} hit(s) for “{}”", app.hits.len(), app.search_input.value),
                dim(&th),
            ),
        ]));
        lines.push(Line::default());
        if app.hits.is_empty() {
            lines.push(Line::from(Span::styled("no matches", dim(&th))));
        }
        let max = body.height.saturating_sub(5) as usize;
        let start = app.hit_cursor.saturating_sub(max.saturating_sub(1));
        for (i, h) in app.hits.iter().enumerate().skip(start).take(max) {
            let focused = i == app.hit_cursor;
            let cursor = if focused { Span::styled("> ", focus(&th)) } else { Span::raw("  ") };
            let style = if focused { focus(&th) } else { value(&th) };
            lines.push(Line::from(vec![
                cursor,
                Span::styled(
                    format!("{:<40}", h.audio.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()),
                    style,
                ),
                Span::raw("  "),
                Span::styled(h.snippet.clone(), dim(&th)),
            ]));
        }
        lines.push(Line::default());
        lines.push(key_hints(&th, &[("enter", "open"), ("/", "new search"), ("esc", "back")]));
        f.render_widget(Paragraph::new(lines), body);
        return;
    }

    lines.push(Line::from(vec![
        Span::styled("library", value(&th)),
        Span::styled(format!("  {}", app.cfg.out_dir), dim(&th)),
    ]));
    lines.push(Line::default());
    if app.lib.is_empty() {
        lines.push(Line::from(Span::styled("no recordings yet", dim(&th))));
    }
    let max = body.height.saturating_sub(6) as usize;
    let start = app.lib_cursor.saturating_sub(max.saturating_sub(1));
    for (i, e) in app.lib.iter().enumerate().skip(start).take(max) {
        let focused = i == app.lib_cursor;
        let cursor = if focused { Span::styled("> ", focus(&th)) } else { Span::raw("  ") };
        let style = if focused { focus(&th) } else { value(&th) };
        let tx = if e.has_tx { Span::styled("✓ ", ok(&th)) } else { Span::styled("· ", dim(&th)) };
        let when: chrono::DateTime<chrono::Local> = e.modified.into();
        lines.push(Line::from(vec![
            cursor,
            tx,
            Span::styled(format!("{:<44}", lib_name(e)), style),
            Span::styled(format!("{}  {}", when.format("%d/%m/%Y %H:%M"), human_size(e.size)), dim(&th)),
        ]));
    }
    if app.lib_confirm {
        if let Some(e) = app.lib.get(app.lib_cursor) {
            lines.push(Line::from(Span::styled(
                format!("delete {} and its transcript? y / any other key cancels", lib_name(e)),
                err(&th),
            )));
        }
    }
    if !app.lib_notice.is_empty() {
        let style = if app.lib_notice.contains("failed") { err(&th) } else { warn(&th) };
        lines.push(Line::from(Span::styled(app.lib_notice.clone(), style)));
    } else if app.import_rx.is_some() {
        lines.push(Line::from(vec![
            Span::styled(spinner(app), Style::default().fg(th.blue)),
            Span::styled(" importing…", dim(&th)),
        ]));
    }
    lines.push(Line::default());
    lines.push(key_hints(&th, &[("enter", "open"), ("/", "search"), ("i", "import"), ("d", "delete"), ("o", "folder"), ("esc", "back")]));
    f.render_widget(Paragraph::new(lines), body);
}

// ----------------------------------------------------------------------
// done

fn draw_done(f: &mut Frame, app: &mut App, body: Rect) {
    let th = app.theme;
    let mut lines: Vec<Line> = Vec::new();
    lines.push(Line::from(vec![
        Span::styled("recording saved  ", ok(&th)),
        Span::styled(app.file.display().to_string(), value(&th)),
    ]));
    if !app.notice.is_empty() {
        lines.push(Line::from(Span::styled(app.notice.clone(), warn(&th))));
    }

    // player strip: rendered separately after we know the panel rect
    let player_rows: u16 = if app.p_ready { 9 } else if app.decode_rx.is_some() { 1 } else { 0 };

    if app.did_trans && !app.segs.is_empty() {
        let dur = app.segs.iter().map(|s| s.end).fold(0.0, f64::max);
        let words: usize = app.segs.iter().map(|s| s.text.split_whitespace().count()).sum();
        lines.push(Line::from(Span::styled(
            format!(
                "{} of speech · {words} words · {} marker(s) · {} clip(s)",
                fmt_clock(dur),
                app.markers.len(),
                app.clips
            ),
            dim(&th),
        )));
        if !app.stats.is_empty() {
            lines.push(Line::default());
            lines.push(Line::from(Span::styled("talk time", label(&th))));
            for s in &app.stats {
                let mut spans = vec![Span::raw("  ")];
                spans.extend(progress_spans(22, s.share, &th));
                spans.push(Span::styled(format!(" {:3.0}%  ", s.share * 100.0), dim(&th)));
                spans.push(Span::styled(display_name(&app.stats, &s.id), value(&th)));
                lines.push(Line::from(spans));
            }
        }
    }

    if app.did_trans && app.segs.is_empty() {
        // transcription ran but heard nothing: the notice says so
    } else if app.did_trans {
        if let Some(dir) = &app.tx_dir {
            lines.push(Line::default());
            lines.push(Line::from(vec![
                Span::styled("transcript in    ", ok(&th)),
                Span::styled(dir.display().to_string(), value(&th)),
            ]));
        }
        if let Some(md) = &app.transcript_md {
            lines.push(Line::from(Span::styled(
                format!("named transcript: {}", md.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()),
                dim(&th),
            )));
        }
        if !app.preview.is_empty() {
            lines.push(Line::default());
            for p in &app.preview {
                let mut p = p.clone();
                if p.chars().count() > 100 {
                    p = p.chars().take(100).collect::<String>() + "…";
                }
                lines.push(Line::from(Span::styled(format!("  {p}"), dim(&th))));
            }
        }
    } else if let Some(e) = &app.trans_err {
        lines.push(Line::from(Span::styled(format!("transcription failed: {e}"), err(&th))));
        lines.push(Line::from(Span::styled("the recording is safe — press t to retry", dim(&th))));
    }
    if let Some(mk) = &app.markers_file {
        lines.push(Line::from(Span::styled(
            format!("markers: {}", mk.file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default()),
            dim(&th),
        )));
    }
    if !app.post_status.is_empty() {
        let style = if app.post_status.contains("failed") { err(&th) } else { dim(&th) };
        lines.push(Line::from(Span::styled(app.post_status.clone(), style)));
    }

    let text_h = (lines.len() + 2) as u16;
    let panel = Rect { height: text_h.min(body.height), ..body };
    f.render_widget(Paragraph::new(lines).block(panel_block(&th)), panel);
    let mut y = panel.y + panel.height;

    if app.p_ready && !app.p_wave.is_empty() && app.p_dur > 0.0 && body.height > y - body.y + player_rows {
        let w_cells = ((app.p_wave.len() / 2) as u16).min(body.width.saturating_sub(4));
        let cur = app.cur_pos();
        let head = ((cur / app.p_dur * w_cells as f64) as u16).min(w_cells.saturating_sub(1));
        let wave_area = Rect { x: body.x + 2, y: y + 1, width: w_cells, height: 7 };
        let marks: Vec<u16> = app
            .markers
            .iter()
            .map(|m| ((m.as_secs_f64() / app.p_dur) * w_cells as f64) as u16)
            .filter(|&c| c < w_cells)
            .collect();
        render_wave(f.buffer_mut(), wave_area, &app.p_wave, &th, Some(head), &marks);
        let state = if app.playback.is_some() { "▶" } else { "⏸" };
        let mut transport = format!("{state} {} / {}", fmt_clock(cur), fmt_clock(app.p_dur));
        if !app.markers.is_empty() {
            transport += &format!("  ·  {} marker(s), [ ] to jump", app.markers.len());
        }
        f.render_widget(
            Paragraph::new(Line::from(Span::styled(transport, dim(&th)))),
            Rect { x: body.x + 2, y: y + 8, width: body.width.saturating_sub(4), height: 1 },
        );
        y += player_rows;
    } else if app.decode_rx.is_some() {
        f.render_widget(
            Paragraph::new(Line::from(Span::styled("decoding waveform…", dim(&th)))),
            Rect { x: body.x, y, width: body.width, height: 1 },
        );
        y += 1;
    }

    let play_hint = if app.playback.is_some() { "pause" } else { "play" };
    let mut hints: Vec<(&str, &str)> = Vec::new();
    if !app.did_trans || app.segs.is_empty() {
        hints.push(("t", "transcribe"));
    }
    if app.did_trans && !app.stats.is_empty() {
        hints.push(("s", "speakers"));
    }
    hints.push(("p", play_hint));
    hints.push(("←→", "±5s"));
    hints.push(("n", "new recording"));
    hints.push(("o", "open folder"));
    if app.from_lib {
        hints.push(("esc", "library"));
    }
    hints.push(("q", "quit"));
    let rest = Rect { x: body.x, y: y + 1, width: body.width, height: body.height.saturating_sub(y + 1 - body.y) };
    f.render_widget(Paragraph::new(key_hints(&th, &hints)), rest);
}
