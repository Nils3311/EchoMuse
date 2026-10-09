package shadow

// Pryon engine: the device's own libpryon.so, run in the pryon_wake helper
// process (device/tools/pryon_wake), behind the same Scorer as openWakeWord.
//
// The helper is a separate process for one reason: libpryon.so is Amazon's,
// reverse engineered, with its own threads and C++ exceptions, and a crash in
// it must cost a restart of the helper, not the server — and with the server,
// the microphone and the dashboard terminal. Talking to it is protocol v1,
// documented at the top of pryon_wake.c: binary frames on stdin, text lines on
// stdout, library log lines on stderr.
//
// The Scorer's rule holds here unchanged: nothing may block the audio path.
// There are three buffers between the microphone and the decoder and none of
// them can push back on the mic goroutine:
//
//  1. mic -> scorer: the Scorer's 8-frame queue, drops counted (shadow.go).
//  2. scorer -> helper: Push encodes the frame and offers it to a writer
//     goroutine with a NON-BLOCKING send into an 8-frame channel. Full means
//     the helper is behind: the frame is refused with errBehind, which the
//     Scorer counts under Errors. Only the writer goroutine ever blocks on the
//     pipe, and Close can always get rid of it by closing the pipe and, if
//     that is not enough, killing the process.
//  3. inside the helper: the library decodes on its own thread and buffers.
//
// Because Push never waits, the Scorer's own Drops and MaxInferMs say nothing
// about Pryon being overloaded: Drops stays at zero and MaxInferMs measures a
// channel send. Overload shows up as drops= (errBehind) and stale= in the
// helper's 30s log line instead, and as the end->W latency on that line.
//
// Detections arrive asynchronously, some hundreds of milliseconds after the
// audio they are about. They are turned back into the capture time of a frame
// the mic actually stamped (see wakeAt), held for two frames so the barge-in
// two-frame rule can see them, and thrown away when a Reset or their age says
// they no longer describe the stream.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// pryonFrameSamples is the scorer's frame, 80ms at 16kHz: the unit of the
	// ring, of the at-offset and of the hold.
	pryonFrameSamples = 1280
	// pryonRingFrames is how far back a detection can be mapped to a capture
	// time: 64 frames, 5.12s. Anything older is stale by a wide margin anyway
	// (pryonStaleAfter), so the ring only has to be long enough never to be
	// the reason a fresh detection is lost.
	pryonRingFrames = 64
	// pryonWriterFrames bounds the frames in flight to the writer goroutine:
	// 640ms, like the Scorer's queue. Kept short on purpose: the
	// listen gate rings only 2s, and a frame queued here, then in the pipe,
	// then in the decoder, must still be in that ring when its detection
	// comes back.
	pryonWriterFrames = 8
	// pryonEventBuffer is detections and status lines waiting for the scorer
	// goroutine. A wake is at most a line every second or so; 64 is never
	// reached unless the scorer goroutine is gone, and then a lost line is
	// counted rather than blocking the reader (which would block the helper's
	// stdout and with it the library's decoder thread).
	pryonEventBuffer = 64
	// pryonHoldFrames is how many scored frames an accepted wake is reported
	// for. Two, because the Scorer's barge-in bar needs two consecutive frames
	// above it (shadow.go, run); at the normal bar the refractory period
	// suppresses the second one.
	pryonHoldFrames = 2
	// pryonStaleAfter: a detection whose capture time is older than this when
	// the scorer goroutine sees it is dropped. The listen gate
	// rings 2s of audio and a session starts with the frames AFTER `at`, so a
	// much older `at` would open a session on whatever is left, and the
	// arbitration age would be wrong by the same amount. 1.2s leaves room for
	// the scorer queue on top. Note `at` is already ~240ms before the word end
	// (wakeAt), so the decoder's own latency budget is about 0.9s.
	pryonStaleAfter = 1200 * time.Millisecond
	// pryonReadyTimeout: a helper that has not said READY this long after it
	// started is killed. Measured: model load plus warm-up takes about 1s.
	pryonReadyTimeout = 5 * time.Second
	// pryonFirstReadyTimeout replaces pryonReadyTimeout until a helper has
	// been ready once (EM_PRYON_READY_TIMEOUT_MS). Missing it is the one READY
	// timeout that falls back to openWakeWord at once, so it must not be
	// judged on a cold boot, when dlopen of /system/lib, the model load and
	// the warm-up compete with everything else starting up.
	pryonFirstReadyTimeout = 20 * time.Second
	// pryonStuckAfter: a ready helper that has refused every frame this long
	// (errBehind, i.e. the writer channel stayed full) is killed and counted
	// as a crash. It has stopped reading stdin — a wedged decoder thread
	// blocking PushSamples, or a stopped process — and would otherwise neither
	// exit nor miss a READY: the device would be deaf while reporting itself
	// ready, indefinitely. 2s is 25 frames, far beyond any scheduling hiccup
	// the 640ms writer channel is there to absorb.
	pryonStuckAfter = 2 * time.Second
	// pryonLongRun: a helper that ran at least this long before dying is
	// treated as having worked; its exit starts a fresh crash series.
	pryonLongRun = 60 * time.Second
	// pryonMaxCrashes in a row, each within pryonLongRun of starting, and the
	// engine gives up on Pryon until the model changes or the server restarts.
	pryonMaxCrashes = 5
	// pryonStatsEvery is the period of the "[pryon] cpu=..." log line.
	pryonStatsEvery = 30 * time.Second
	// pryonStderrPerMinute caps how many of the library's log lines reach the
	// server log. stderr is always READ in full — a helper blocked writing to
	// a full stderr pipe would stall the library's decoder thread, which logs
	// from there — only the logging is limited.
	pryonStderrPerMinute = 20
	// pryonClockTicks is USER_HZ, the unit of utime/stime in /proc/<pid>/stat.
	// 100 on every Linux/Android ARM build this runs on.
	pryonClockTicks = 100
	// DefaultPryonAtOffsetFrames: see wakeAt.
	DefaultPryonAtOffsetFrames = 3
)

