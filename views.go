package main

import (
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

func (m model) View() string {
	var body string
	switch m.scr {
	case screenSetup:
		body = m.viewSetup()
	case screenArmed, screenRecording:
		body = m.viewLive()
	case screenTranscribing:
		body = m.viewTranscribing()
	case screenSpeakers:
		body = m.viewSpeakers()
	case screenLibrary:
		body = m.viewLibrary()
	case screenDone:
		body = m.viewDone()
	}
	header := titleStyle.Render("soundbooth") + dimStyle.Render("  record · meter · transcribe")
	return lipgloss.JoinVertical(lipgloss.Left, header, "", body)
}

// --- setup ---

func retentionLabel(days int) string {
	if days == 0 {
		return "keep audio forever"
	}
	return fmt.Sprintf("delete audio after %d days (transcripts kept)", days)
}

func bufferLabel(minutes int) string {
	if minutes == 0 {
		return "off — record from trigger (armed mode)"
	}
	return fmt.Sprintf("last %d min (armed mode)", minutes)
}

func (m model) viewSetup() string {
	speakers := "auto"
	if m.cfg.Speakers > 0 {
		speakers = strconv.Itoa(m.cfg.Speakers)
	}
	onOff := func(b bool, on, off string) string {
		if b {
			return on
		}
		return off
	}
	mode := "record now"
	startLabel := "[ Start recording ]"
	if m.cfg.Mode == "armed" {
		mode = "armed (replay buffer)"
		startLabel = "[ Arm replay buffer ]"
	}
	rows := []struct {
		label, value string
	}{
		{"Microphone", m.devices[m.devIdx].Name},
		{"Save to", m.outInput.View()},
		{"Name", m.nameInput.View()},
		{"Channels", onOff(m.cfg.Channels == 2, "stereo", "mono")},
		{"Mode", mode},
		{"Buffer", bufferLabel(m.cfg.BufferMin)},
		{"Transcribe", onOff(m.cfg.Transcribe, "on", "off")},
		{"Whisper model", m.cfg.Model},
		{"Speakers", speakers},
		{"Language", m.cfg.Language},
		{"Theme", m.cfg.Theme},
		{"Retention", retentionLabel(m.cfg.RetentionDays)},
		{"", startLabel},
	}
	var b strings.Builder
	for i, r := range rows {
		cursor := "  "
		style := valueStyle
		if i == m.cursor {
			cursor = focusStyle.Render("> ")
			style = focusStyle
		}
		if i == fieldBuffer && m.cfg.Mode != "armed" && i != m.cursor {
			style = dimStyle
		}
		if r.label == "" {
			b.WriteString(fmt.Sprintf("%s%s\n", cursor, style.Render(r.value)))
			continue
		}
		b.WriteString(fmt.Sprintf("%s%s  %s\n", cursor,
			labelStyle.Render(fmt.Sprintf("%-14s", r.label)), style.Render(r.value)))
	}
	out := panelStyle.Render(strings.TrimRight(b.String(), "\n"))
	if m.cursor == fieldTheme {
		out = lipgloss.JoinVertical(lipgloss.Left, out, themePreview(m.width))
	}
	var notes []string
	if m.setupNote != "" {
		notes = append(notes, dimStyle.Render(m.setupNote))
	}
	if len(m.orphans) > 0 {
		o := m.orphans[0]
		notes = append(notes, warnStyle.Render(fmt.Sprintf(
			"unfinished session found (~%s, %d segment(s)) — r recover · d discard",
			fmtDur(o.EstDuration()), len(o.Segments))))
	}
	if m.cfg.Transcribe && !hfTokenPresent() {
		notes = append(notes, warnStyle.Render("no Hugging Face token (~/.cache/huggingface/token) — transcription will fail"))
	}
	if m.setupErr != "" {
		notes = append(notes, errStyle.Render(m.setupErr))
	}
	hints := keyHint("↑↓", "move", "←→", "change", "enter", "edit/start", "b", "library", "q", "quit")
	parts := append([]string{out}, notes...)
	parts = append(parts, "", hints)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// themePreview renders a synthetic waveform, VU bar and palette swatches
// so cycling the Theme field shows exactly what each flavour looks like.
func themePreview(width int) string {
	wCells := 46
	if width > 0 && width-10 < wCells {
		wCells = width - 10
	}
	if wCells < 20 {
		wCells = 20
	}
	cols := make([]waveCol, wCells*2)
	for i := range cols {
		x := float64(i)
		p := 0.15 + 0.85*math.Abs(math.Sin(x/6.5))*math.Abs(math.Sin(x/23))
		cols[i] = waveCol{peak: p, rms: p * 0.55}
	}
	// show the clip colour too
	for i := len(cols) * 3 / 4; i < len(cols)*3/4+3 && i < len(cols); i++ {
		cols[i].clip = true
	}
	wave := renderWave(cols, wCells, 4)

	var sw strings.Builder
	for _, hex := range []string{th.Lavender, th.Blue, th.Sapphire, th.Green, th.Yellow, th.Red, th.Mauve, th.Text, th.Overlay0} {
		sw.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(hex)).Render("● "))
	}
	// background chips — the dark flavours differ most in their base tints
	sw.WriteString("  ")
	sw.WriteString(lipgloss.NewStyle().
		Background(lipgloss.Color(th.Base)).Foreground(lipgloss.Color(th.Text)).
		Render(" " + th.Name + " "))
	sw.WriteString(lipgloss.NewStyle().
		Background(lipgloss.Color(th.Surface0)).Foreground(lipgloss.Color(th.Subtext0)).
		Render(" " + th.Base + " "))
	body := wave + "\n" + renderVU(wCells-9, 0.62, 0.8) + "\n" + sw.String()
	return panelStyle.Render(body)
}

