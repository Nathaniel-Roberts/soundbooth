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
	fieldTranscribe
	fieldModel
	fieldSpeakers
	fieldLanguage
	fieldStart
	fieldCount
)

var modelChoices = []string{"large-v3-turbo", "large-v3", "medium", "small", "base"}
var languageChoices = []string{"en", "auto"}

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
	devices   []string
	devIdx    int
	cursor    int
	editing   bool
	outInput  textinput.Model
	nameInput textinput.Model
	setupErr  string

	// recording
	rec       *Recorder
	file      string
	wave      []waveCol
	rmaxdb    float64
	clips     int
	clipTicks int

	// transcription
	trans     *Transcriber
	spin      spinner.Model
	transLog  []string
	transErr  error
	didTrans  bool
}

func newModel() model {
	cfg := loadConfig()

	devices := listInputDevices()
	devIdx := 0
	for i, d := range devices {
		if d == cfg.Device {
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
			if m.rec != nil {
				m.rec.Stop()
			}
			return m, tea.Quit
		}
		switch m.scr {
		case screenSetup:
			return m.updateSetupKeys(msg)
		case screenRecording:
			return m.updateRecordingKeys(msg)
		case screenDone:
			return m.updateDoneKeys(msg)
		}
		return m, nil

	case meterMsg:
		if m.scr != screenRecording {
			return m, nil
		}
		t := MeterTick(msg)
		m.wave = append(m.wave, waveCol{rms: t.RMS, peak: t.Peak, clip: t.Clip})
		if len(m.wave) > 2048 {
			m.wave = m.wave[len(m.wave)-1024:]
		}
		m.rmaxdb -= 0.5 // decays 10 dB/s at 20 ticks/s
		if t.DB > m.rmaxdb {
			m.rmaxdb = t.DB
		}
		if t.Clip {
			m.clips++
			m.clipTicks = 40
		} else if m.clipTicks > 0 {
			m.clipTicks--
		}
		return m, waitMeter(m.rec.Ticks)

	case meterClosedMsg:
		return m, nil

	case recErrMsg:
		if m.scr == screenRecording && msg.err != nil {
			m.rec.Stop()
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
			return m.startRecording()
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

func (m model) startRecording() (tea.Model, tea.Cmd) {
	m.setupErr = ""
	m.cfg.Device = m.devices[m.devIdx]
	m.cfg.OutDir = expandHome(strings.TrimSpace(m.outInput.Value()))
	name := strings.TrimSpace(m.nameInput.Value())
	if name == "" {
		name = "recording"
	}
	if err := os.MkdirAll(m.cfg.OutDir, 0o755); err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	_ = m.cfg.save()

	m.file = filepath.Join(m.cfg.OutDir,
		fmt.Sprintf("%s-%s.flac", name, time.Now().Format("20060102-150405")))
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
	m.wave = nil
	m.rmaxdb = -99
	m.clips = 0
	m.clipTicks = 0
	m.scr = screenRecording
	return m, tea.Batch(waitMeter(rec.Ticks), waitRecErr(rec.Err))
}

// --- recording ---

func (m model) updateRecordingKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "s":
		m.rec.Stop()
		if m.cfg.Transcribe {
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
		m.scr = screenDone
		return m, nil
	case "x", "esc":
		// keep the file, skip transcription
		m.rec.Stop()
		m.scr = screenDone
		return m, nil
	}
	return m, nil
}

// --- done ---

func (m model) updateDoneKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "o":
		dir := filepath.Dir(m.file)
		_ = exec.Command("open", dir).Start()
	case "n":
		fresh := newModel()
		fresh.width, fresh.height = m.width, m.height
		return fresh, nil
	}
	return m, nil
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
	case screenRecording:
		body = m.viewRecording()
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
	transcribe := "off"
	if m.cfg.Transcribe {
		transcribe = "on"
	}
	channels := "mono"
	if m.cfg.Channels == 2 {
		channels = "stereo"
	}
	rows := []struct {
		label, value string
	}{
		{"Microphone", m.devices[m.devIdx]},
		{"Save to", m.outInput.View()},
		{"Name", m.nameInput.View()},
		{"Channels", channels},
		{"Transcribe", transcribe},
		{"Whisper model", m.cfg.Model},
		{"Speakers", speakers},
		{"Language", m.cfg.Language},
		{"", "[ Start recording ]"},
	}
	var b strings.Builder
	for i, r := range rows {
		cursor := "  "
		style := valueStyle
		if i == m.cursor {
			cursor = focusStyle.Render("> ")
			style = focusStyle
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

func (m model) viewRecording() string {
	wCells := m.width - 6
	if wCells < 20 {
		wCells = 60
	}
	hCells := (m.height - 10)
	if hCells > 12 {
		hCells = 12
	}
	if hCells < 5 {
		hCells = 5
	}
	wave := renderWave(m.wave, wCells, hCells)

	elapsed := m.rec.Elapsed().Round(time.Second)
	mins := int(elapsed.Minutes())
	secs := int(elapsed.Seconds()) % 60

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
	status := fmt.Sprintf("%s  %s  %s",
		dimStyle.Render(fmt.Sprintf("%02d:%02d", mins, secs)),
		levelStyle.Render(fmt.Sprintf("peak %d dB  %s", int(m.rmaxdb), advice)),
		dimStyle.Render(fmt.Sprintf("%s  %s", filepath.Base(m.file), humanSize(m.rec.FileSize()))),
	)
	hints := keyHint("enter", "stop"+transcribeSuffix(m.cfg.Transcribe), "x", "stop, skip transcribe", "ctrl+c", "abort")
	return lipgloss.JoinVertical(lipgloss.Left,
		panelStyle.Render(wave), status, "", hints)
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
		lines = append(lines, dimStyle.Render("(json with speaker labels, plus srt, vtt, txt)"))
	case m.transErr != nil:
		lines = append(lines, errStyle.Render("transcription failed: "+m.transErr.Error()))
		lines = append(lines, dimStyle.Render("the recording is safe — rerun with: transcribe "+m.file))
	}
	hints := keyHint("n", "new recording", "o", "open folder", "q", "quit")
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