// pryonBackoff is the delay before restarting a helper that exited, indexed by
// how many restarts there have been since the last helper that ran for
// pryonLongRun.
var pryonBackoff = []time.Duration{
	1 * time.Second, 2 * time.Second, 4 * time.Second,
	8 * time.Second, 16 * time.Second, 30 * time.Second,
}

// pryonFatalCodes are the E codes that will not get better by restarting: the
// library, a symbol or the model is missing, the decoder cannot be created, or
// the library's structure layout is not the one pryon_api.h describes. IO is
// not among them: it means a broken frame or a vanished server, both of which
// a fresh helper does not inherit.
var pryonFatalCodes = map[string]bool{"LIB": true, "SYM": true, "MODEL": true, "DECODER": true, "ABI": true}

// procRoot is /proc; a variable so tests can point it at a fake tree.
var procRoot = "/proc"

var (
	// errBehind: the writer channel was full, the frame was not sent.
	errBehind = errors.New("pryon: helper is behind, frame not sent")
	// errHelperDown: the helper exited and is waiting out its restart backoff.
	errHelperDown = errors.New("pryon: helper not running")
)

// pryonConfig is how to run the helper. See pryon_open.go for where each value
// comes from.
type pryonConfig struct {
	Helper   string // pryon_wake binary
	Lib      string // libpryon.so
	Manifest string // the keyword model's pryon.manifest
	WarmupMs int    // -w: noise fed before the first real audio
	// ResetMode is -r: 0 ignores a Reset inside the helper (the decoder runs
	// on across the gap), 1 ends the decoder session and warms up again.
	ResetMode int
	// AtOffsetFrames: see wakeAt.
	AtOffsetFrames int
	// FirstReadyMs: see pryonFirstReadyTimeout. 0 means the default.
	FirstReadyMs int
}

// pryonEvent is one stdout line, stamped when it was READ — not when the
// scorer goroutine got round to it, which can be minutes later while muted.
type pryonEvent struct {
	kind       byte // 'R' READY, 'W' wake, 'N' near miss, 'E' fatal error
	keyword    string
	begin, end uint64
	score      float32 // -1: the helper did not see the library's score
	code, text string
	arrived    time.Time
}

// parsePryonLine reads one protocol v1 stdout line.
func parsePryonLine(line string) (pryonEvent, bool) {
	f := strings.Fields(line)
	if len(f) == 0 {
		return pryonEvent{}, false
	}
	num := func(s string) (uint64, bool) {
		v, err := strconv.ParseUint(s, 10, 64)
		return v, err == nil
	}
	score := func(s string) (float32, bool) {
		v, err := strconv.ParseFloat(s, 32)
		return float32(v), err == nil
	}
	switch f[0] {
	case "READY":
		return pryonEvent{kind: 'R', text: strings.TrimSpace(strings.TrimPrefix(line, "READY"))}, true
	case "W": // W <seq> <keyword> <type> <begin> <end> <score>
		if len(f) != 7 {
			return pryonEvent{}, false
		}
		b, ok1 := num(f[4])
		e, ok2 := num(f[5])
		s, ok3 := score(f[6])
		if !ok1 || !ok2 || !ok3 {
			return pryonEvent{}, false
		}
		return pryonEvent{kind: 'W', keyword: f[2], begin: b, end: e, score: s}, true
	case "N": // N <keyword> <begin> <end> <score>
		if len(f) != 5 {
			return pryonEvent{}, false
		}
		b, ok1 := num(f[2])
		e, ok2 := num(f[3])
		s, ok3 := score(f[4])
		if !ok1 || !ok2 || !ok3 {
			return pryonEvent{}, false
		}
		return pryonEvent{kind: 'N', keyword: f[1], begin: b, end: e, score: s}, true
	case "E": // E <code> <text>
		if len(f) < 2 {
			return pryonEvent{}, false
		}
		text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "E"), " "+f[1]))
		return pryonEvent{kind: 'E', code: f[1], text: text}, true
	}
	return pryonEvent{}, false
}

// encodePryonFrame builds a protocol v1 stdin frame: 'P' 'W' type flags len32.
func encodePryonFrame(typ byte, payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	b[0], b[1], b[2] = 'P', 'W', typ
	n := uint32(len(payload))
	b[4], b[5], b[6], b[7] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	copy(b[8:], payload)
	return b
}

// encodePryonAudio is encodePryonFrame('A') without the intermediate copy.
func encodePryonAudio(pcm []int16) []byte {
	b := make([]byte, 8+2*len(pcm))
	b[0], b[1], b[2] = 'P', 'W', 'A'
	n := uint32(2 * len(pcm))
	b[4], b[5], b[6], b[7] = byte(n), byte(n>>8), byte(n>>16), byte(n>>24)
	for i, v := range pcm {
		b[8+2*i] = byte(uint16(v))
		b[9+2*i] = byte(uint16(v) >> 8)
	}
	return b
}

// pryonFrame is one frame handed to the helper: where it starts in the
// helper's count of real samples, how long it is, and when the mic stamped it.
type pryonFrame struct {
	start uint64
	n     uint32
	at    time.Time
}

// pryonRing remembers the last pryonRingFrames frames sent to the helper. Only
// frames that were actually SENT are in it (not ones refused with errBehind),
// so its sample positions are the helper's.
type pryonRing struct {
	buf  [pryonRingFrames]pryonFrame
	head int // next slot to write
	n    int
}

func (r *pryonRing) add(f pryonFrame) {
	r.buf[r.head] = f
	r.head = (r.head + 1) % len(r.buf)
	if r.n < len(r.buf) {
		r.n++
	}
}

func (r *pryonRing) clear() { *r = pryonRing{} }

