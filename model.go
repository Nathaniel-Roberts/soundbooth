package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type screen int

const (
	screenSetup screen = iota
	screenArmed
	screenRecording
	screenTranscribing
	screenDone
)

// setup form fields, in display order
const (
	fieldDevice = iota
	fieldOutDir
	fieldName
	fieldChannels
	fieldMode
	fieldBuffer
	fieldTranscribe
	fieldModel
	fieldSpeakers
	fieldLanguage
	fieldStart
	fieldCount
)

var modelChoices = []string{"large-v3-turbo", "large-v3", "medium", "small", "base"}
var languageChoices = []string{"en", "auto"}
var bufferChoices = []int{0, 5, 10, 15, 20, 30} // 0 = no buffer, record from trigger

type meterMsg MeterTick
type meterClosedMsg struct{}
type recErrMsg struct{ err error }
type transLineMsg string
type transLinesClosedMsg struct{}
type transDoneMsg struct{ err error }

type model struct {
	cfg    Config
	scr    screen
	width  int
	height int

	// setup
	devices   []Device
	devIdx    int
	cursor    int
	editing   bool
	outInput  textinput.Model
	nameInput textinput.Model
	setupErr  string

	// live capture (either mode)
	rec       *Recorder // record-now mode
	spool     *Spooler  // armed mode
	meter     *Meter    // armed mode's meter (record mode uses rec.Meter)
	armStart  time.Time
	mark      time.Time // armed: when the save was triggered
	file      string
	name      string
	wave      []waveCol
	rmaxdb    float64
	clips     int
	clipTicks int
	vuLevel   float64 // fast-attack / slow-release bar level, 0..1


	// transcription
	trans    *Transcriber
	spin     spinner.Model
	transLog []string
	transErr error
	didTrans bool
	preview  []string
}

func newModel() model {
	cfg := loadConfig()

	devices := listInputDevices()
	devIdx := 0
	for i, d := range devices {
		if d.Name == cfg.Device {
			devIdx = i
		}
	}

	out := textinput.New()
	out.SetValue(cfg.OutDir)
	out.CharLimit = 512
	out.Prompt = ""

	name := textinput.New()
	name.SetValue("recording")
	name.CharLimit = 64
	name.Prompt = ""

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(mochaBlue)

	return model{
		cfg:       cfg,
		scr:       screenSetup,
		devices:   devices,
		devIdx:    devIdx,
		outInput:  out,
		nameInput: name,
		spin:      sp,
		rmaxdb:    -99,
	}
}

func (m model) Init() tea.Cmd { return nil }

func (m *model) ticks() chan MeterTick {
	if m.meter != nil {
		return m.meter.Ticks
	}
	if m.rec != nil && m.rec.Meter != nil {
		return m.rec.Meter.Ticks
	}
	return nil
}

func waitMeter(ch chan MeterTick) tea.Cmd {
	return func() tea.Msg {
		t, ok := <-ch
		if !ok {
			return meterClosedMsg{}
		}
		return meterMsg(t)
	}
}

func waitRecErr(ch chan error) tea.Cmd {
	return func() tea.Msg { return recErrMsg{<-ch} }
}

func waitTransLine(ch chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return transLinesClosedMsg{}
		}
		return transLineMsg(line)
	}
}

func waitTransDone(ch chan error) tea.Cmd {
	return func() tea.Msg { return transDoneMsg{<-ch} }
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.teardown()
			return m, tea.Quit
		}
		switch m.scr {
		case screenSetup:
			return m.updateSetupKeys(msg)
		case screenArmed:
			return m.updateArmedKeys(msg)
		case screenRecording:
			return m.updateRecordingKeys(msg)
		case screenDone:
			return m.updateDoneKeys(msg)
		}
		return m, nil

	case meterMsg:
		if m.scr != screenRecording && m.scr != screenArmed {
			return m, nil
		}
		t := MeterTick(msg)
		col := waveCol{rms: t.RMS, peak: t.Peak, rmsR: t.RMSR, peakR: t.PeakR, clip: t.Clip}
		if m.rec != nil && m.rec.Paused() {
			col.paused = true
			col.clip = false
		}
		m.wave = append(m.wave, col)
		if len(m.wave) > 2048 {
			m.wave = m.wave[len(m.wave)-1024:]
		}
		m.rmaxdb -= 0.5 // decays 10 dB/s at 20 ticks/s
		if t.DB > m.rmaxdb {
			m.rmaxdb = t.DB
		}
		// VU ballistics: instant attack, exponential release
		target := t.Peak
		if t.PeakR > target {
			target = t.PeakR
		}
		if target > m.vuLevel {
			m.vuLevel = target
		} else {
			m.vuLevel *= 0.82
		}
		if col.clip {
			m.clips++
			m.clipTicks = 40
		} else if m.clipTicks > 0 {
			m.clipTicks--
		}
		return m, waitMeter(m.ticks())

	case meterClosedMsg:
		return m, nil

	case recErrMsg:
		if (m.scr == screenRecording || m.scr == screenArmed) && msg.err != nil {
			m.teardown()
			m.setupErr = msg.err.Error()
			m.scr = screenSetup
		}
		return m, nil

	case transLineMsg:
		m.transLog = append(m.transLog, string(msg))
		if len(m.transLog) > 400 {
			m.transLog = m.transLog[len(m.transLog)-200:]
		}
		return m, waitTransLine(m.trans.Lines)

	case transLinesClosedMsg:
		return m, nil

	case transDoneMsg:
		m.transErr = msg.err
		m.didTrans = msg.err == nil
		if m.didTrans {
			m.preview = transcriptPreview(m.trans.OutDir, m.file, 6)
		}
		m.scr = screenDone
		return m, nil

	case spinner.TickMsg:
		if m.scr == screenTranscribing {
			var cmd tea.Cmd
			m.spin, cmd = m.spin.Update(msg)
			return m, cmd
		}
		return m, nil
	}

	if m.editing {
		return m.updateEditing(msg)
	}
	return m, nil
}

