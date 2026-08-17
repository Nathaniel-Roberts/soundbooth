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
	fieldSysAudio
	fieldMode
	fieldBuffer
	fieldTranscribe
	fieldCaptions
	fieldModel
	fieldSpeakers
	fieldLanguage
	fieldTheme
	fieldRetention
	fieldStart
	fieldCount
)

var retentionChoices = []int{0, 7, 30, 90, 180}

var modelChoices = []string{"large-v3-turbo", "large-v3", "medium", "small", "base"}
var languageChoices = []string{"en", "auto"}
var bufferChoices = []int{0, 5, 10, 15, 20, 30} // 0 = no buffer, record from trigger
var zoomLevels = []int{1, 2, 4, 10} // meter ticks per Braille sub-column

// frameMsg is the live view's fixed 50 ms frame clock. Audio ticks are
// pulled from the meter channels on each frame rather than pushed as
// messages: sox delivers ticks in bursts (pipe buffering), and painting
// on arrival made the waveform crawl and lurch instead of scroll.
type frameMsg struct{ gen int }
type sysReadyMsg struct{ err error }
type captionMsg string
type captionsClosedMsg struct{}
type recErrMsg struct{ err error }
type transLineMsg string
type transLinesClosedMsg struct{}
type transDoneMsg struct{ err error }
type playDoneMsg struct{ gen int }

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
	setupNote string
	orphans   []orphan

	// live capture (either mode)
	rec       *Recorder // record-now mode
	spool     *Spooler  // armed mode
	meter     *Meter    // armed mode's meter (record mode uses rec.Meter)
	sysCap    *SysCapture
	sysLevel  MeterTick // latest system-audio meter tick
	micFile   string    // temp mic track when system audio is being merged
	captioner *Captioner
	captions  []string // rolling live captions
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
	frameGen  int // invalidates stale frame clocks

	// perf overlay (toggled with f on the live screens)
	showPerf   bool
	perfLast   time.Time
	perfAvgMs  float64
	perfMaxMs  float64
	perfTicks  float64

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
	playGen      int
	playStart    time.Time
	pWave        []waveCol
	pDur         float64
	pPos         float64
	pReady       bool
	pDecoding    bool
	postStatus   string
	postRan      bool
	lib          []libEntry
	libCursor    int
	libConfirm   bool
	fromLib      bool
	libSearching bool // typing a query
	searchInput  textinput.Model
	hits         []searchHit
	hitCursor    int
	showHits     bool
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

	search := textinput.New()
	search.CharLimit = 128
	search.Prompt = "/ "
	search.Placeholder = "search transcripts"

	sp := spinner.New(spinner.WithSpinner(spinner.MiniDot))
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Blue))

	setupNote := ""
	if n := sweepRetention(cfg.OutDir, cfg.RetentionDays); n > 0 {
		setupNote = fmt.Sprintf("retention: deleted %d recording(s) older than %d days (transcripts kept)", n, cfg.RetentionDays)
	}

	return model{
		cfg:       cfg,
		scr:       screenSetup,
		devices:   devices,
		devIdx:    devIdx,
		outInput:    out,
		nameInput:   name,
		spkInput:    spk,
		searchInput: search,
		spin:        sp,
		rmaxdb:    -99,
		zoomIdx:   1, // 100 ms cells; zoom 0 is the 50 ms close-up
		orphans:   findOrphans(),
		setupNote: setupNote,
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

func frameTick(gen int) tea.Cmd {
	return tea.Tick(time.Second/tickHz, func(time.Time) tea.Msg { return frameMsg{gen: gen} })
}

func waitRecErr(ch chan error) tea.Cmd {
	return func() tea.Msg { return recErrMsg{<-ch} }
}

func waitSysReady(s *SysCapture) tea.Cmd {
	return func() tea.Msg { return sysReadyMsg{s.WaitReady(6 * time.Second)} }
}

func waitCaption(ch chan string) tea.Cmd {
	return func() tea.Msg {
		line, ok := <-ch
		if !ok {
			return captionsClosedMsg{}
		}
		return captionMsg(line)
	}
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

func waitPlay(cmd *exec.Cmd, gen int) tea.Cmd {
	return func() tea.Msg {
		_ = cmd.Wait()
		return playDoneMsg{gen: gen}
	}
}

// --- player helpers ---

func (m *model) resetPlayer() {
	m.stopPlayback()
	m.pWave = nil
	m.pDur = 0
	m.pPos = 0
	m.pReady = false
	m.pDecoding = false
}

// ensureDecode kicks the whole-file waveform decode for the player.
func (m *model) ensureDecode() tea.Cmd {
	if m.pReady || m.pDecoding || m.file == "" {
		return nil
	}
	if _, err := os.Stat(m.file); err != nil {
		return nil
	}
	w := m.width - 6
	if w < 20 {
		w = 60
	}
	m.pDecoding = true
	return decodeWave(m.file, w*2)
}

// curPos is the current playback position in seconds.
func (m *model) curPos() float64 {
	pos := m.pPos
	if m.playing {
		pos += time.Since(m.playStart).Seconds()
	}
	if m.pDur > 0 && pos > m.pDur {
		pos = m.pDur
	}
	if pos < 0 {
		pos = 0
	}
	return pos
}

// seekTo moves the playhead, restarting audio if currently playing.
func (m *model) seekTo(t float64) tea.Cmd {
	if t < 0 {
		t = 0
	}
	if m.pDur > 0 && t > m.pDur-0.2 {
		t = m.pDur - 0.2
		if t < 0 {
			t = 0
		}
	}
	wasPlaying := m.playing
	m.stopPlayback()
	m.pPos = t
	if !wasPlaying {
		return nil
	}
	return m.beginPlayback()
}

func (m *model) beginPlayback() tea.Cmd {
	cmd, err := startPlayback(m.file, m.pPos)
	if err != nil {
		m.notice = err.Error()
		return nil
	}
	m.playCmd = cmd
	m.playing = true
	m.playGen++
	m.playStart = time.Now()
	return tea.Batch(waitPlay(cmd, m.playGen), playTick())
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

	case sysReadyMsg:
		if msg.err != nil && m.sysCap != nil {
			m.sysCap.Stop()
			m.sysCap = nil
			m.notice = "system audio unavailable: " + msg.err.Error() + " — recording mic only"
		}
		return m, nil

	case captionMsg:
		m.captions = append(m.captions, string(msg))
		if len(m.captions) > 6 {
			m.captions = m.captions[len(m.captions)-6:]
		}
		if m.captioner != nil {
			return m, waitCaption(m.captioner.Lines)
		}
		return m, nil

	case captionsClosedMsg:
		return m, nil

	case frameMsg:
		if msg.gen != m.frameGen || (m.scr != screenRecording && m.scr != screenArmed) {
			return m, nil // stale clock, or we left the live screens
		}
		now := time.Now()
		if !m.perfLast.IsZero() {
			iv := float64(now.Sub(m.perfLast).Microseconds()) / 1000
			m.perfAvgMs += (iv - m.perfAvgMs) * 0.05
			m.perfMaxMs *= 0.98
			if iv > m.perfMaxMs {
				m.perfMaxMs = iv
			}
		}
		m.perfLast = now
		pulled := 0
		// pull the latest system-audio level first so mic columns built
		// this frame carry it
		if m.sysCap != nil {
		drainSys:
			for i := 0; i < 80; i++ {
				select {
				case t, ok := <-m.sysCap.Ticks:
					if !ok {
						break drainSys
					}
					m.sysLevel = t
				default:
					break drainSys
				}
			}
		}
		ch := m.ticks()
		closed := false
	drain:
		for i := 0; i < 80; i++ {
			select {
			case t, ok := <-ch:
				if !ok {
					closed = true
					break drain
				}
				m.applyTick(t)
				pulled++
			default:
				break drain
			}
		}
		if closed {
			m.reviveMeters()
		}
		m.perfTicks += (float64(pulled) - m.perfTicks) * 0.05
		return m, frameTick(m.frameGen)

	case recErrMsg:
		if m.scr != screenRecording || msg.err == nil || m.rec == nil {
			return m, nil
		}
		dev, err := m.rec.Recover()
		if err == nil {
			m.notice = "input device hiccup — capture continued on " + dev
			m.markers = append(m.markers, m.liveElapsed())
			return m, waitRecErr(m.rec.Err)
		}
		// Cannot continue: salvage what we have.
		_ = m.rec.Stop()
		m.notice = "recording stopped: " + err.Error()
		m.finishFiles()
		m.scr = screenDone
		m.resetPlayer()
		return m, m.ensureDecode()

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
			m.resetPlayer()
			return m, m.ensureDecode()
		}
		m.txDir = m.trans.OutDir
		m.preview = transcriptPreview(m.txDir, m.file, 6)
		m.segs = loadSegments(m.txDir, m.file)
		m.stats = speakerStats(m.segs)
		if len(m.segs) == 0 {
			m.notice = "transcription found no speech — check mic selection and gain (watch the level meter while talking)"
			m.scr = screenDone
			m.resetPlayer()
			return m, m.ensureDecode()
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
		if msg.gen != m.playGen {
			return m, nil // stale watcher from a killed playback
		}
		if m.playing {
			m.pPos += time.Since(m.playStart).Seconds()
			if m.pDur > 0 && m.pPos >= m.pDur-0.5 {
				m.pPos = 0 // natural end: rewind
			}
			m.playing = false
		}
		m.playCmd = nil
		return m, nil

	case waveReadyMsg:
		m.pDecoding = false
		if msg.err == nil && msg.file == m.file {
			m.pWave = msg.cols
			m.pDur = msg.dur
			m.pReady = true
		}
		return m, nil

	case playTickMsg:
		if m.playing && m.scr == screenDone {
			return m, playTick()
		}
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

// applyTick folds one 50 ms meter tick into the live-view state.
func (m *model) applyTick(t MeterTick) {
	col := waveCol{rms: t.RMS, peak: t.Peak, rmsR: t.RMSR, peakR: t.PeakR, clip: t.Clip}
	if m.sysCap != nil {
		// stereo lanes become mic (top) and system audio (bottom)
		col.rmsR = m.sysLevel.RMS
		col.peakR = m.sysLevel.Peak
		col.clip = col.clip || m.sysLevel.Clip
		if m.sysLevel.DB > t.DB {
			t.DB = m.sysLevel.DB
		}
	}
	if m.rec != nil && m.rec.Paused() {
		col.paused = true
		col.clip = false
	}
	m.wave = append(m.wave, col)
	if len(m.wave) > 16384 {
		m.wave = m.wave[len(m.wave)-8192:]
	}
	m.rmaxdb -= 10.0 / tickHz // decays 10 dB/s
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
		m.vuLevel *= 0.905 // ~same release curve as the old 20 Hz 0.82
	}
	if col.clip {
		m.clips++
		m.clipTicks = 2 * tickHz
	} else if m.clipTicks > 0 {
		m.clipTicks--
	}
	if m.diskTick++; m.diskTick >= 5*tickHz { // every ~5 s
		m.diskTick = 0
		m.diskFree = freeBytes(m.cfg.OutDir)
	}
}

// reviveMeters restarts a dead metering stream so the display keeps
// moving (device change); capture recovery is recErrMsg's job.
func (m *model) reviveMeters() {
	if m.meter != nil {
		if soxPath, err := findBin("sox"); err == nil {
			if nm, err := startMeter(soxPath, m.cfg.Device, m.cfg.Channels); err == nil {
				m.meter = nm
			}
		}
		return
	}
	if m.rec != nil {
		m.rec.reviveMeter()
	}
}

// teardown stops every live process. Recordings in flight are finalised;
// an untriggered armed buffer is discarded (that is its contract).
func (m *model) teardown() {
	if m.captioner != nil {
		m.captioner.Stop()
		m.captioner = nil
	}
	if m.sysCap != nil {
		m.sysCap.Stop()
		m.sysCap = nil
	}
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
	m.resetPlayer()
	return m, m.ensureDecode()
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
	case fieldSysAudio:
		m.cfg.SystemAudio = !m.cfg.SystemAudio
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
	case fieldCaptions:
		m.cfg.LiveCaptions = !m.cfg.LiveCaptions
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
	case fieldRetention:
		i := 0
		for j, r := range retentionChoices {
			if r == m.cfg.RetentionDays {
				i = j
			}
		}
		m.cfg.RetentionDays = retentionChoices[(i+dir+len(retentionChoices))%len(retentionChoices)]
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
	ts := time.Now().Format("20060102-150405")
	m.file = filepath.Join(m.cfg.OutDir, fmt.Sprintf("%s-%s.flac", m.name, ts))

	useSys := m.cfg.SystemAudio && m.cfg.Mode == "record"
	micTarget := m.file
	channels := m.cfg.Channels
	if useSys {
		// mic goes to a temp mono track, merged with the system track at
		// stop (mic = left, system = right)
		_ = os.MkdirAll(sessionsDir(), 0o755)
		m.micFile = filepath.Join(sessionsDir(), "mic-"+ts+".flac")
		micTarget = m.micFile
		channels = 1
	}

	rec, err := NewRecorder(m.cfg.Device, micTarget, channels)
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

	m.frameGen++
	cmds := []tea.Cmd{frameTick(m.frameGen), waitRecErr(rec.Err)}
	if m.cfg.LiveCaptions {
		if cap, err := startCaptioner(m.cfg.Device); err != nil {
			m.notice = "live captions unavailable: " + err.Error()
		} else {
			m.captioner = cap
			m.captions = nil
			cmds = append(cmds, waitCaption(cap.Lines))
		}
	}
	if useSys {
		sys, err := startSysCapture(filepath.Join(sessionsDir(), "sys-"+ts+".flac"))
		if err != nil {
			m.notice = "system audio unavailable: " + err.Error() + " — recording mic only"
			m.micFile = ""
			// mic is already recording to the temp path; retarget is not
			// possible mid-flight, so remember to rename at stop
			m.micFile = micTarget
		} else {
			m.sysCap = sys
			cmds = append(cmds, waitSysReady(sys)) // levels are pulled by the frame clock
		}
	}
	return m, tea.Batch(cmds...)
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
	m.frameGen++
	return m, frameTick(m.frameGen)
}

// --- armed ---

func (m model) updateArmedKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "f":
		m.showPerf = !m.showPerf
		return m, nil
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
	case "f":
		m.showPerf = !m.showPerf
		return m, nil
	case "p", " ":
		if m.sysCap != nil {
			m.notice = "pause is not available while capturing system audio"
			return m, nil
		}
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
		m.resetPlayer()
		return m, m.ensureDecode()
	}
	return m, nil
}