// lookup returns the frame holding sample pos. A position before the oldest
// frame is not in the ring (ok=false): that audio has been forgotten. One past
// the newest frame maps to the newest, because a keyword end may be reported
// as one past the last sample of the word.
func (r *pryonRing) lookup(pos uint64) (pryonFrame, bool) {
	if r.n == 0 {
		return pryonFrame{}, false
	}
	oldest := r.buf[(r.head-r.n+len(r.buf))%len(r.buf)]
	if pos < oldest.start {
		return pryonFrame{}, false
	}
	for i := 1; i <= r.n; i++ {
		f := r.buf[(r.head-i+len(r.buf))%len(r.buf)]
		if f.start <= pos {
			return f, true
		}
	}
	return oldest, true
}

// wakeAt maps a keyword end, in the helper's real-sample count, to the capture
// time the Scorer reports for the crossing. The rule is:
//
//	at = the capture time of the frame holding sample (end - offset*1280),
//	     offset = 3 by default (EM_PRYON_AT_OFFSET_FRAMES), never interpolated.
//
// Why that frame. The time a crossing reports decides where a private-listening
// session starts: listen.Gate.Open sends every ringed frame captured strictly
// AFTER `at` (listen.go), so the session begins with the frame after the one
// chosen here. The controller then drops the first VOICE_PREROLL_DISCARD = 3
// frames (240ms) of every wake-word session before STT (em_esphome.py,
// em_controller.py: preroll_discard), a value tuned for openWakeWord, whose
// score rises only after the word has ended. Let E be the frame holding the
// keyword end. Choosing E-3 here gives:
//
//	gate sends from E-2, controller drops E-2, E-1, E, STT hears from E+1.
//
// So STT starts with the first frame wholly after the one the word ended in:
// none of "Alexa" reaches it, and at most the 0-80ms remainder of frame E after
// the end is lost — normally the pause before the command. Choosing E itself,
// as the plan first had it, would lose E+1..E+3 as well, 240ms more: the "wie"
// of "Alexa, wie spät ist es" when spoken without a pause. openWakeWord, for
// comparison, crosses at some frame C >= E and STT hears from C+4.
//
// Why not interpolate within the frame (the plan's at + (end-start)/16ms). The
// mic reads 160ms ALSA batches and stamps both 80ms frames of a batch with
// time.Now() in one loop (client/data.go), so they carry times microseconds
// apart, not 80ms. An interpolated time would land after the NEXT frame's
// stamp, and the gate would skip that frame too. The ring therefore returns
// exactly a time the mic gave a frame — the same thing openWakeWord reports.
//
// Edges, deliberately not corrected: whether Pryon's end is the last sample of
// the word or one past it is not documented (the stub reports one past); at a
// frame boundary that moves E by one frame, i.e. up to 80ms of the word's tail
// or of the pause. A frame the Scorer's queue dropped between E-3 and E never
// reached the helper but is in the gate's ring, which moves the STT start one
// frame EARLIER per dropped frame: a rare "...xa" at the start of the
// transcript, never a lost command word. And `at` is ~240ms earlier than the
// moment the word was heard, so in arbitration ("first to hear wins") a Pryon
// Echo has that much head start over an openWakeWord one.
func wakeAt(r *pryonRing, end uint64, offsetFrames int) (pryonFrame, bool) {
	back := uint64(offsetFrames) * pryonFrameSamples
	target := uint64(0)
	if end > back {
		target = end - back
	}
	return r.lookup(target)
}

// lineLimiter lets through at most perMinute lines a minute and counts the
// rest. Shared by the stderr readers of successive helpers, hence the lock.
type lineLimiter struct {
	mu         sync.Mutex
	perMinute  int
	start      time.Time
	n          int
	suppressed int
}

// allow reports whether this line may be logged, and how many were suppressed
// in the window that just ended (to be logged once, when nonzero).
func (l *lineLimiter) allow(now time.Time) (ok bool, suppressed int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.start.IsZero() || now.Sub(l.start) >= time.Minute {
		suppressed, l.suppressed = l.suppressed, 0
		l.start, l.n = now, 0
	}
	if l.n < l.perMinute {
		l.n++
		return true, suppressed
	}
	l.suppressed++
	return false, suppressed
}

// pryonProc is one run of the helper. A restart makes a new one, so nothing
// from a dead helper — a queued frame, a late stdout line — can leak into the
// next: its channels die with it.
type pryonProc struct {
	cmd     *exec.Cmd
	pid     int
	started time.Time
	stdin   *os.File
	frames  chan []byte
	events  chan pryonEvent
	quit    chan struct{}
	quitter sync.Once
	// Closed when the respective goroutine has finished. Close waits for all of
	// them, with a deadline, so a test can prove nothing is left running.
	writerDone, stdoutDone, stderrDone, exited chan struct{}
	// Set by the waiter before it closes exited.
	exitErr error
	exitAt  time.Time
	// lost counts stdout lines dropped because events was full.
	lost atomic.Uint64
	// stdoutErr is set by the stdout reader when its scanner gives up (a line
	// over 64KiB). Everything after that is drained unread, so the helper
	// can never report a wake again; pump kills it as a crash.
	stdoutErr atomic.Pointer[string]

	// Scorer goroutine only.
	ready bool
	// behindSince is when the current run of errBehind began; zero while
	// frames are being accepted.
	behindSince time.Time
	fatal       string // "CODE text" of a fatal E line
	lastE       string
	killedByUs  bool
	killReason  string
}

func (p *pryonProc) stop() { p.quitter.Do(func() { close(p.quit) }) }