// --- live (armed + recording) ---

func (m model) viewLive() string {
	wCells := m.width - 6
	if wCells < 20 {
		wCells = 60
	}
	hCells := m.height - 13
	if hCells > 14 {
		hCells = 14
	}
	if hCells < 5 {
		hCells = 5
	}

	z := zoomLevels[m.zoomIdx]
	cellMs := z * 100
	cols := downsample(m.wave, z)

	var markerCells []int
	if len(m.markers) > 0 {
		now := m.liveElapsed()
		for _, mk := range m.markers {
			back := int(now-mk) / int(time.Duration(cellMs)*time.Millisecond)
			if back >= 0 && back < wCells {
				markerCells = append(markerCells, back)
			}
		}
	}

	var wave string
	if m.cfg.Channels == 2 {
		wave = renderWaveStereo(cols, wCells, hCells)
	} else {
		wave = renderWave(cols, wCells, hCells)
	}
	wave += "\n" + renderRuler(wCells, cellMs, markerCells)

	panel := panelStyle
	if m.clipTicks > 0 {
		panel = panel.BorderForeground(lipgloss.Color(th.Red))
	}

	var levelStyle lipgloss.Style
	var advice string
	switch {
	case m.clipTicks > 0:
		levelStyle, advice = errStyle, fmt.Sprintf("CLIPPING (%d) — gain down", m.clips)
	case m.rmaxdb > -5:
		levelStyle, advice = warnStyle, "hot — nudge gain down"
	case m.rmaxdb < -30:
		levelStyle, advice = warnStyle, "quiet — gain up?"
	default:
		levelStyle, advice = okStyle, "level OK"
	}
	level := levelStyle.Render(fmt.Sprintf("peak %d dB  %s", int(m.rmaxdb), advice))

	var badge, detail, hints string
	markerHint := fmt.Sprintf("marker (%d)", len(m.markers))
	switch {
	case m.scr == screenArmed && m.spool == nil:
		badge = warnStyle.Render("● STANDBY")
		detail = dimStyle.Render("no buffer — nothing is recorded until you press enter")
		hints = keyHint("enter", "start recording", "x", "back to setup", "ctrl+c", "quit")
	case m.scr == screenArmed:
		buffered := time.Since(m.armStart)
		window := time.Duration(m.cfg.BufferMin) * time.Minute
		if buffered > window {
			buffered = window
		}
		badge = warnStyle.Render("● ARMED")
		detail = dimStyle.Render(fmt.Sprintf("buffered %s of last %d min — nothing kept unless you save",
			fmtDur(buffered), m.cfg.BufferMin))
		hints = keyHint("enter", fmt.Sprintf("save last %d min + keep recording", m.cfg.BufferMin), "x", "disarm (discard)", "ctrl+c", "quit")
	case m.spool != nil:
		badge = errStyle.Render("● REC")
		detail = dimStyle.Render(fmt.Sprintf("%s + the %d min before the trigger  ·  %s",
			fmtDur(time.Since(m.mark)), m.cfg.BufferMin, filepath.Base(m.file)))
		hints = keyHint("enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "m", markerHint, "+/-", "zoom", "x", "skip transcribe")
	case m.rec != nil && m.rec.Paused():
		badge = warnStyle.Render("● PAUSED")
		detail = dimStyle.Render(fmt.Sprintf("%s recorded  ·  %s  %s",
			fmtDur(m.rec.Elapsed()), filepath.Base(m.file), humanSize(m.rec.FileSize())))
		hints = keyHint("p", "resume", "enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "skip transcribe")
	default:
		badge = errStyle.Render("● REC")
		detail = dimStyle.Render(fmt.Sprintf("%s  ·  %s  %s",
			fmtDur(m.rec.Elapsed()), filepath.Base(m.file), humanSize(m.rec.FileSize())))
		hints = keyHint("p", "pause", "m", markerHint, "+/-", "zoom", "enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "skip transcribe")
	}

	hold := (m.rmaxdb - dbFloor) / -dbFloor
	if hold < 0 {
		hold = 0
	}
	if hold > 1 {
		hold = 1
	}
	vuWidth := wCells - 30
	if vuWidth > 60 {
		vuWidth = 60
	}
	if vuWidth < 16 {
		vuWidth = 16
	}
	vu := renderVU(vuWidth, m.vuLevel, hold)

	status := badge + "  " + level + "  " + detail
	parts := []string{panel.Render(wave), status, vu}
	if m.diskFree >= 0 && m.diskFree < 2<<30 && m.scr == screenRecording {
		hoursLeft := float64(m.diskFree) / (50 * 1024 * float64(m.cfg.Channels)) / 3600
		parts = append(parts, warnStyle.Render(
			fmt.Sprintf("low disk: %s free (~%.1f h of audio)", humanSize(m.diskFree), hoursLeft)))
	}
	if m.notice != "" {
		parts = append(parts, warnStyle.Render(m.notice))
	}
	parts = append(parts, "", hints)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

func fmtDur(d time.Duration) string {
	d = d.Round(time.Second)
	return fmt.Sprintf("%02d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}

func transcribeSuffix(on bool) string {
	if on {
		return " + transcribe"
	}
	return ""
}

// --- transcribing ---

func (m model) viewTranscribing() string {
	head := fmt.Sprintf("%s %s  %s", m.spin.View(),
		valueStyle.Render("transcribing "+filepath.Base(m.file)),
		dimStyle.Render(fmt.Sprintf("%s · %s · Apple GPU", m.cfg.Model, fmtDur(time.Since(m.transStart)))))

	if m.showLog {
		tail := m.transLog
		maxLines := m.height - 10
		if maxLines < 4 {
			maxLines = 4
		}
		if len(tail) > maxLines {
			tail = tail[len(tail)-maxLines:]
		}
		return lipgloss.JoinVertical(lipgloss.Left, head, "",
			panelStyle.Render(dimStyle.Render(strings.Join(tail, "\n"))), "",
			keyHint("l", "hide log", "ctrl+c", "abort"))
	}

	var b strings.Builder
	for i, name := range stageNames {
		switch {
		case i < m.stage:
			b.WriteString(okStyle.Render("  ✓ ") + dimStyle.Render(name))
		case i == m.stage:
			b.WriteString("  " + m.spin.View() + " " + focusStyle.Render(name))
			if i == 2 && m.stagePct > 0 {
				b.WriteString("  " + renderProgress(24, m.stagePct) +
					dimStyle.Render(fmt.Sprintf(" %3.0f%%", m.stagePct*100)))
			}
		default:
			b.WriteString(dimStyle.Render("  · " + name))
		}
		b.WriteByte('\n')
	}
	body := strings.TrimRight(b.String(), "\n")

	if len(m.liveSegs) > 0 {
		body += "\n\n" + labelStyle.Render("  hearing:")
		for _, s := range m.liveSegs {
			if len(s) > 90 {
				s = s[:90] + "…"
			}
			body += "\n" + dimStyle.Render("    "+s)
		}
	}
	return lipgloss.JoinVertical(lipgloss.Left, head, "",
		panelStyle.Render(body), "",
		keyHint("l", "show log", "ctrl+c", "abort"))
}

// renderProgress draws a gradient progress bar, frac 0..1.
func renderProgress(width int, frac float64) string {
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	fill := int(frac*float64(width) + 0.5)
	var b strings.Builder
	for i := 0; i < width; i++ {
		if i < fill {
			t := float64(i) / float64(width-1)
			b.WriteString(lipgloss.NewStyle().
				Foreground(lipgloss.Color(waveRamp(t))).Render("█"))
		} else {
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(th.Surface0)).Render("░"))
		}
	}
	return b.String()
}

// --- speakers ---

func (m model) viewSpeakers() string {
	head := valueStyle.Render("who was talking?") + dimStyle.Render("  assign names — they land in the transcript")
	var b strings.Builder
	for i, s := range m.stats {
		cursor := "  "
		nameStr := s.Name
		if nameStr == "" {
			nameStr = dimStyle.Render(fmt.Sprintf("Speaker %d", i+1))
		} else {
			nameStr = valueStyle.Render(nameStr)
		}
		if i == m.spkCursor {
			cursor = focusStyle.Render("> ")
			if m.spkEdit {
				nameStr = m.spkInput.View()
			}
		}
		bar := renderProgress(20, s.Share)
		fmt.Fprintf(&b, "%s%s  %s %s  %s\n", cursor,
			labelStyle.Render(fmt.Sprintf("%-10s", s.ID)),
			bar, dimStyle.Render(fmt.Sprintf("%4.0f%%  %s", s.Share*100, fmtClock(s.Dur))),
			nameStr)
		quote := s.Quote
		if len(quote) > 84 {
			quote = quote[:84] + "…"
		}
		fmt.Fprintf(&b, "  %s\n", dimStyle.Render("“"+quote+"”"))
	}
	hints := keyHint("↑↓", "move", "enter", "name", "c", "continue")
	return lipgloss.JoinVertical(lipgloss.Left, head, "",
		panelStyle.Render(strings.TrimRight(b.String(), "\n")), "", hints)
}

// --- library ---

func (m model) viewLibrary() string {
	head := valueStyle.Render("library") + dimStyle.Render("  "+m.cfg.OutDir)

	if m.libSearching {
		return lipgloss.JoinVertical(lipgloss.Left, head, "",
			panelStyle.Render(m.searchInput.View()), "",
			keyHint("enter", "search", "esc", "cancel"))
	}
	if m.showHits {
		var b strings.Builder
		if len(m.hits) == 0 {
			b.WriteString(dimStyle.Render("no matches for “" + m.searchInput.Value() + "”"))
		}
		maxRows := m.height - 10
		if maxRows < 5 {
			maxRows = 5
		}
		start := 0
		if m.hitCursor >= maxRows {
			start = m.hitCursor - maxRows + 1
		}
		for i := start; i < len(m.hits) && i < start+maxRows; i++ {
			h := m.hits[i]
			cursor := "  "
			style := valueStyle
			if i == m.hitCursor {
				cursor = focusStyle.Render("> ")
				style = focusStyle
			}
			fmt.Fprintf(&b, "%s%s  %s\n", cursor,
				style.Render(fmt.Sprintf("%-40s", filepath.Base(h.Audio))),
				dimStyle.Render(h.Snippet))
		}
		return lipgloss.JoinVertical(lipgloss.Left,
			head+dimStyle.Render(fmt.Sprintf("  ·  %d hit(s) for “%s”", len(m.hits), m.searchInput.Value())), "",
			panelStyle.Render(strings.TrimRight(b.String(), "\n")), "",
			keyHint("enter", "open", "/", "new search", "esc", "back"))
	}

	if len(m.lib) == 0 {
		return lipgloss.JoinVertical(lipgloss.Left, head, "",
			panelStyle.Render(dimStyle.Render("no recordings yet")), "",
			keyHint("esc", "back"))
	}
	maxRows := m.height - 10
	if maxRows < 5 {
		maxRows = 5
	}
	start := 0
	if m.libCursor >= maxRows {
		start = m.libCursor - maxRows + 1
	}
	var b strings.Builder
	for i := start; i < len(m.lib) && i < start+maxRows; i++ {
		e := m.lib[i]
		cursor := "  "
		style := valueStyle
		if i == m.libCursor {
			cursor = focusStyle.Render("> ")
			style = focusStyle
		}
		tx := dimStyle.Render("·")
		if e.HasTx {
			tx = okStyle.Render("✓")
		}
		fmt.Fprintf(&b, "%s%s %s  %s  %s\n", cursor, tx,
			style.Render(fmt.Sprintf("%-44s", filepath.Base(e.Path))),
			dimStyle.Render(e.Mod.Format("02/01/2006 15:04")),
			dimStyle.Render(humanSize(e.Size)))
	}
	var confirm string
	if m.libConfirm {
		confirm = errStyle.Render(fmt.Sprintf("delete %s and its transcript? y / any other key cancels",
			filepath.Base(m.lib[m.libCursor].Path)))
	}
	hints := keyHint("enter", "open", "/", "search", "d", "delete", "o", "folder", "esc", "back")
	parts := []string{head, "", panelStyle.Render(strings.TrimRight(b.String(), "\n"))}
	if confirm != "" {
		parts = append(parts, confirm)
	}
	parts = append(parts, "", hints)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// --- done ---

func (m model) viewDone() string {
	var lines []string
	lines = append(lines, okStyle.Render("recording saved  ")+valueStyle.Render(m.file))
	if m.notice != "" {
		lines = append(lines, warnStyle.Render(m.notice))
	}

	// session stats from the diarised segments
	if m.didTrans && len(m.segs) > 0 {
		var dur, words float64
		for _, s := range m.segs {
			if s.End > dur {
				dur = s.End
			}
			words += float64(len(strings.Fields(s.Text)))
		}
		lines = append(lines, dimStyle.Render(fmt.Sprintf("%s of speech · %.0f words · %d marker(s) · %d clip(s)",
			fmtClock(dur), words, len(m.markers), m.clips)))
		if len(m.stats) > 0 {
			lines = append(lines, "")
			lines = append(lines, labelStyle.Render("talk time"))
			for _, s := range m.stats {
				lines = append(lines, fmt.Sprintf("  %s %s  %s",
					renderProgress(22, s.Share),
					dimStyle.Render(fmt.Sprintf("%4.0f%%", s.Share*100)),
					valueStyle.Render(displayName(m.stats, s.ID))))
			}
		}
	}

	switch {
	case m.didTrans && len(m.segs) == 0:
		// transcription ran but heard nothing — do not pretend there is a
		// transcript worth opening
	case m.didTrans && m.txDir != "":
		lines = append(lines, "")
		lines = append(lines, okStyle.Render("transcript in    ")+valueStyle.Render(m.txDir))
		if m.transcriptMD != "" {
			lines = append(lines, dimStyle.Render("named transcript: "+filepath.Base(m.transcriptMD)))
		}
		if len(m.preview) > 0 {
			lines = append(lines, "")
			for _, p := range m.preview {
				if len(p) > 100 {
					p = p[:100] + "…"
				}
				lines = append(lines, dimStyle.Render("  "+p))
			}
		}
	case m.transErr != nil:
		lines = append(lines, errStyle.Render("transcription failed: "+m.transErr.Error()))
		lines = append(lines, dimStyle.Render("the recording is safe — press t to retry"))
	}
	if m.markersFile != "" {
		lines = append(lines, dimStyle.Render("markers: "+filepath.Base(m.markersFile)))
	}
	if m.postStatus != "" {
		style := dimStyle
		if strings.Contains(m.postStatus, "failed") {
			style = errStyle
		}
		lines = append(lines, style.Render(m.postStatus))
	}

	playHint := "play"
	if m.playing {
		playHint = "stop playback"
	}
	hintPairs := []string{"p", playHint, "n", "new recording", "o", "open folder"}
	if m.didTrans && len(m.stats) > 0 {
		hintPairs = append([]string{"s", "speakers"}, hintPairs...)
	}
	if !m.didTrans || len(m.segs) == 0 {
		hintPairs = append([]string{"t", "transcribe"}, hintPairs...)
	}
	if m.fromLib {
		hintPairs = append(hintPairs, "esc", "library")
	}
	hintPairs = append(hintPairs, "q", "quit")
	hints := keyHint(hintPairs...)
	return lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Render(strings.Join(lines, "\n")), "", hints)
}

func humanSize(n int64) string {
	switch {
	case n > 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