// finishCapture stops whichever capture path is live and assembles m.file.
func (m *model) finishCapture() error {
	if m.captioner != nil {
		m.captioner.Stop()
		m.captioner = nil
	}
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
		err := m.rec.Stop()
		switch {
		case m.sysCap != nil:
			m.sysCap.Stop()
			if err == nil {
				err = mergeMicSystem(m.micFile, m.sysCap.File, m.file)
			}
			_ = os.Remove(m.micFile)
			_ = os.Remove(m.sysCap.File)
			m.sysCap = nil
			m.micFile = ""
		case m.micFile != "":
			// system audio fell over mid-flight; keep the mic track
			if err == nil {
				err = os.Rename(m.micFile, m.file)
			}
			m.micFile = ""
		}
		return err
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
	m.resetPlayer()
	if m.didTrans && !m.postRan && m.cfg.PostCommand != "" {
		m.postRan = true
		m.postStatus = "post-hook: running..."
		return m, tea.Batch(m.ensureDecode(),
			runPostHook(m.cfg.PostCommand, m.file, m.txDir, m.transcriptMD, m.markersFile))
	}
	return m, m.ensureDecode()
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

// openLibEntry populates the done screen for a library recording.
func (m *model) openLibEntry(e libEntry) {
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
	m.resetPlayer()
	m.markers = loadMarkersFromFile(e.Path)
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

func (m model) updateLibraryKeys(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// typing a search query
	if m.libSearching {
		switch msg.String() {
		case "esc":
			m.libSearching = false
			m.searchInput.Blur()
			return m, nil
		case "enter":
			m.libSearching = false
			m.searchInput.Blur()
			m.hits = searchTranscripts(m.lib, m.searchInput.Value())
			m.hitCursor = 0
			m.showHits = true
			return m, nil
		}
		var cmd tea.Cmd
		m.searchInput, cmd = m.searchInput.Update(msg)
		return m, cmd
	}
	// navigating search results
	if m.showHits {
		switch msg.String() {
		case "esc", "q":
			m.showHits = false
		case "up", "k":
			if m.hitCursor > 0 {
				m.hitCursor--
			}
		case "down", "j":
			if m.hitCursor < len(m.hits)-1 {
				m.hitCursor++
			}
		case "/":
			m.libSearching = true
			m.searchInput.Focus()
			return m, textinput.Blink
		case "enter":
			if len(m.hits) == 0 {
				return m, nil
			}
			h := m.hits[m.hitCursor]
			for _, e := range m.lib {
				if e.Path == h.Audio {
					m.openLibEntry(e)
					// preview centres on the hit rather than the top
					if ctx := hitContext(h, 2); len(ctx) > 0 {
						m.preview = ctx
					}
					return m, m.ensureDecode()
				}
			}
		}
		return m, nil
	}
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
	case "/":
		m.libSearching = true
		m.searchInput.SetValue("")
		m.searchInput.Focus()
		return m, textinput.Blink
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
		if len(m.lib) > 0 {
			m.openLibEntry(m.lib[m.libCursor])
			return m, m.ensureDecode()
		}
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
	case "p", " ":
		if m.playing {
			m.stopPlayback()
			return m, nil
		}
		return m, tea.Batch(m.beginPlayback(), m.ensureDecode())
	case "left", "h":
		return m, m.seekTo(m.curPos() - 5)
	case "right", "l":
		return m, m.seekTo(m.curPos() + 5)
	case "[":
		cur := m.curPos()
		target := 0.0
		for _, mk := range m.markers {
			s := mk.Seconds()
			if s < cur-1 && s > target {
				target = s
			}
		}
		return m, m.seekTo(target)
	case "]":
		cur := m.curPos()
		for _, mk := range m.markers {
			if s := mk.Seconds(); s > cur+0.5 {
				return m, m.seekTo(s)
			}
		}
		return m, nil
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
	if m.playing {
		m.pPos += time.Since(m.playStart).Seconds()
		if m.pDur > 0 && m.pPos > m.pDur {
			m.pPos = m.pDur
		}
	}
	m.playing = false
	m.playGen++ // orphan any pending waitPlay watcher
	if m.playCmd != nil && m.playCmd.Process != nil {
		_ = m.playCmd.Process.Kill()
	}
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