// pryonEngine is the engine for Pryon. All methods except Info run on the
// scorer goroutine (Close after it has exited), so its state needs no lock;
// the goroutines it starts talk to it only through a pryonProc's channels and
// through fb (the fallback) and info, which are locked.
type pryonEngine struct {
	cfg  pryonConfig
	stem string
	now  func() time.Time
	logf func(format string, args ...any)
	// Real-time bounds on Close; fields so tests can shorten them.
	closeGrace time.Duration // after closing stdin, before SIGKILL
	killWait   time.Duration // after SIGKILL, for the process and its readers
	staleAfter time.Duration
	readyAfter time.Duration
	// firstReadyAfter applies instead of readyAfter until any helper has
	// been ready.
	firstReadyAfter time.Duration
	stderr          lineLimiter
	stdoutJunk      lineLimiter
	// badScoreLog limits the log line about W lines with an unusable score.
	badScoreLog lineLimiter

	p           *pryonProc
	everReady   bool
	nextStart   time.Time
	backoffIdx  int
	crashStreak int
	lastErr     string
	// downReported: the first frame after a helper exit returns errHelperDown,
	// so the Scorer's LastErr says why; later frames until the restart just
	// count as not ready instead of inflating Errors by 12 a second.
	downReported bool

	ring     pryonRing
	fed      uint64 // real samples sent to the current helper
	resetFed uint64 // fed at the last Reset: a wake ending before it is stale
	lastAt   time.Time
	hold     struct {
		score float32
		at    time.Time
		left  int
	}

	// Sticky requests to the helper: retried on every frame until a send
	// succeeds, so a full writer channel delays them rather than losing them.
	playback     bool
	propPending  bool
	resetPending bool

	// Failover to openWakeWord. fallback builds it — off the scorer goroutine,
	// because it loads ONNX Runtime (12MB) and that would cost seconds of
	// frames. inner is set once it is there.
	fallback func() (engine, error)
	// noFallback: never fail over. A helper that keeps dying is restarted
	// forever on the backoff (capped at its last step), and one that reports
	// a fatal error or never comes up is treated as a crash like any other —
	// the device stays on Pryon or hears nothing, and says so, instead of
	// quietly scoring with openWakeWord. Set by openWithPryon unless
	// EM_PRYON_FALLBACK=oww.
	noFallback bool
	failing    bool
	failReason string
	fbDone     chan struct{}
	fb         struct {
		sync.Mutex
		closed bool
		ready  bool
		eng    engine
		err    error
	}
	inner engine
	fbErr error

	infoMu sync.Mutex
	info   string

	// The 30s log line. win is per window; restarts is since the engine began.
	restarts  uint64
	lastStats time.Time
	win       struct {
		accepts, nearMiss, drops, stale, foreign, badScore uint64
		lat                                                []time.Duration
	}
	cpuPid   int
	cpuTicks uint64
	cpuAt    time.Time

	// onSpawn, when set, sees every helper started. Tests use it to prove
	// that each one, and every goroutine serving it, is gone after Close.
	onSpawn func(*pryonProc)

	closeOnce sync.Once
}

// newPryonEngine makes an engine that has not started a helper yet.
func newPryonEngine(stem string, cfg pryonConfig, fallback func() (engine, error)) *pryonEngine {
	e := &pryonEngine{
		cfg:        cfg,
		stem:       stem,
		now:        time.Now,
		logf:       log.Printf,
		closeGrace: 500 * time.Millisecond,
		killWait:   time.Second,
		staleAfter: pryonStaleAfter,
		readyAfter: pryonReadyTimeout,
		fallback:   fallback,
	}
	e.firstReadyAfter = pryonFirstReadyTimeout
	if cfg.FirstReadyMs > 0 {
		e.firstReadyAfter = time.Duration(cfg.FirstReadyMs) * time.Millisecond
	}
	e.stderr.perMinute = pryonStderrPerMinute
	e.stdoutJunk.perMinute = 5
	e.badScoreLog.perMinute = 5
	e.setInfo(fmt.Sprintf("pryon, model %s, manifest %s, warm-up %dms, reset R%d, at-offset %d frames",
		stem, cfg.Manifest, cfg.WarmupMs, cfg.ResetMode, cfg.AtOffsetFrames))
	return e
}

func (e *pryonEngine) setInfo(s string) {
	e.infoMu.Lock()
	e.info = s
	e.infoMu.Unlock()
}

// Info describes the engine; after a failover, the openWakeWord engine and why.
func (e *pryonEngine) Info() string {
	e.infoMu.Lock()
	defer e.infoMu.Unlock()
	return e.info
}

// spawn starts one helper and the four goroutines that serve it.
func (e *pryonEngine) spawn(now time.Time) (*pryonProc, error) {
	args := []string{
		"-l", e.cfg.Lib, "-m", e.cfg.Manifest, "-k", "ALEXA",
		"-w", strconv.Itoa(e.cfg.WarmupMs), "-r", strconv.Itoa(e.cfg.ResetMode),
	}
	cmd := exec.Command(e.cfg.Helper, args...)
	cmd.SysProcAttr = pryonSysProcAttr()

	// Plain os.Pipe rather than cmd.*Pipe: Wait must not depend on the readers
	// having finished, and the readers must not depend on Wait. These pipes
	// are close-on-exec, and the child is GIVEN only its three ends.
	//
	// That is not all it gets. exec closes only descriptors marked
	// FD_CLOEXEC; every other one the server holds survives into the child.
	// The microphone's PCM descriptor is such a one (opened by tinyalsa's C
	// open() without O_CLOEXEC; the dashboard terminal's shells inherit it
	// too), and pcm_open blocks while any process holds the PCM. The only
	// protection is pryon_wake's first statement, close_inherited_fds()
	// (TestHelperInheritsNonCloexecFds pins the inheritance,
	// TestRealHelperClosesInheritedFds the cleanup). Its known gaps:
	//   - it closes fds 3..1023 only. The Go runtime raises the soft
	//     RLIMIT_NOFILE at start, so a server fd above 1023 is possible in
	//     principle; the PCM is opened early and gets a low number.
	//   - between exec and that statement the dynamic loader and libc
	//     initialisation run with the descriptor held: microseconds, and a
	//     server that dies in them takes the helper with it (Pdeathsig).
	//   - EM_PRYON_HELPER pointing at a wrapper script defeats it: the shell
	//     holds the PCM for as long as it lives.
	// The real fix is FD_CLOEXEC on the PCM descriptor in bindings/mic, which
	// would also stop the terminal's shells inheriting it.
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, err
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{inR, inW, outR, outW, errR, errW} {
			f.Close()
		}
		return nil, err
	}
	// The child's ends, closed here at once: while this process held a write
	// end of stdout, the reader would never see EOF when the helper dies.
	inR.Close()
	outW.Close()
	errW.Close()

	p := &pryonProc{
		cmd:        cmd,
		pid:        cmd.Process.Pid,
		started:    now,
		stdin:      inW,
		frames:     make(chan []byte, pryonWriterFrames),
		events:     make(chan pryonEvent, pryonEventBuffer),
		quit:       make(chan struct{}),
		writerDone: make(chan struct{}),
		stdoutDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		exited:     make(chan struct{}),
	}
	if e.onSpawn != nil {
		e.onSpawn(p)
	}
	go e.writeLoop(p)
	go e.readStdout(p, outR)
	go e.readStderr(p, errR)
	go func() {
		p.exitErr = p.cmd.Wait()
		p.exitAt = e.now()
		close(p.exited)
	}()
	return p, nil
}

