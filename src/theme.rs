use ratatui::style::Color;
use std::collections::HashMap;

/// A Catppuccin-style palette. Colours are ratatui RGB values.
#[derive(Clone, Copy)]
pub struct Theme {
    pub name: &'static str,
    pub surface0: Color,
    pub overlay0: Color,
    pub overlay1: Color,
    pub subtext0: Color,
    pub text: Color,
    pub blue: Color,
    pub lavender: Color,
    pub sapphire: Color,
    pub green: Color,
    pub yellow: Color,
    pub red: Color,
    pub mauve: Color,
    pub base: Color,
}

const fn rgb(hex: u32) -> Color {
    Color::Rgb((hex >> 16) as u8, (hex >> 8) as u8, hex as u8)
}

pub const THEMES: [Theme; 4] = [
    Theme {
        name: "mocha",
        base: rgb(0x1e1e2e), surface0: rgb(0x313244),
        overlay0: rgb(0x6c7086), overlay1: rgb(0x7f849c), subtext0: rgb(0xa6adc8),
        text: rgb(0xcdd6f4), blue: rgb(0x89b4fa), lavender: rgb(0xb4befe),
        sapphire: rgb(0x74c7ec), green: rgb(0xa6e3a1), yellow: rgb(0xf9e2af),
        red: rgb(0xf38ba8), mauve: rgb(0xcba6f7),
    },
    Theme {
        name: "macchiato",
        base: rgb(0x24273a), surface0: rgb(0x363a4f),
        overlay0: rgb(0x6e738d), overlay1: rgb(0x8087a2), subtext0: rgb(0xa5adcb),
        text: rgb(0xcad3f5), blue: rgb(0x8aadf4), lavender: rgb(0xb7bdf8),
        sapphire: rgb(0x7dc4e4), green: rgb(0xa6da95), yellow: rgb(0xeed49f),
        red: rgb(0xed8796), mauve: rgb(0xc6a0f6),
    },
    Theme {
        name: "frappe",
        base: rgb(0x303446), surface0: rgb(0x414559),
        overlay0: rgb(0x737994), overlay1: rgb(0x838ba7), subtext0: rgb(0xa5adce),
        text: rgb(0xc6d0f5), blue: rgb(0x8caaee), lavender: rgb(0xbabbf1),
        sapphire: rgb(0x85c1dc), green: rgb(0xa6d189), yellow: rgb(0xe5c890),
        red: rgb(0xe78284), mauve: rgb(0xca9ee6),
    },
    Theme {
        name: "latte",
        base: rgb(0xeff1f5), surface0: rgb(0xccd0da),
        overlay0: rgb(0x9ca0b0), overlay1: rgb(0x8c8fa1), subtext0: rgb(0x6c6f85),
        text: rgb(0x4c4f69), blue: rgb(0x1e66f5), lavender: rgb(0x7287fd),
        sapphire: rgb(0x209fb5), green: rgb(0x40a02b), yellow: rgb(0xdf8e1d),
        red: rgb(0xd20f39), mauve: rgb(0x8839ef),
    },
];

pub fn by_name(name: &str) -> Theme {
    THEMES.iter().find(|t| t.name == name).copied().unwrap_or(THEMES[0])
}

pub fn theme_names() -> Vec<&'static str> {
    THEMES.iter().map(|t| t.name).collect()
}

fn parse_hex(s: &str) -> Option<Color> {
    let s = s.trim_start_matches('#');
    if s.len() != 6 {
        return None;
    }
    let v = u32::from_str_radix(s, 16).ok()?;
    Some(rgb(v))
}

/// Per-colour overrides from the config, e.g. {"blue": "#0000ff"}.
pub fn apply_overrides(mut t: Theme, overrides: &HashMap<String, String>) -> Theme {
    for (k, v) in overrides {
        let Some(c) = parse_hex(v) else { continue };
        match k.as_str() {
            "base" => t.base = c,
            "surface0" => t.surface0 = c,
            "overlay0" => t.overlay0 = c,
            "overlay1" => t.overlay1 = c,
            "subtext0" => t.subtext0 = c,
            "text" => t.text = c,
            "blue" => t.blue = c,
            "lavender" => t.lavender = c,
            "sapphire" => t.sapphire = c,
            "green" => t.green = c,
            "yellow" => t.yellow = c,
            "red" => t.red = c,
            "mauve" => t.mauve = c,
            _ => {}
        }
    }
    t
}

fn channels(c: Color) -> (f64, f64, f64) {
    match c {
        Color::Rgb(r, g, b) => (r as f64, g as f64, b as f64),
        _ => (205.0, 214.0, 244.0),
    }
}

pub fn mix(a: Color, b: Color, t: f64) -> Color {
    let t = t.clamp(0.0, 1.0);
    let (ar, ag, ab) = channels(a);
    let (br, bg, bb) = channels(b);
    Color::Rgb(
        (ar + (br - ar) * t) as u8,
        (ag + (bg - ag) * t) as u8,
        (ab + (bb - ab) * t) as u8,
    )
}

impl Theme {
    /// Vertical waveform gradient: lavender core, blue mids, sapphire tips.
    pub fn wave_ramp(&self, t: f64) -> Color {
        if t < 0.55 {
            mix(self.lavender, self.blue, t / 0.55)
        } else {
            mix(self.blue, self.sapphire, (t - 0.55) / 0.45)
        }
    }

    /// VU bar gradient: green through yellow into red.
    pub fn vu_ramp(&self, t: f64) -> Color {
        if t < 0.76 {
            mix(self.green, self.yellow, t / 0.76)
        } else {
            mix(self.yellow, self.red, (t - 0.76) / 0.24)
        }
    }
}