// teardown stops every live process. Recordings in flight are finalised;
// an untriggered armed buffer is discarded (that is its contract).
func (m *model) teardown() {
	if m.rec != nil {
		_ = m.rec.Stop()
	}
	if m.spool != nil {
		m.spool.Stop()
		m.spool.Cleanup()
		m.spool = nil
	}
	if m.meter != nil {
		m.meter.Stop()
		m.meter = nil
	}
}

// --- setup ---

func (m model) updateSetupKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.editing {
		return m.updateEditing(msg)
	}
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j", "tab":
		if m.cursor < fieldCount-1 {
			m.cursor++
		}
	case "left", "h":
		m.adjustField(-1)
	case "right", "l", " ":
		m.adjustField(1)
	case "enter":
		switch m.cursor {
		case fieldOutDir:
			m.editing = true
			m.outInput.Focus()
			return m, textinput.Blink
		case fieldName:
			m.editing = true
			m.nameInput.Focus()
			return m, textinput.Blink
		case fieldTranscribe:
			m.cfg.Transcribe = !m.cfg.Transcribe
		case fieldStart:
			return m.start()
		default:
			m.adjustField(1)
		}
	}
	return m, nil
}

func (m *model) adjustField(dir int) {
	switch m.cursor {
	case fieldDevice:
		m.devIdx = (m.devIdx + dir + len(m.devices)) % len(m.devices)
	case fieldChannels:
		if m.cfg.Channels == 1 {
			m.cfg.Channels = 2
		} else {
			m.cfg.Channels = 1
		}
	case fieldMode:
		if m.cfg.Mode == "record" {
			m.cfg.Mode = "armed"
		} else {
			m.cfg.Mode = "record"
		}
	case fieldBuffer:
		i := 0
		for j, b := range bufferChoices {
			if b == m.cfg.BufferMin {
				i = j
			}
		}
		m.cfg.BufferMin = bufferChoices[(i+dir+len(bufferChoices))%len(bufferChoices)]
	case fieldTranscribe:
		m.cfg.Transcribe = !m.cfg.Transcribe
	case fieldModel:
		i := indexOf(modelChoices, m.cfg.Model)
		m.cfg.Model = modelChoices[(i+dir+len(modelChoices))%len(modelChoices)]
	case fieldSpeakers:
		m.cfg.Speakers += dir
		if m.cfg.Speakers < 0 {
			m.cfg.Speakers = 8
		}
		if m.cfg.Speakers > 8 {
			m.cfg.Speakers = 0
		}
	case fieldLanguage:
		i := indexOf(languageChoices, m.cfg.Language)
		m.cfg.Language = languageChoices[(i+dir+len(languageChoices))%len(languageChoices)]
	}
}

func indexOf(list []string, v string) int {
	for i, s := range list {
		if s == v {
			return i
		}
	}
	return 0
}

func (m model) updateEditing(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter", "esc":
			m.editing = false
			m.outInput.Blur()
			m.nameInput.Blur()
			return m, nil
		}
	}
	var cmd tea.Cmd
	if m.cursor == fieldOutDir {
		m.outInput, cmd = m.outInput.Update(msg)
	} else {
		m.nameInput, cmd = m.nameInput.Update(msg)
	}
	return m, cmd
}