// writeLoop is the only code that can block on the helper's stdin. After a
// write fails (the helper is gone) it keeps draining the channel, so Push sees
// a dead helper as down rather than as behind.
func (e *pryonEngine) writeLoop(p *pryonProc) {
	defer close(p.writerDone)
	broken := false
	for {
		select {
		case <-p.quit:
			return
		case b := <-p.frames:
			if broken {
				continue
			}
			if _, err := p.stdin.Write(b); err != nil {
				broken = true
			}
		}
	}
}

func (e *pryonEngine) readStdout(p *pryonProc, r *os.File) {
	defer close(p.stdoutDone)
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		line := sc.Text()
		ev, ok := parsePryonLine(line)
		if !ok {
			if ok, _ := e.stdoutJunk.allow(e.now()); ok {
				e.logf("[pryon] unexpected helper output: %q", line)
			}
			continue
		}
		ev.arrived = e.now()
		select {
		case p.events <- ev:
		default:
			p.lost.Add(1)
		}
	}
	if err := sc.Err(); err != nil {
		msg := err.Error()
		p.stdoutErr.Store(&msg)
	}
	// A line too long for the scanner ends Scan, but the helper may still be
	// writing: keep draining so it can never block on a full stdout until it
	// has been killed.
	_, _ = io.Copy(io.Discard, r)
}

func (e *pryonEngine) readStderr(p *pryonProc, r *os.File) {
	defer close(p.stderrDone)
	defer r.Close()
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64*1024)
	for sc.Scan() {
		ok, suppressed := e.stderr.allow(e.now())
		if suppressed > 0 {
			e.logf("[pryon] (%d helper log lines suppressed)", suppressed)
		}
		if ok {
			// The helper already prefixes the library's lines; one prefix is
			// enough.
			e.logf("[pryon] %s", strings.TrimPrefix(sc.Text(), "[pryon] "))
		}
	}
	_, _ = io.Copy(io.Discard, r)
}

// waitClosed waits for ch until d has passed (real time). True if ch closed.
func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	default:
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return true
	case <-t.C:
		return false
	}
}

