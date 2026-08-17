package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
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
	screenSpeakers
	screenLibrary
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
	fieldTheme
	fieldStart
	fieldCount
)

var modelChoices = []string{"large-v3-turbo", "large-v3", "medium", "small", "base"}
var languageChoices = []string{"en", "auto"}
var bufferChoices = []int{0, 5, 10, 15, 20, 30} // 0 = no buffer, record from trigger
var zoomLevels = []int{1, 2, 5}                 // meter ticks per Braille sub-column

type meterMsg MeterTick
type meterClosedMsg struct{}
type recErrMsg struct{ err error }
type transLineMsg string
type transLinesClosedMsg struct{}
type transDoneMsg struct{ err error }
type playDoneMsg struct{}

// transcription pipeline stages, parsed from whispermlx output
var stageNames = []string{"load model", "voice activity", "transcribe", "align", "diarise"}

var segLineRe = regexp.MustCompile(`^\[\s*\d+(\.\d+)?\s*-->\s*\d+(\.\d+)?\]\s*(.+)$`)
var pctRe = regexp.MustCompile(`Transcribing:\s*(\d+)%`)

func stageOf(line string) int {
	switch {
	case strings.Contains(line, "Performing diarization") || strings.Contains(line, "Loading diarization model"):
		return 4
	case strings.Contains(line, "Performing alignment"):
		return 3
	case strings.Contains(line, "Performing transcription") || strings.Contains(line, "Transcribing:"):
		return 2
	case strings.Contains(line, "voice activity detection"):
		return 1
	case strings.Contains(line, "Loading MLX Whisper model"):
		return 0
	}
	return -1
}

type libEntry struct {
	Path  string
	Mod   time.Time
	Size  int64
	HasTx bool
}

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
	orphans   []orphan

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
	markers   []time.Duration
	zoomIdx   int
	notice    string
	diskFree  int64
	diskTick  int

	// transcription
	trans      *Transcriber
	txDir      string
	spin       spinner.Model
	transLog   []string
	transErr   error
	didTrans   bool
	preview    []string
	showLog    bool
	transStart time.Time
	stage      int
	stagePct   float64  // transcription stage progress 0..1
	liveSegs   []string // transcript segments streaming in

	// speakers
	segs      []wSeg
	stats     []spkStat
	spkCursor int
	spkEdit   bool
	spkInput  textinput.Model

	// done / library
	transcriptMD string
	markersFile  string
	playCmd      *exec.Cmd
	playing      bool
	postStatus   string
	postRan      bool
	lib          []libEntry
	libCursor    int
	libConfirm   bool
	fromLib      bool
}