func (m model) start() (tea.Model, tea.Cmd) {
	m.setupErr = ""
	m.cfg.Device = m.devices[m.devIdx].Name
	m.cfg.OutDir = expandHome(strings.TrimSpace(m.outInput.Value()))
	m.name = strings.TrimSpace(m.nameInput.Value())
	if m.name == "" {
		m.name = "recording"
	}
	if err := os.MkdirAll(m.cfg.OutDir, 0o755); err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	_ = m.cfg.save()

	m.wave = nil
	m.rmaxdb = -99
	m.clips = 0
	m.clipTicks = 0

	if m.cfg.Mode == "armed" {
		return m.startArmed()
	}
	return m.startRecording()
}

func (m model) startRecording() (tea.Model, tea.Cmd) {
	m.file = filepath.Join(m.cfg.OutDir,
		fmt.Sprintf("%s-%s.flac", m.name, time.Now().Format("20060102-150405")))
	rec, err := NewRecorder(m.cfg.Device, m.file, m.cfg.Channels)
	if err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	if err := rec.Start(); err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	m.rec = rec
	m.scr = screenRecording
	return m, tea.Batch(waitMeter(rec.Meter.Ticks), waitRecErr(rec.Err))
}

func (m model) startArmed() (tea.Model, tea.Cmd) {
	soxPath, err := findBin("sox")
	if err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	var spool *Spooler
	if m.cfg.BufferMin > 0 {
		window := time.Duration(m.cfg.BufferMin) * time.Minute
		spool, err = startSpooler(m.devices[m.devIdx].AVIndex, m.cfg.Channels, window)
		if err != nil {
			m.setupErr = err.Error()
			return m, nil
		}
	}
	meter, err := startMeter(soxPath, m.cfg.Device, m.cfg.Channels)
	if err != nil {
		if spool != nil {
			spool.Stop()
			spool.Cleanup()
		}
		m.setupErr = err.Error()
		return m, nil
	}
	m.spool = spool
	m.meter = meter
	m.armStart = time.Now()
	m.mark = time.Time{}
	m.scr = screenArmed
	return m, waitMeter(meter.Ticks)
}

// --- armed ---

func (m model) updateArmedKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "s":
		if m.spool == nil {
			// No buffer: standby only — recording starts right now.
			if m.meter != nil {
				m.meter.Stop()
				m.meter = nil
			}
			return m.startRecording()
		}
		// Save trigger: keep the buffered window, keep capturing forward.
		m.mark = time.Now()
		m.spool.Trigger()
		m.file = filepath.Join(m.cfg.OutDir,
			fmt.Sprintf("%s-%s.flac", m.name, m.mark.Format("20060102-150405")))
		m.scr = screenRecording
		return m, nil
	case "x", "esc":
		// Disarm: the whole point is nothing survives unless triggered.
		m.teardown()
		m.scr = screenSetup
		return m, nil
	}
	return m, nil
}

// --- recording ---

func (m model) updateRecordingKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	armed := m.spool != nil
	switch msg.String() {
	case "enter", "s":
		if err := m.finishCapture(); err != nil {
			m.transErr = err
			m.scr = screenDone
			return m, nil
		}
		if m.cfg.Transcribe {
			return m.beginTranscribe()
		}
		m.scr = screenDone
		return m, nil
	case "p", " ":
		if !armed && m.rec != nil {
			if m.rec.Paused() {
				if err := m.rec.Resume(); err != nil {
					m.setupErr = err.Error()
				}
			} else {
				m.rec.Pause()
			}
		}
		return m, nil
	case "x", "esc":
		// keep the file, skip transcription
		if err := m.finishCapture(); err != nil {
			m.transErr = err
		}
		m.scr = screenDone
		return m, nil
	}
	return m, nil
}

// finishCapture stops whichever capture path is live and assembles m.file.
func (m *model) finishCapture() error {
	if m.spool != nil {
		segs := m.spool.Stop()
		if m.meter != nil {
			m.meter.Stop()
			m.meter = nil
		}
		soxPath, err := findBin("sox")
		if err != nil {
			return err
		}
		err = concatFlac(soxPath, segs, m.file)
		m.spool.Cleanup()
		m.spool = nil
		return err
	}
	if m.rec != nil {
		return m.rec.Stop()
	}
	return fmt.Errorf("nothing was recording")
}

func (m model) beginTranscribe() (tea.Model, tea.Cmd) {
	trans, err := startTranscribe(m.file, m.cfg.Model, m.cfg.Language, m.cfg.Speakers)
	if err != nil {
		m.transErr = err
		m.scr = screenDone
		return m, nil
	}
	m.trans = trans
	m.transLog = nil
	m.scr = screenTranscribing
	return m, tea.Batch(m.spin.Tick, waitTransLine(trans.Lines), waitTransDone(trans.Done))
}

// --- done ---