// killGroup SIGKILLs the helper's process group (it is its own leader, see
// pryonSysProcAttr), and the process itself in case the group is gone.
func killGroup(p *pryonProc) {
	select {
	case <-p.exited:
		return // reaped: the pid may already belong to someone else
	default:
	}
	_ = syscall.Kill(-p.pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
}

// closeProc shuts one helper down and waits for it, with bounds: stdin EOF is
// the polite way out (the helper deletes the decoder and exits 0); a helper
// that does not take it within closeGrace — wedged in the library, or never
// reading stdin — is SIGKILLed. Never waits longer than closeGrace+2*killWait,
// because this runs on config changes and those run on the control link's
// read loop.
func (e *pryonEngine) closeProc(p *pryonProc) {
	p.stop()
	// Unblocks a writer stuck on a full pipe, and is the EOF that ends the
	// helper.
	_ = p.stdin.Close()
	if !waitClosed(p.exited, e.closeGrace) {
		p.killedByUs = true
		killGroup(p)
		if !waitClosed(p.exited, e.killWait) {
			e.logf("[pryon] helper pid %d did not exit after SIGKILL", p.pid)
		}
	}
	deadline := time.Now().Add(e.killWait)
	for _, ch := range []chan struct{}{p.writerDone, p.stdoutDone, p.stderrDone} {
		if !waitClosed(ch, time.Until(deadline)) {
			e.logf("[pryon] helper pid %d: I/O goroutine still running after close", p.pid)
			return
		}
	}
}

// start launches a helper now, or records a failed launch as a crash.
func (e *pryonEngine) start(now time.Time) error {
	p, err := e.spawn(now)
	e.ring.clear()
	e.fed, e.resetFed = 0, 0
	e.hold.left = 0
	e.resetPending = false
	if err != nil {
		e.crashed(now, 0, fmt.Sprintf("did not start: %v", err))
		return err
	}
	e.p = p
	e.downReported = false
	// A new helper knows no properties; tell it again if it matters.
	e.propPending = e.playback
	return nil
}

// crashed applies the restart policy after a helper ended (or failed to
// start). Every death within pryonLongRun of the start counts towards the
// series, whoever caused it: a SIGKILL from Android's low-memory killer
// included. With EM_PRYON_FALLBACK=oww the fifth in a row gives up on Pryon
// for openWakeWord; without it (noFallback, the default) the helper is
// restarted for good on the capped backoff.
func (e *pryonEngine) crashed(at time.Time, uptime time.Duration, why string) {
	if uptime >= pryonLongRun {
		e.crashStreak, e.backoffIdx = 0, 0
	} else {
		e.crashStreak++
	}
	if e.crashStreak >= pryonMaxCrashes && !e.noFallback {
		e.failover(fmt.Sprintf("%d crashes in a row, last: %s", e.crashStreak, why))
		return
	}
	if e.crashStreak == pryonMaxCrashes && e.noFallback {
		// Once per series: the point where the old behaviour gave up. Said
		// plainly, because without a fallback this Echo now hears nothing.
		e.logf("[pryon] WARNING: %d crashes in a row (last: %s) — this Echo hears no wake word "+
			"until the helper runs again; retrying every %s, no openWakeWord fallback",
			e.crashStreak, why, pryonBackoff[len(pryonBackoff)-1])
	}
	delay := pryonBackoff[min(e.backoffIdx, len(pryonBackoff)-1)]
	e.backoffIdx++
	e.nextStart = at.Add(delay)
	e.restarts++
	e.lastErr = fmt.Sprintf("helper %s; restart in %s", why, delay)
	e.downReported = false
	e.logf("[pryon] %s", e.lastErr)
}

// pump brings the engine up to date: reads what the helper said, notices that
// it exited or never became ready, restarts it when due, and writes the
// periodic log line. Runs at the top of every Push, i.e. once per frame.
//
// Order matters, and so does using the times the reader and waiter stamped
// rather than now: pump does not run while the mic is stopped (mute, a button
// turn), and a READY that arrived three seconds after the start must not be
// judged as missing because nobody looked for ten minutes.
func (e *pryonEngine) pump() {
	now := e.now()
	if p := e.p; p != nil {
		e.drain(p, false, now)
		select {
		case <-p.exited:
			// The final E line may still be on its way through the reader,
			// which ends at EOF — immediately, now the helper is gone.
			waitClosed(p.stdoutDone, 200*time.Millisecond)
			e.drain(p, false, now)
			e.handleExit(p)
		default:
			if msg := p.stdoutErr.Load(); msg != nil && !p.killedByUs {
				// Logged once, by crashed(), through the kill reason.
				p.killedByUs = true
				p.killReason = "stdout unreadable (" + *msg + ")"
				killGroup(p)
			}
			limit := e.readyAfter
			if !e.everReady {
				limit = e.firstReadyAfter
			}
			if !p.ready && !p.killedByUs && now.Sub(p.started) >= limit {
				p.killedByUs = true
				p.killReason = fmt.Sprintf("no READY within %s", limit)
				killGroup(p) // the exit is handled on a later frame
			}
		}
	}
	if e.p == nil && !e.failing && !now.Before(e.nextStart) {
		_ = e.start(now)
	}
	e.maybeStats(now)
}

// handleExit handles the end of the current helper.
func (e *pryonEngine) handleExit(p *pryonProc) {
	e.closeProc(p) // already gone: just collects its goroutines
	e.p = nil
	e.hold.left = 0
	uptime := p.exitAt.Sub(p.started)
	why := describeExit(p)
	switch {
	case e.noFallback:
		// No openWakeWord to fall back to: every ending is a crash, retried
		// on the backoff. The reason stays in lastErr and the log.
		if p.fatal != "" {
			why = "reported " + p.fatal
		}
		e.crashed(p.exitAt, uptime, why)
	case p.fatal != "":
		e.failover("helper reported " + p.fatal)
	case p.killReason != "" && !e.everReady:
		// The first helper never came up: on a device that cannot run
		// Pryon there is no point in five more tries.
		e.failover(p.killReason)
	default:
		e.crashed(p.exitAt, uptime, why)
	}
}

func describeExit(p *pryonProc) string {
	var b strings.Builder
	switch {
	case p.killReason != "":
		b.WriteString(p.killReason)
	case p.exitErr == nil:
		b.WriteString("exited 0 unasked")
	default:
		b.WriteString(p.exitErr.Error())
	}
	if p.lastE != "" {
		fmt.Fprintf(&b, " (E %s)", p.lastE)
	}
	return b.String()
}

// drain takes everything the helper has said so far. discard drops wakes and
// near misses (a Reset); status lines are always applied.
func (e *pryonEngine) drain(p *pryonProc, discard bool, now time.Time) {
	for {
		var ev pryonEvent
		select {
		case ev = <-p.events:
		default:
			return
		}
		switch ev.kind {
		case 'R':
			p.ready = true
			e.everReady = true
			e.logf("[pryon] helper pid %d ready after %dms: %s", p.pid,
				ev.arrived.Sub(p.started).Milliseconds(), ev.text)
		case 'E':
			p.lastE = strings.TrimSpace(ev.code + " " + ev.text)
			if pryonFatalCodes[ev.code] {
				p.fatal = p.lastE
			}
			e.logf("[pryon] helper error: %s", p.lastE)
		case 'N':
			if !discard {
				e.win.nearMiss++
			}
		case 'W':
			if discard {
				e.win.stale++
				continue
			}
			e.wake(ev, now)
		}
	}
}

// wake turns an accepted detection into a held score, or drops it as stale.
func (e *pryonEngine) wake(ev pryonEvent, now time.Time) {
	if !strings.EqualFold(ev.keyword, "ALEXA") {
		// The helper filters with -k ALEXA; this is belt and braces against a
		// different helper build, because STOP from a television must never
		// open a session.
		e.win.foreign++
		return
	}
	e.win.accepts++
	if ev.end <= e.resetFed {
		// Heard before the last Reset: about audio from before the gap.
		e.win.stale++
		e.logf("[pryon] wake from before the last reset dropped (end=%d, reset at %d)", ev.end, e.resetFed)
		return
	}
	if fe, ok := e.ring.lookup(ev.end); ok {
		e.win.lat = append(e.win.lat, ev.arrived.Sub(fe.at))
	}
	f, ok := wakeAt(&e.ring, ev.end, e.cfg.AtOffsetFrames)
	if !ok {
		e.win.stale++
		e.logf("[pryon] wake at sample %d is older than the ring, dropped", ev.end)
		return
	}
	if age := now.Sub(f.at); age > e.staleAfter {
		e.win.stale++
		e.logf("[pryon] wake %dms old when seen, dropped (limit %dms)",
			age.Milliseconds(), e.staleAfter.Milliseconds())
		return
	}
	score := ev.score
	// Written as "not inside" so NaN, which compares false with everything,
	// lands here too: passed on, it would silently never cross.
	if !(score >= 0 && score <= 1) {
		// -1: the library's score line was not seen. Anything else here (NaN,
		// Inf, out of range) is a score the helper could not read. Either
		// way the wake was accepted by Pryon's own threshold, so it is
		// reported as certain.
		if score != -1 {
			e.win.badScore++
			if ok, _ := e.badScoreLog.allow(now); ok {
				e.logf("[pryon] wake with an unusable score %v, reported as 1", score)
			}
		}
		score = 1
	}
	e.hold.score, e.hold.at, e.hold.left = score, f.at, pryonHoldFrames
	e.logf("[pryon] wake score=%.3f end=%d at=-%dms", score, ev.end, now.Sub(f.at).Milliseconds())
}

// failover gives up on Pryon and starts building the openWakeWord engine in
// the background. Logged once: it happens at most once per engine.
func (e *pryonEngine) failover(reason string) {
	if e.failing {
		return
	}
	if p := e.p; p != nil {
		e.p = nil
		e.closeProc(p)
	}
	e.failing = true
	e.failReason = reason
	e.hold.left = 0
	e.logf("[pryon] giving up on Pryon (%s); falling back to openWakeWord with %s.onnx "+
		"until the model changes or the server restarts", reason, e.stem)
	e.setInfo(fmt.Sprintf("pryon failed (%s), loading openWakeWord", reason))
	done := make(chan struct{})
	e.fbDone = done
	fallback := e.fallback
	go func() {
		defer close(done)
		var eng engine
		var err error
		if fallback == nil {
			err = errors.New("no fallback")
		} else {
			eng, err = fallback()
		}
		e.fb.Lock()
		if e.fb.closed {
			// Closed while loading: nobody will take it, so release it here
			// or the ORT sessions leak.
			e.fb.Unlock()
			if eng != nil {
				_ = eng.Close()
			}
			return
		}
		e.fb.ready, e.fb.eng, e.fb.err = true, eng, err
		e.fb.Unlock()
	}()
}

// active returns the fallback engine once it is in place, picking it up when
// the background load has finished.
func (e *pryonEngine) active() engine {
	if e.inner != nil || !e.failing || e.fbErr != nil {
		return e.inner
	}
	e.fb.Lock()
	ready, eng, err := e.fb.ready, e.fb.eng, e.fb.err
	e.fb.eng = nil
	e.fb.Unlock()
	if !ready {
		return nil
	}
	if err != nil {
		e.fbErr = fmt.Errorf("pryon failed (%s) and the openWakeWord fallback failed: %w", e.failReason, err)
		e.setInfo(e.fbErr.Error())
		e.logf("[pryon] %v", e.fbErr)
		return nil
	}
	e.inner = eng
	e.setInfo(fmt.Sprintf("%s (pryon failed: %s)", eng.Info(), e.failReason))
	e.logf("[pryon] now scoring with openWakeWord: %s", eng.Info())
	return eng
}

func (e *pryonEngine) Push(pcm []int16, at time.Time) error {
	if eng := e.active(); eng != nil {
		return eng.Push(pcm, at)
	}
	if e.fbErr != nil {
		return e.fbErr
	}
	if e.failing {
		return nil // openWakeWord still loading: not ready
	}
	e.pump()
	if e.failing {
		return nil
	}
	p := e.p
	if p == nil {
		if !e.downReported {
			e.downReported = true
			return fmt.Errorf("%w: %s", errHelperDown, e.lastErr)
		}
		return nil
	}
	if !p.ready {
		// Nothing is sent before READY: the helper does not read stdin while
		// it loads, and audio queued behind a slow start would come out as
		// detections too old to use.
		return nil
	}
	if e.resetPending {
		if !trySend(p, encodePryonFrame('R', nil)) {
			// The gap marker must precede the audio after the gap.
			return e.behind(p)
		}
		e.resetPending = false
	}
	if e.propPending {
		v := "0"
		if e.playback {
			v = "1"
		}
		if trySend(p, encodePryonFrame('P', []byte("audio_playback="+v))) {
			e.propPending = false
		}
	}
	if !trySend(p, encodePryonAudio(pcm)) {
		return e.behind(p)
	}
	p.behindSince = time.Time{}
	e.ring.add(pryonFrame{start: e.fed, n: uint32(len(pcm)), at: at})
	e.fed += uint64(len(pcm))
	e.lastAt = at
	return nil
}

// behind counts a refused frame and is the stuck-helper watchdog: see
// pryonStuckAfter. The kill is noticed as an exit on a later frame and goes
// through the ordinary crash policy (backoff, and failover after five).
func (e *pryonEngine) behind(p *pryonProc) error {
	e.win.drops++
	now := e.now()
	if p.behindSince.IsZero() {
		p.behindSince = now
	} else if !p.killedByUs && now.Sub(p.behindSince) >= pryonStuckAfter {
		p.killedByUs = true
		p.killReason = fmt.Sprintf("not reading stdin for %s", now.Sub(p.behindSince).Round(time.Millisecond))
		e.logf("[pryon] helper pid %d is %s; killing it", p.pid, p.killReason)
		killGroup(p)
	}
	return errBehind
}

func trySend(p *pryonProc, b []byte) bool {
	select {
	case p.frames <- b:
		return true
	default:
		return false
	}
}

func (e *pryonEngine) Ready() bool {
	if e.inner != nil {
		return e.inner.Ready()
	}
	return !e.failing && e.p != nil && e.p.ready
}

// Score reports a held wake for pryonHoldFrames frames, otherwise 0 for the
// frame just pushed. Near misses never get here: a Pryon near miss can carry a
// classifier score of 0.8, which would clear openWakeWord's 0.5 and open a
// session on a word Pryon itself rejected.
func (e *pryonEngine) Score() (float32, time.Time, error) {
	if e.inner != nil {
		return e.inner.Score()
	}
	if e.hold.left > 0 {
		// Checked again on the way out, not only when the line arrived: Score
		// runs only for frames the helper accepted, so while it is behind a
		// held wake can sit here for seconds and would then open a session
		// on audio the listen gate no longer has.
		if age := e.now().Sub(e.hold.at); age > e.staleAfter {
			e.hold.left = 0
			e.win.stale++
			e.logf("[pryon] held wake %dms old, dropped (limit %dms)",
				age.Milliseconds(), e.staleAfter.Milliseconds())
			return 0, e.lastAt, nil
		}
		e.hold.left--
		return e.hold.score, e.hold.at, nil
	}
	return 0, e.lastAt, nil
}

// Reset drops every wake that is held or queued, whatever the helper's reset
// mode: a wake reported across a gap belongs to the audio before it — for
// example one that arrived just as the mic was muted, which would
// otherwise open a session on the first frame after unmuting. Late arrivals
// are caught by resetFed.
func (e *pryonEngine) Reset() {
	if e.inner != nil {
		e.inner.Reset()
		return
	}
	e.hold.left = 0
	if e.p != nil {
		e.drain(e.p, true, e.now())
		e.resetPending = true
	}
	e.resetFed = e.fed
}

// SetPlayback forwards the playback state as the client property
// audio_playback, which lowers Pryon's own threshold the way the Scorer lowers
// its bar. Sticky: sent with the next frame that fits, and again after a
// restart.
func (e *pryonEngine) SetPlayback(on bool) {
	if e.inner != nil {
		e.inner.SetPlayback(on)
	}
	if on != e.playback {
		e.playback = on
		e.propPending = true
	}
}

// Close stops the helper (bounded, see closeProc) and whatever replaced it.
func (e *pryonEngine) Close() error {
	e.closeOnce.Do(func() {
		if p := e.p; p != nil {
			e.p = nil
			e.closeProc(p)
		}
		e.fb.Lock()
		e.fb.closed = true
		pending := e.fb.eng
		e.fb.eng = nil
		e.fb.Unlock()
		if pending != nil {
			_ = pending.Close()
		}
		if e.inner != nil {
			_ = e.inner.Close()
		}
	})
	return nil
}

// maybeStats writes the 30s line:
//
//	[pryon] cpu=18.2% accepts=1 nearMiss=3 restarts=0 drops=0 stale=0 lat=p50 210ms/max 340ms
//
// cpu is the helper's user+system time as a percentage of ONE core over the
// window, all its threads together, so it can exceed 100% on a multi-core
// device; it is left out where /proc is not there. drops are frames refused
// because the helper was behind, stale are accepted wakes thrown away for
// their age or a Reset, lat is word end -> W line arriving, which with the
// stale limit is the number that says whether Pryon is fast enough here.
func (e *pryonEngine) maybeStats(now time.Time) {
	if e.lastStats.IsZero() {
		e.lastStats = now
		return
	}
	if now.Sub(e.lastStats) < pryonStatsEvery {
		return
	}
	var b strings.Builder
	b.WriteString("[pryon]")
	if p := e.p; p != nil {
		if ticks, ok := readProcCPU(procRoot, p.pid); ok {
			if e.cpuPid == p.pid && !e.cpuAt.IsZero() && now.After(e.cpuAt) && ticks >= e.cpuTicks {
				secs := float64(ticks-e.cpuTicks) / pryonClockTicks
				fmt.Fprintf(&b, " cpu=%.1f%%", 100*secs/now.Sub(e.cpuAt).Seconds())
			}
			e.cpuPid, e.cpuTicks, e.cpuAt = p.pid, ticks, now
		}
	}
	w := &e.win
	fmt.Fprintf(&b, " accepts=%d nearMiss=%d restarts=%d drops=%d stale=%d",
		w.accepts, w.nearMiss, e.restarts, w.drops, w.stale)
	if len(w.lat) > 0 {
		sort.Slice(w.lat, func(i, j int) bool { return w.lat[i] < w.lat[j] })
		fmt.Fprintf(&b, " lat=p50 %dms/max %dms",
			w.lat[len(w.lat)/2].Milliseconds(), w.lat[len(w.lat)-1].Milliseconds())
	}
	if w.foreign > 0 {
		fmt.Fprintf(&b, " foreign=%d", w.foreign)
	}
	if w.badScore > 0 {
		fmt.Fprintf(&b, " badScore=%d", w.badScore)
	}
	if p := e.p; p != nil {
		if lost := p.lost.Swap(0); lost > 0 {
			fmt.Fprintf(&b, " lost=%d", lost)
		}
	}
	e.logf("%s", b.String())
	e.win.accepts, e.win.nearMiss, e.win.drops, e.win.stale, e.win.foreign, e.win.badScore = 0, 0, 0, 0, 0, 0
	e.win.lat = e.win.lat[:0]
	e.lastStats = now
}

// readProcCPU returns utime+stime of pid in clock ticks.
func readProcCPU(root string, pid int) (uint64, bool) {
	b, err := os.ReadFile(fmt.Sprintf("%s/%d/stat", root, pid))
	if err != nil {
		return 0, false
	}
	f, ok := procStatFields(string(b))
	if !ok || len(f) < 13 {
		return 0, false
	}
	u, err1 := strconv.ParseUint(f[11], 10, 64)
	s, err2 := strconv.ParseUint(f[12], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return u + s, true
}

// procStatFields splits /proc/<pid>/stat AFTER the command name, which is in
// parentheses and may itself contain spaces and parentheses. Index 0 is the
// state, 1 the ppid, 11 utime, 12 stime.
func procStatFields(stat string) ([]string, bool) {
	i := strings.LastIndexByte(stat, ')')
	if i < 0 {
		return nil, false
	}
	return strings.Fields(stat[i+1:]), true
}