func newModel() model {
	cfg := loadConfig()
	applyTheme(applyOverrides(themeByName(cfg.Theme), cfg.ThemeColors))
	sweepStaleSpools()

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

	spk := textinput.New()
	spk.CharLimit = 48
	spk.Prompt = ""

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Blue))

	return model{
		cfg:       cfg,
		scr:       screenSetup,
		devices:   devices,
		devIdx:    devIdx,
		outInput:  out,
		nameInput: name,
		spkInput:  spk,
		spin:      sp,
		rmaxdb:    -99,
		orphans:   findOrphans(),
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
	if ch == nil {
		return nil
	}
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

func waitPlay(cmd *exec.Cmd) tea.Cmd {
	return func() tea.Msg {
		_ = cmd.Wait()
		return playDoneMsg{}
	}
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
		case screenTranscribing:
			if msg.String() == "l" {
				m.showLog = !m.showLog
			}
			return m, nil
		case screenSpeakers:
			return m.updateSpeakerKeys(msg)
		case screenLibrary:
			return m.updateLibraryKeys(msg)
		case screenDone:
			return m.updateDoneKeys(msg)
		}
		return m, nil

	case remoteCmdMsg:
		return m.handleRemote(msg.cmd)

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
		if len(m.wave) > 8192 {
			m.wave = m.wave[len(m.wave)-4096:]
		}
		m.rmaxdb -= 0.5 // decays 10 dB/s at 20 ticks/s
		if t.DB > m.rmaxdb {
			m.rmaxdb = t.DB
		}
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
		if m.diskTick++; m.diskTick >= 100 { // every ~5 s
			m.diskTick = 0
			m.diskFree = freeBytes(m.cfg.OutDir)
		}
		return m, waitMeter(m.ticks())

	case meterClosedMsg:
		// The metering stream died (device change?). Try to revive it so
		// the display keeps moving; capture recovery is recErrMsg's job.
		if m.scr == screenRecording || m.scr == screenArmed {
			if m.meter != nil {
				if soxPath, err := findBin("sox"); err == nil {
					if nm, err := startMeter(soxPath, m.cfg.Device, m.cfg.Channels); err == nil {
						m.meter = nm
						return m, waitMeter(nm.Ticks)
					}
				}
			} else if m.rec != nil {
				m.rec.reviveMeter()
				return m, waitMeter(m.ticks())
			}
		}
		return m, nil

	case recErrMsg:
		if m.scr != screenRecording || msg.err == nil || m.rec == nil {
			return m, nil
		}
		dev, err := m.rec.Recover()
		if err == nil {
			m.notice = "input device hiccup — capture continued on " + dev
			m.markers = append(m.markers, m.liveElapsed())
			return m, tea.Batch(waitRecErr(m.rec.Err), waitMeter(m.ticks()))
		}
		// Cannot continue: salvage what we have.
		_ = m.rec.Stop()
		m.notice = "recording stopped: " + err.Error()
		m.finishFiles()
		m.scr = screenDone
		return m, nil

	case transLineMsg:
		line := string(msg)
		m.transLog = append(m.transLog, line)
		if len(m.transLog) > 400 {
			m.transLog = m.transLog[len(m.transLog)-200:]
		}
		if s := stageOf(line); s > m.stage {
			m.stage = s
		}
		if pm := pctRe.FindStringSubmatch(line); pm != nil {
			if pct, err := strconv.Atoi(pm[1]); err == nil {
				m.stagePct = float64(pct) / 100
			}
		}
		if sm := segLineRe.FindStringSubmatch(line); sm != nil {
			m.liveSegs = append(m.liveSegs, strings.TrimSpace(sm[3]))
			if len(m.liveSegs) > 4 {
				m.liveSegs = m.liveSegs[len(m.liveSegs)-4:]
			}
		}
		return m, waitTransLine(m.trans.Lines)

	case transLinesClosedMsg:
		return m, nil

	case transDoneMsg:
		m.transErr = msg.err
		m.didTrans = msg.err == nil
		if !m.didTrans {
			m.scr = screenDone
			return m, nil
		}
		m.txDir = m.trans.OutDir
		m.preview = transcriptPreview(m.txDir, m.file, 6)
		m.segs = loadSegments(m.txDir, m.file)
		m.stats = speakerStats(m.segs)
		if len(m.segs) == 0 {
			m.notice = "transcription found no speech — check mic selection and gain (watch the level meter while talking)"
			m.scr = screenDone
			return m, nil
		}
		if len(m.stats) > 1 {
			m.spkCursor = 0
			m.spkEdit = false
			m.scr = screenSpeakers
			return m, nil
		}
		return m.enterDone()

	case postDoneMsg:
		if msg.err != nil {
			m.postStatus = "post-hook failed: " + msg.err.Error()
		} else {
			m.postStatus = "post-hook: done"
		}
		return m, nil

	case playDoneMsg:
		m.playing = false
		m.playCmd = nil
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
	if m.spkEdit {
		return m.updateSpeakerEdit(msg)
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
	if m.playCmd != nil && m.playCmd.Process != nil {
		_ = m.playCmd.Process.Kill()
	}
}

func (m *model) liveElapsed() time.Duration {
	switch {
	case m.spool != nil && !m.mark.IsZero():
		return time.Since(m.mark)
	case m.rec != nil:
		return m.rec.Elapsed()
	default:
		return 0
	}
}

// handleRemote services control-socket commands from `soundbooth <cmd>`.
func (m model) handleRemote(cmd string) (tea.Model, tea.Cmd) {
	switch cmd {
	case "trigger":
		if m.scr == screenArmed {
			return m.updateArmedKeys(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case "stop":
		if m.scr == screenRecording {
			return m.updateRecordingKeys(tea.KeyMsg{Type: tea.KeyEnter})
		}
	case "marker":
		if m.scr == screenRecording && !(m.rec != nil && m.rec.Paused()) {
			m.markers = append(m.markers, m.liveElapsed())
		}
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
	case "b":
		m.lib = loadLibrary(expandHome(strings.TrimSpace(m.outInput.Value())))
		m.libCursor = 0
		m.libConfirm = false
		m.scr = screenLibrary
		return m, nil
	case "r":
		if len(m.orphans) > 0 {
			return m.recoverOrphan(m.orphans[0])
		}
	case "d":
		if len(m.orphans) > 0 {
			_ = os.RemoveAll(m.orphans[0].Dir)
			m.orphans = findOrphans()
		}
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

func (m model) recoverOrphan(o orphan) (tea.Model, tea.Cmd) {
	soxPath, err := findBin("sox")
	if err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	file := o.Meta.File
	if file == "" {
		file = filepath.Join(expandHome(strings.TrimSpace(m.outInput.Value())),
			fmt.Sprintf("recovered-%s.flac", time.Now().Format("20060102-150405")))
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		m.setupErr = err.Error()
		return m, nil
	}
	if err := concatFlac(soxPath, o.Segments, file); err != nil {
		m.setupErr = "recovery failed: " + err.Error()
		return m, nil
	}
	_ = os.RemoveAll(o.Dir)
	m.orphans = findOrphans()
	m.file = file
	m.didTrans = false
	m.transErr = nil
	m.notice = "recovered from unfinished session"
	m.markers = nil
	m.scr = screenDone
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
	case fieldTheme:
		names := themeNames()
		i := indexOf(names, m.cfg.Theme)
		m.cfg.Theme = names[(i+dir+len(names))%len(names)]
		applyTheme(applyOverrides(themeByName(m.cfg.Theme), m.cfg.ThemeColors))
		m.spin.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Blue))
		_ = m.cfg.save()
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
	m.vuLevel = 0
	m.markers = nil
	m.notice = ""
	m.postStatus = ""
	m.postRan = false
	m.fromLib = false
	m.diskFree = freeBytes(m.cfg.OutDir)

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
		m.finishFiles()
		if m.cfg.Transcribe {
			return m.beginTranscribe()
		}
		return m.enterDone()
	case "m":
		if !(m.rec != nil && m.rec.Paused()) {
			m.markers = append(m.markers, m.liveElapsed())
		}
		return m, nil
	case "+", "=":
		if m.zoomIdx > 0 {
			m.zoomIdx--
		}
		return m, nil
	case "-", "_":
		if m.zoomIdx < len(zoomLevels)-1 {
			m.zoomIdx++
		}
		return m, nil
	case "p", " ":
		if !armed && m.rec != nil {
			if m.rec.Paused() {
				if err := m.rec.Resume(); err != nil {
					m.notice = err.Error()
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
		m.finishFiles()
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

// finishFiles writes capture side-artifacts (markers).
func (m *model) finishFiles() {
	m.markersFile = writeMarkersFile(m.file, m.markers)
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
	m.liveSegs = nil
	m.stage = 0
	m.stagePct = 0
	m.showLog = false
	m.postRan = false // a fresh transcription earns a fresh post-hook run
	m.transStart = time.Now()
	m.scr = screenTranscribing
	return m, tea.Batch(m.spin.Tick, waitTransLine(trans.Lines), waitTransDone(trans.Done))
}

// --- speakers ---

func (m model) updateSpeakerKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.spkEdit {
		return m.updateSpeakerEdit(msg)
	}
	switch msg.String() {
	case "up", "k":
		if m.spkCursor > 0 {
			m.spkCursor--
		}
	case "down", "j", "tab":
		if m.spkCursor < len(m.stats)-1 {
			m.spkCursor++
		}
	case "enter":
		m.spkEdit = true
		m.spkInput.SetValue(m.stats[m.spkCursor].Name)
		m.spkInput.Focus()
		return m, textinput.Blink
	case "c", "s":
		return m.applySpeakers()
	}
	return m, nil
}

func (m model) updateSpeakerEdit(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "enter", "esc":
			if key.String() == "enter" {
				m.stats[m.spkCursor].Name = strings.TrimSpace(m.spkInput.Value())
			}
			m.spkEdit = false
			m.spkInput.Blur()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.spkInput, cmd = m.spkInput.Update(msg)
	return m, cmd
}

func (m model) applySpeakers() (tea.Model, tea.Cmd) {
	if path, err := writeNamedTranscript(m.txDir, m.file, m.segs, m.stats, m.markers); err == nil {
		m.transcriptMD = path
	}
	return m.enterDone()
}

// enterDone lands on the done screen; fires the post-hook once per
// successful transcription.
func (m model) enterDone() (tea.Model, tea.Cmd) {
	if m.didTrans && m.transcriptMD == "" && len(m.segs) > 0 {
		if path, err := writeNamedTranscript(m.txDir, m.file, m.segs, m.stats, m.markers); err == nil {
			m.transcriptMD = path
		}
	}
	m.scr = screenDone
	if m.didTrans && !m.postRan && m.cfg.PostCommand != "" {
		m.postRan = true
		m.postStatus = "post-hook: running..."
		return m, runPostHook(m.cfg.PostCommand, m.file, m.txDir, m.transcriptMD, m.markersFile)
	}
	return m, nil
}

// --- library ---

func loadLibrary(dir string) []libEntry {
	matches, _ := filepath.Glob(filepath.Join(dir, "*.flac"))
	var out []libEntry
	for _, p := range matches {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		stem := strings.TrimSuffix(p, filepath.Ext(p))
		txInfo, err := os.Stat(stem)
		e := libEntry{Path: p, Mod: info.ModTime(), Size: info.Size(),
			HasTx: err == nil && txInfo.IsDir()}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mod.After(out[j].Mod) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func (m model) updateLibraryKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.libConfirm {
		switch msg.String() {
		case "y":
			e := m.lib[m.libCursor]
			_ = os.Remove(e.Path)
			_ = os.RemoveAll(strings.TrimSuffix(e.Path, filepath.Ext(e.Path)))
			for _, suffix := range []string{"-markers.txt"} {
				_ = os.Remove(strings.TrimSuffix(e.Path, filepath.Ext(e.Path)) + suffix)
			}
			m.lib = loadLibrary(m.cfg.OutDir)
			if m.libCursor >= len(m.lib) && m.libCursor > 0 {
				m.libCursor--
			}
		}
		m.libConfirm = false
		return m, nil
	}
	switch msg.String() {
	case "esc", "q", "b":
		m.scr = screenSetup
	case "up", "k":
		if m.libCursor > 0 {
			m.libCursor--
		}
	case "down", "j":
		if m.libCursor < len(m.lib)-1 {
			m.libCursor++
		}
	case "d":
		if len(m.lib) > 0 {
			m.libConfirm = true
		}
	case "o":
		_ = exec.Command("open", m.cfg.OutDir).Start()
	case "enter":
		if len(m.lib) == 0 {
			return m, nil
		}
		e := m.lib[m.libCursor]
		m.file = e.Path
		m.didTrans = e.HasTx
		m.transErr = nil
		m.transcriptMD = ""
		m.markers = nil
		m.markersFile = ""
		m.postRan = true // never auto-run hooks on old recordings
		m.postStatus = ""
		m.notice = ""
		m.fromLib = true
		if e.HasTx {
			m.txDir = strings.TrimSuffix(e.Path, filepath.Ext(e.Path))
			m.preview = transcriptPreview(m.txDir, e.Path, 6)
			m.segs = loadSegments(m.txDir, e.Path)
			m.stats = speakerStats(m.segs)
			md := filepath.Join(m.txDir, audioStem(e.Path)+"-transcript.md")
			if _, err := os.Stat(md); err == nil {
				m.transcriptMD = md
			}
		} else {
			m.txDir = ""
			m.preview = nil
			m.segs = nil
			m.stats = nil
		}
		m.scr = screenDone
	}
	return m, nil
}

// --- done ---

func (m model) updateDoneKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q":
		m.teardown()
		return m, tea.Quit
	case "esc":
		if m.fromLib {
			m.stopPlayback()
			m.lib = loadLibrary(m.cfg.OutDir)
			m.scr = screenLibrary
			return m, nil
		}
		m.teardown()
		return m, tea.Quit
	case "o":
		_ = exec.Command("open", filepath.Dir(m.file)).Start()
	case "p":
		if m.playing {
			m.stopPlayback()
			return m, nil
		}
		cmd := exec.Command("afplay", m.file)
		if err := cmd.Start(); err == nil {
			m.playCmd = cmd
			m.playing = true
			return m, waitPlay(cmd)
		}
	case "t":
		if m.file != "" {
			if _, err := os.Stat(m.file); err == nil {
				m.transErr = nil
				return m.beginTranscribe()
			}
		}
	case "s":
		if m.didTrans && len(m.stats) > 0 {
			m.spkCursor = 0
			m.spkEdit = false
			m.scr = screenSpeakers
		}
	case "n":
		m.stopPlayback()
		fresh := newModel()
		fresh.width, fresh.height = m.width, m.height
		return fresh, nil
	}
	return m, nil
}

func (m *model) stopPlayback() {
	if m.playCmd != nil && m.playCmd.Process != nil {
		_ = m.playCmd.Process.Kill()
	}
	m.playing = false
	m.playCmd = nil
}

func transcriptPreview(outDir, audioFile string, lines int) []string {
	data, err := os.ReadFile(filepath.Join(outDir, audioStem(audioFile)+".txt"))
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