func (m model) updateDoneKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "o":
		_ = exec.Command("open", filepath.Dir(m.file)).Start()
	case "t":
		if !m.didTrans && m.file != "" {
			if _, err := os.Stat(m.file); err == nil {
				m.transErr = nil
				return m.beginTranscribe()
			}
		}
	case "n":
		fresh := newModel()
		fresh.width, fresh.height = m.width, m.height
		return fresh, nil
	}
	return m, nil
}

func transcriptPreview(outDir, audioFile string, lines int) []string {
	base := filepath.Base(audioFile)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	data, err := os.ReadFile(filepath.Join(outDir, stem+".txt"))
	if err != nil {
		return nil
	}
	var out []string
	for _, l := range strings.Split(string(data), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		out = append(out, l)
		if len(out) >= lines {
			break
		}
	}
	return out
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// --- views ---

func (m model) View() string {
	var body string
	switch m.scr {
	case screenSetup:
		body = m.viewSetup()
	case screenArmed:
		body = m.viewLive()
	case screenRecording:
		body = m.viewLive()
	case screenTranscribing:
		body = m.viewTranscribing()
	case screenDone:
		body = m.viewDone()
	}
	header := titleStyle.Render("soundbooth") + dimStyle.Render("  record · meter · transcribe")
	return lipgloss.JoinVertical(lipgloss.Left, header, "", body)
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
	var notes []string
	if m.cfg.Transcribe && !hfTokenPresent() {
		notes = append(notes, warnStyle.Render("no Hugging Face token (~/.cache/huggingface/token) — transcription will fail"))
	}
	if m.setupErr != "" {
		notes = append(notes, errStyle.Render(m.setupErr))
	}
	hints := keyHint("↑↓", "move", "←→", "change", "enter", "edit/start", "q", "quit")
	parts := append([]string{out}, notes...)
	parts = append(parts, "", hints)
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

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
	var wave string
	if m.cfg.Channels == 2 {
		wave = renderWaveStereo(m.wave, wCells, hCells)
	} else {
		wave = renderWave(m.wave, wCells, hCells)
	}
	wave += "\n" + renderRuler(wCells)

	panel := panelStyle
	if m.clipTicks > 0 {
		panel = panel.BorderForeground(mochaRed)
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
		hints = keyHint("enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "stop, skip transcribe", "ctrl+c", "abort")
	case m.rec != nil && m.rec.Paused():
		badge = warnStyle.Render("● PAUSED")
		detail = dimStyle.Render(fmt.Sprintf("%s recorded  ·  %s  %s",
			fmtDur(m.rec.Elapsed()), filepath.Base(m.file), humanSize(m.rec.FileSize())))
		hints = keyHint("p", "resume", "enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "stop, skip transcribe")
	default:
		badge = errStyle.Render("● REC")
		detail = dimStyle.Render(fmt.Sprintf("%s  ·  %s  %s",
			fmtDur(m.rec.Elapsed()), filepath.Base(m.file), humanSize(m.rec.FileSize())))
		hints = keyHint("p", "pause", "enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "stop, skip transcribe")
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
	return lipgloss.JoinVertical(lipgloss.Left, panel.Render(wave), status, vu, "", hints)
}

func bufferLabel(minutes int) string {
	if minutes == 0 {
		return "off — record from trigger (armed mode)"
	}
	return fmt.Sprintf("last %d min (armed mode)", minutes)
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

func (m model) viewTranscribing() string {
	tail := m.transLog
	maxLines := m.height - 10
	if maxLines < 4 {
		maxLines = 4
	}
	if len(tail) > maxLines {
		tail = tail[len(tail)-maxLines:]
	}
	log := dimStyle.Render(strings.Join(tail, "\n"))
	head := fmt.Sprintf("%s %s", m.spin.View(),
		valueStyle.Render("transcribing "+filepath.Base(m.file)+" ("+m.cfg.Model+", Apple GPU)"))
	return lipgloss.JoinVertical(lipgloss.Left, head, "", panelStyle.Render(log))
}

func (m model) viewDone() string {
	var lines []string
	lines = append(lines, okStyle.Render("recording saved  ")+valueStyle.Render(m.file))
	switch {
	case m.didTrans && m.trans != nil:
		lines = append(lines, okStyle.Render("transcript in    ")+valueStyle.Render(m.trans.OutDir))
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
	hintPairs := []string{"n", "new recording", "o", "open folder", "q", "quit"}
	if !m.didTrans {
		hintPairs = append([]string{"t", "transcribe"}, hintPairs...)
	}
	hints := keyHint(hintPairs...)
	return lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Render(strings.Join(lines, "\n")), "", hints)
}

func humanSize(n int64) string {
	switch {
	case n > 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n > 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
