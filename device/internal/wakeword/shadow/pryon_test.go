package shadow

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The Pryon engine is tested against a FAKE helper: this test binary, run
// again with GO_WANT_HELPER=1, speaks protocol v1 the way pryon_wake does. It
// needs no device, no libpryon.so and no C compiler, and lets each test choose
// the helper's behaviour (FAKE_PRYON) — including the ones the real helper only
// shows on a bad day: never ready, never reading, never exiting, crashing.
//
// Detections are steered from the audio itself: a frame whose first sample is
// one of the cmd* values below makes the fake helper print a line about it, in
// order with the audio — which is what keeps these tests deterministic while
// the engine reads the helper's output on other goroutines.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_HELPER") == "1" {
		os.Exit(fakePryonHelper())
	}
	os.Exit(m.Run())
}

const (
	cmdWake     = 0x5757 // W ALEXA, end from samples 1-2, score*1000 in 3
	cmdNear     = 0x4E4E // N ALEXA
	cmdStop     = 0x5353 // W STOP (a keyword that must never count)
	cmdCrash    = 0x4343 // exit 1
	cmdFatal    = 0x4545 // E ABI, exit 6
	cmdSIGKILL  = 0x4B4B // SIGKILL itself, like the OOM killer would
	cmdSplitW   = 0x5858 // W ALEXA, written in two pieces 30ms apart
	fakeFrameSz = 2560
)

func fakePryonHelper() int {
	mode := map[string]bool{}
	for _, m := range strings.Split(os.Getenv("FAKE_PRYON"), ",") {
		mode[m] = true
	}
	out := os.Stdout
	switch {
	case mode["fatal"]:
		fmt.Fprintln(out, "E LIB fake: cannot load libpryon.so")
		return 3
	case mode["crashstart"]:
		return 1
	}
	if !mode["noready"] {
		fmt.Fprintln(out, "READY v=1 warmup=0 manifest=/fake attrs=fake")
	}
	if mode["spam"] {
		for i := 0; i < 100; i++ {
			fmt.Fprintf(os.Stderr, "[pryon] KWS: line %d\n", i)
		}
	}
	if mode["noread"] {
		time.Sleep(time.Hour)
		return 0
	}
	var fed, seq uint64
	h := make([]byte, 8)
	for {
		if _, err := io.ReadFull(os.Stdin, h); err != nil {
			if mode["noexit"] {
				time.Sleep(time.Hour)
			}
			return 0
		}
		if h[0] != 'P' || h[1] != 'W' {
			fmt.Fprintln(out, "E IO bad frame magic")
			return 7
		}
		n := int(h[4]) | int(h[5])<<8 | int(h[6])<<16 | int(h[7])<<24
		payload := make([]byte, n)
		if _, err := io.ReadFull(os.Stdin, payload); err != nil {
			return 7
		}
		switch h[2] {
		case 'P':
			fmt.Fprintf(os.Stderr, "got P %s\n", payload)
		case 'R':
			fmt.Fprintln(os.Stderr, "got R")
		case 'A':
			if n != fakeFrameSz {
				fmt.Fprintf(out, "E IO audio frame of %d bytes\n", n)
				return 7
			}
			s := func(i int) uint16 { return uint16(payload[2*i]) | uint16(payload[2*i+1])<<8 }
			fed += uint64(n / 2)
			end := uint64(s(1)) | uint64(s(2))<<16
			begin := uint64(0)
			if end > 8000 {
				begin = end - 8000
			}
			score := float64(int16(s(3))) / 1000
			switch s(0) {
			case cmdWake:
				seq++
				fmt.Fprintf(out, "W %d ALEXA 2 %d %d %.3f\n", seq, begin, end, score)
			case cmdSplitW:
				seq++
				line := fmt.Sprintf("W %d ALEXA 3 %d %d %.3f\n", seq, begin, end, score)
				out.WriteString(line[:5])
				time.Sleep(30 * time.Millisecond)
				out.WriteString(line[5:])
			case cmdNear:
				fmt.Fprintf(out, "N ALEXA %d %d %.3f\n", begin, end, score)
			case cmdStop:
				seq++
				fmt.Fprintf(out, "W %d STOP 2 %d %d %.3f\n", seq, begin, end, score)
			case cmdCrash:
				return 1
			case cmdFatal:
				fmt.Fprintln(out, "E ABI implausible result")
				return 6
			case cmdSIGKILL:
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				time.Sleep(time.Hour)
			}
		}
	}
}

// --- test scaffolding --------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Add(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (l *logSink) logf(format string, args ...any) {
	l.mu.Lock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}

func (l *logSink) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (l *logSink) has(sub string) bool { return l.count(sub) > 0 }

func (l *logSink) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// fakeEngine is the openWakeWord stand-in a failover lands on.
type fakeEngine struct {
	mu     sync.Mutex
	pushes int
	closed bool
}

func (f *fakeEngine) Push([]int16, time.Time) error {
	f.mu.Lock()
	f.pushes++
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) Ready() bool                        { return true }
func (f *fakeEngine) Score() (float32, time.Time, error) { return 0, time.Time{}, nil }
func (f *fakeEngine) Reset()                             {}
func (f *fakeEngine) SetPlayback(bool)                   {}
func (f *fakeEngine) Info() string                       { return "fake oww" }
func (f *fakeEngine) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeEngine) isClosed() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.closed }

type harness struct {
	t     *testing.T
	e     *pryonEngine
	clk   *fakeClock
	log   *logSink
	fb    *fakeEngine
	fbN   int
	procs []*pryonProc
	mu    sync.Mutex
}

// newHarness starts an engine on the fake helper in the given mode, and on
// cleanup closes it and proves that nothing it started is still running.
func newHarness(t *testing.T, mode string) *harness {
	t.Helper()
	t.Setenv("GO_WANT_HELPER", "1")
	t.Setenv("FAKE_PRYON", mode)
	// A -race helper otherwise sleeps 1s on exit (atexit_sleep_ms), which
	// would make every clean shutdown look like one that needed SIGKILL.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	before := runtime.NumGoroutine()
	h := &harness{t: t, clk: newFakeClock(), log: &logSink{}, fb: &fakeEngine{}}
	cfg := pryonConfig{Helper: os.Args[0], Lib: "/fake/libpryon.so", Manifest: "/fake/pryon.manifest",
		AtOffsetFrames: DefaultPryonAtOffsetFrames}
	h.e = newPryonEngine("pryon_alexa", cfg, func() (engine, error) {
		h.mu.Lock()
		h.fbN++
		h.mu.Unlock()
		return h.fb, nil
	})
	h.e.now = h.clk.Now
	h.e.logf = h.log.logf
	h.e.closeGrace = 300 * time.Millisecond
	h.e.killWait = 2 * time.Second
	h.e.onSpawn = func(p *pryonProc) {
		h.mu.Lock()
		h.procs = append(h.procs, p)
		h.mu.Unlock()
	}
	if err := h.e.start(h.clk.Now()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		h.e.Close()
		h.assertAllGone(before)
	})
	return h
}

// assertAllGone: every helper exited and was reaped, every goroutine that
// served one finished, and the goroutine count is back where it was.
func (h *harness) assertAllGone(before int) {
	h.t.Helper()
	h.mu.Lock()
	procs := append([]*pryonProc(nil), h.procs...)
	h.mu.Unlock()
	for _, p := range procs {
		for name, ch := range map[string]chan struct{}{"process": p.exited, "writer": p.writerDone,
			"stdout reader": p.stdoutDone, "stderr reader": p.stderrDone} {
			if !waitClosed(ch, 3*time.Second) {
				h.t.Errorf("helper pid %d: %s still running after Close", p.pid, name)
			}
		}
		if err := syscall.Kill(p.pid, 0); !errors.Is(err, syscall.ESRCH) {
			h.t.Errorf("helper pid %d still exists after Close (kill 0: %v)", p.pid, err)
		}
	}
	if h.e.fbDone != nil && !waitClosed(h.e.fbDone, 3*time.Second) {
		h.t.Error("fallback goroutine still running after Close")
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		h.t.Errorf("%d goroutines before, %d after Close:\n%s", before, n, buf[:runtime.Stack(buf, true)])
	}
}

func silence() []int16 { return make([]int16, pryonFrameSamples) }

// command builds a frame that makes the fake helper report end/score.
func command(cmd uint16, end uint64, score float64) []int16 {
	f := silence()
	f[0] = int16(cmd)
	f[1] = int16(uint16(end))
	f[2] = int16(uint16(end >> 16))
	f[3] = int16(score * 1000)
	return f
}

// push sends one frame stamped with the clock, advancing it by 80ms first.
// A test loop pushes far faster than the 12.5 frames a second of a real mic,
// so a frame refused with errBehind is offered again (same stamp) until the
// helper has caught up; TestPryonPushNeverBlocks calls Push directly to see
// the refusals.
func (h *harness) push(pcm []int16) error {
	at := h.clk.Add(80 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := h.e.Push(pcm, at)
		if !errors.Is(err, errBehind) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// until pushes silent frames until cond holds (real-time deadline), because
// what the helper says arrives on other goroutines and is only taken in by
// Push — exactly as in the Scorer.
func (h *harness) until(what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s\nlog:\n%s", what, h.log)
		}
		_ = h.e.Push(silence(), h.clk.Now())
		time.Sleep(2 * time.Millisecond)
	}
}

// untilQuiet waits without pushing: for events to be queued by the reader.
func (h *harness) untilQueued(n int) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(h.e.p.events) < n {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %d queued helper lines", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) ready() {
	h.t.Helper()
	h.until("READY", h.e.Ready)
}

func (h *harness) fallbacks() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fbN
}

// --- protocol ----------------------------------------------------------------

func TestPryonParseLine(t *testing.T) {
	cases := []struct {
		line string
		ok   bool
		want pryonEvent
	}{
		{"READY v=1 warmup=3000 manifest=/m attrs=x", true, pryonEvent{kind: 'R', text: "v=1 warmup=3000 manifest=/m attrs=x"}},
		{"W 7 ALEXA 2 16000 16800 0.950", true, pryonEvent{kind: 'W', keyword: "ALEXA", begin: 16000, end: 16800, score: 0.95}},
		{"W 1 ALEXA 3 0 1 -1.000", true, pryonEvent{kind: 'W', keyword: "ALEXA", begin: 0, end: 1, score: -1}},
		{"N ALEXA 100 900 0.801", true, pryonEvent{kind: 'N', keyword: "ALEXA", begin: 100, end: 900, score: 0.801}},
		{"E MODEL /x/missing.manifest", true, pryonEvent{kind: 'E', code: "MODEL", text: "/x/missing.manifest"}},
		{"E ABI implausible result: library layout differs", true, pryonEvent{kind: 'E', code: "ABI", text: "implausible result: library layout differs"}},
		{"W 7 ALEXA 2 16000 16800", false, pryonEvent{}},
		{"W x ALEXA 2 a b c", false, pryonEvent{}},
		{"hello", false, pryonEvent{}},
		{"", false, pryonEvent{}},
	}
	for _, c := range cases {
		got, ok := parsePryonLine(c.line)
		if ok != c.ok || (ok && (got.kind != c.want.kind || got.keyword != c.want.keyword ||
			got.begin != c.want.begin || got.end != c.want.end || got.score != c.want.score ||
			got.code != c.want.code || got.text != c.want.text)) {
			t.Errorf("parsePryonLine(%q) = %+v, %v; want %+v, %v", c.line, got, ok, c.want, c.ok)
		}
	}
}

func TestPryonFrameEncoding(t *testing.T) {
	b := encodePryonAudio([]int16{1, -2, 0x1234})
	want := []byte{'P', 'W', 'A', 0, 6, 0, 0, 0, 1, 0, 0xFE, 0xFF, 0x34, 0x12}
	if string(b) != string(want) {
		t.Fatalf("audio frame % x, want % x", b, want)
	}
	p := encodePryonFrame('P', []byte("audio_playback=1"))
	if string(p[:8]) != "PWP\x00\x10\x00\x00\x00" || string(p[8:]) != "audio_playback=1" {
		t.Fatalf("property frame %q", p)
	}
	if r := encodePryonFrame('R', nil); string(r) != "PWR\x00\x00\x00\x00\x00" {
		t.Fatalf("reset frame %q", r)
	}
}

// TestPryonFramesArriveWhole: the helper checks every header and length, so a
// framing error ends it with E IO. Many frames, a property and a reset later
// it must still be running and must have seen the property and the reset; and
// a stdout line written in two pieces must still be read as one wake.
func TestPryonFramesArriveWhole(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	h.e.SetPlayback(true)
	for i := 0; i < 40; i++ {
		if err := h.push(silence()); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
	}
	h.e.Reset()
	_ = h.push(silence())
	h.until("property and reset at the helper", func() bool {
		return h.log.has("got P audio_playback=1") && h.log.has("got R")
	})
	end := h.e.fed + pryonFrameSamples
	_ = h.push(command(cmdSplitW, end, 0.9))
	h.until("a wake split across two writes", func() bool { return h.e.hold.left > 0 })
	if h.log.has("helper error") || h.e.p == nil {
		t.Fatalf("helper rejected a frame:\n%s", h.log)
	}
}

func TestPryonNotReadyUntilREADY(t *testing.T) {
	h := newHarness(t, "noready")
	for i := 0; i < 20; i++ {
		if err := h.push(silence()); err != nil {
			t.Fatalf("Push before READY: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
	if h.e.Ready() {
		t.Fatal("ready without READY")
	}
	if h.e.fed != 0 || len(h.e.p.frames) != 0 {
		t.Fatalf("audio sent before READY: fed=%d queued=%d", h.e.fed, len(h.e.p.frames))
	}
}

// TestPryonReadyTimeoutFallsBack: a first helper that never says READY is
// killed after 5s (by the injected clock) and the engine moves to
// openWakeWord at once — a device that cannot start Pryon should not spend
// five more tries finding out.
func TestPryonReadyTimeoutFallsBack(t *testing.T) {
	h := newHarness(t, "noready")
	_ = h.push(silence())
	h.clk.Add(pryonReadyTimeout)
	h.until("failover to openWakeWord", func() bool { return h.e.inner != nil })
	if !strings.Contains(h.e.Info(), "no READY within 5s") || !strings.Contains(h.e.Info(), "fake oww") {
		t.Errorf("Info = %q", h.e.Info())
	}
	_ = h.push(silence())
	if h.fb.mu.Lock(); h.fb.pushes == 0 {
		t.Error("frames not reaching the fallback engine")
	}
	h.fb.mu.Unlock()
	if h.log.count("giving up on Pryon") != 1 {
		t.Errorf("failover not logged exactly once:\n%s", h.log)
	}
}

// TestPryonLateLookDoesNotKillAReadyHelper: frames stop while muted, so the
// engine may not look at the helper for minutes. A READY that arrived in time
// must count as in time, judged by when it was read, not when it was seen.
func TestPryonLateLookDoesNotKillAReadyHelper(t *testing.T) {
	h := newHarness(t, "")
	h.untilQueued(1) // READY read, not yet taken in
	h.clk.Add(10 * time.Second)
	_ = h.push(silence())
	if !h.e.Ready() || h.e.p == nil || h.e.p.killedByUs {
		t.Fatalf("a helper that was ready in time was treated as never ready:\n%s", h.log)
	}
}

// --- detections -----------------------------------------------------------------

// TestWakeAtTable pins the at formula (see wakeAt): the capture time of the
// frame holding end-3*1280, unchanged — never interpolated.
func TestWakeAtTable(t *testing.T) {
	base := time.Unix(100, 0)
	var r pryonRing
	// Ten frames as the mic stamps them: two per 160ms batch, microseconds
	// apart within a batch.
	at := func(i int) time.Time {
		return base.Add(time.Duration(i/2)*160*time.Millisecond + time.Duration(i%2)*time.Microsecond)
	}
	for i := 0; i < 10; i++ {
		r.add(pryonFrame{start: uint64(i) * 1280, n: 1280, at: at(i)})
	}
	cases := []struct {
		end    uint64
		offset int
		frame  int // -1: not in the ring
		why    string
	}{
		{8960, 3, 4, "end at the start of frame 7 -> frame 4"},
		{8959, 3, 3, "end at the last sample of frame 6 -> frame 3"},
		{9500, 3, 4, "end inside frame 7 -> frame 4, whatever the offset inside"},
		{12800, 3, 7, "end one past the newest frame -> frame 7"},
		{20000, 3, 9, "end beyond what was sent -> newest frame"},
		{3840, 3, 0, "end in frame 3 -> frame 0"},
		{1000, 3, 0, "end within the first 3 frames -> clamps to frame 0"},
		{8960, 0, 7, "offset 0 -> the frame holding the end itself"},
		{8960, 1, 6, "offset 1"},
	}
	for _, c := range cases {
		f, ok := wakeAt(&r, c.end, c.offset)
		if c.frame < 0 {
			if ok {
				t.Errorf("%s: got frame at %v, want none", c.why, f.at)
			}
			continue
		}
		if !ok || !f.at.Equal(at(c.frame)) || f.start != uint64(c.frame)*1280 {
			t.Errorf("%s: end=%d offset=%d -> start %d at %v ok=%v, want frame %d",
				c.why, c.end, c.offset, f.start, f.at, ok, c.frame)
		}
	}
	// After the ring wraps, audio older than it is gone.
	for i := 10; i < 10+pryonRingFrames; i++ {
		r.add(pryonFrame{start: uint64(i) * 1280, n: 1280, at: at(i)})
	}
	if _, ok := wakeAt(&r, 5000+3*1280, 3); ok {
		t.Error("a position older than the ring mapped to a frame")
	}
	oldest := uint64(10) * 1280
	if f, ok := wakeAt(&r, oldest+3*1280, 3); !ok || f.start != oldest {
		t.Errorf("the oldest frame in the ring: start %d ok %v", f.start, ok)
	}
}

// TestPryonWakeIsHeldForTwoFramesAtTheMappedTime runs the mapping through the
// helper: the wake comes back with the capture time of the frame 3 before the
// one it ended in, the score it carried, for exactly two frames.
func TestPryonWakeIsHeldForTwoFramesAtTheMappedTime(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	stamps := map[uint64]time.Time{}
	for i := 0; i < 12; i++ {
		start := h.e.fed
		if err := h.push(silence()); err != nil {
			t.Fatal(err)
		}
		stamps[start] = h.clk.Now()
	}
	end := uint64(10*1280 + 500) // inside frame 10
	if err := h.push(command(cmdWake, end, 0.987)); err != nil {
		t.Fatal(err)
	}
	h.until("the wake", func() bool { return h.e.hold.left > 0 })
	for i := 0; i < 2; i++ {
		score, at, _ := h.e.Score()
		if score < 0.9869 || score > 0.9871 || !at.Equal(stamps[7*1280]) {
			t.Fatalf("frame %d: score %.4f at %v, want 0.987 at frame 7's stamp %v", i, score, at, stamps[7*1280])
		}
	}
	if score, _, _ := h.e.Score(); score != 0 {
		t.Fatalf("wake still held on a third frame (score %v)", score)
	}
}

func TestPryonUnknownScoreCountsAsCertain(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	_ = h.push(silence())
	_ = h.push(command(cmdWake, h.e.fed, -1))
	h.until("the wake", func() bool { return h.e.hold.left > 0 })
	if score, _, _ := h.e.Score(); score != 1 {
		t.Fatalf("score %v for a wake without a library score, want 1", score)
	}
}

// TestPryonResetDropsWakes is review B3: a wake held, queued, or arriving
// late about audio from before a Reset must never open a session after it.
func TestPryonResetDropsWakes(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	for i := 0; i < 6; i++ {
		_ = h.push(silence())
	}
	// Held.
	_ = h.push(command(cmdWake, h.e.fed, 0.95))
	h.until("a held wake", func() bool { return h.e.hold.left > 0 })
	h.e.Reset()
	if s, _, _ := h.e.Score(); s != 0 {
		t.Fatalf("held wake survived Reset (score %v)", s)
	}
	// Queued, not yet taken in.
	_ = h.push(command(cmdWake, h.e.fed, 0.95))
	h.untilQueued(1)
	h.e.Reset()
	_ = h.push(silence())
	if s, _, _ := h.e.Score(); s != 0 {
		t.Fatalf("queued wake survived Reset (score %v)", s)
	}
	// Arriving after the Reset, about audio before it.
	before := h.e.fed
	h.e.Reset()
	_ = h.push(command(cmdWake, before-100, 0.95))
	h.until("the late wake to be dropped", func() bool { return h.log.has("from before the last reset") })
	if h.e.hold.left != 0 {
		t.Fatal("a wake about audio before the Reset was held")
	}
	// And a fresh one after the gap still works.
	_ = h.push(command(cmdWake, h.e.fed, 0.95))
	h.until("a fresh wake", func() bool { return h.e.hold.left > 0 })
}

// TestPryonOldWakeIsDropped: one seen more than ~1.2s after its capture time
// would open a session the listen gate can no longer serve.
func TestPryonOldWakeIsDropped(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	for i := 0; i < 8; i++ {
		_ = h.push(silence())
	}
	end := h.e.fed
	h.clk.Add(2 * time.Second)
	_ = h.e.Push(command(cmdWake, end, 0.95), h.clk.Now())
	h.until("the old wake to be dropped", func() bool { return h.e.win.stale > 0 })
	if h.e.hold.left != 0 {
		t.Fatal("an old wake was held")
	}
}

// TestPryonNearMissesAndOtherKeywordsNeverScore: a near miss can carry 0.8,
// which would clear openWakeWord's 0.5; STOP from a television must not wake.
func TestPryonNearMissesAndOtherKeywordsNeverScore(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	_ = h.push(silence())
	_ = h.push(command(cmdNear, h.e.fed, 0.8))
	_ = h.push(command(cmdStop, h.e.fed, 0.99))
	h.until("both lines", func() bool { return h.e.win.nearMiss == 1 && h.e.win.foreign == 1 })
	for i := 0; i < 3; i++ {
		if s, _, _ := h.e.Score(); s != 0 {
			t.Fatalf("score %v from a near miss or STOP", s)
		}
	}
}

// --- process lifecycle ----------------------------------------------------------

// TestPryonCrashRestartsWithBackoff: 1, 2, 4, 8s between restarts by the
// injected clock, nothing earlier; the fifth crash in a row gives up.
func TestPryonCrashRestartsWithBackoff(t *testing.T) {
	h := newHarness(t, "crashstart")
	for i, delay := range pryonBackoff[:4] {
		h.until(fmt.Sprintf("crash %d noticed", i+1), func() bool { return h.e.p == nil })
		if h.e.crashStreak != i+1 {
			t.Fatalf("crash %d: streak %d", i+1, h.e.crashStreak)
		}
		if got := h.e.nextStart.Sub(h.clk.Now()); got != delay {
			t.Fatalf("crash %d: restart in %v, want %v", i+1, got, delay)
		}
		_ = h.e.Push(silence(), h.clk.Add(delay-time.Millisecond))
		if h.e.p != nil {
			t.Fatalf("crash %d: restarted before the backoff ran out", i+1)
		}
		_ = h.e.Push(silence(), h.clk.Add(time.Millisecond))
		if h.e.p == nil {
			t.Fatalf("crash %d: not restarted after %v", i+1, delay)
		}
	}
	h.until("the fifth crash to fail over", func() bool { return h.e.inner != nil })
	if h.fallbacks() != 1 || h.e.restarts != 4 {
		t.Fatalf("fallbacks %d restarts %d", h.fallbacks(), h.e.restarts)
	}
	if !strings.Contains(h.e.Info(), "5 crashes in a row") {
		t.Errorf("Info = %q", h.e.Info())
	}
}

// TestPryonHelperDownIsReportedOnce: the first frame after a crash says why,
// the rest count as not ready rather than as an error each.
func TestPryonHelperDownIsReportedOnce(t *testing.T) {
	h := newHarness(t, "crashstart")
	var errs []error
	h.until("crash noticed", func() bool { return h.e.p == nil })
	for i := 0; i < 5; i++ {
		if err := h.push(silence()); err != nil {
			errs = append(errs, err)
		}
	}
	// The crash may already have been reported inside until().
	if len(errs) > 1 || (len(errs) == 1 && !errors.Is(errs[0], errHelperDown)) {
		t.Fatalf("errors while down: %v", errs)
	}
}

// TestPryonLongRunResetsTheCrashSeries: a helper that ran a minute worked;
// its crash starts over at a 1s backoff.
func TestPryonLongRunResetsTheCrashSeries(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	h.e.crashStreak, h.e.backoffIdx = 3, 3
	h.clk.Add(pryonLongRun)
	_ = h.e.Push(command(cmdCrash, 0, 0), h.clk.Now())
	h.until("the crash", func() bool { return h.e.p == nil })
	if h.e.crashStreak != 0 || h.e.nextStart.Sub(h.clk.Now()) != time.Second {
		t.Fatalf("streak %d, restart in %v", h.e.crashStreak, h.e.nextStart.Sub(h.clk.Now()))
	}
}

// TestPryonForeignSIGKILLIsNotACrash: the OOM killer or the parent-death
// signal is not Pryon's fault and must not walk the engine to openWakeWord.
func TestPryonForeignSIGKILLIsNotACrash(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	_ = h.push(command(cmdSIGKILL, 0, 0))
	h.until("the kill", func() bool { return h.e.p == nil })
	if h.e.crashStreak != 0 || h.e.restarts != 1 {
		t.Fatalf("streak %d restarts %d", h.e.crashStreak, h.e.restarts)
	}
}

func TestPryonFatalErrorFailsOver(t *testing.T) {
	t.Run("at start", func(t *testing.T) {
		h := newHarness(t, "fatal")
		h.until("failover", func() bool { return h.e.inner != nil })
		if !strings.Contains(h.e.Info(), "LIB fake: cannot load") || h.e.restarts != 0 {
			t.Errorf("Info %q restarts %d", h.e.Info(), h.e.restarts)
		}
	})
	t.Run("while running", func(t *testing.T) {
		h := newHarness(t, "")
		h.ready()
		_ = h.push(command(cmdFatal, 0, 0))
		h.until("failover", func() bool { return h.e.inner != nil })
		if !strings.Contains(h.e.Info(), "ABI implausible") {
			t.Errorf("Info %q", h.e.Info())
		}
	})
}

// TestPryonFallbackClosedWhileLoading: openWakeWord is loaded off the scorer
// goroutine. If the engine is closed first, the loaded engine must still be
// released — it holds the ORT sessions.
func TestPryonFallbackClosedWhileLoading(t *testing.T) {
	h := newHarness(t, "fatal")
	release := make(chan struct{})
	late := &fakeEngine{}
	h.e.fallback = func() (engine, error) { <-release; return late, nil }
	h.until("failover started", func() bool { return h.e.failing })
	h.e.Close()
	close(release)
	if !waitClosed(h.e.fbDone, 3*time.Second) {
		t.Fatal("fallback goroutine did not finish")
	}
	if !late.isClosed() {
		t.Fatal("an engine loaded after Close was not closed")
	}
}

func TestPryonFallbackFailureIsAnError(t *testing.T) {
	h := newHarness(t, "fatal")
	h.e.fallback = func() (engine, error) { return nil, errors.New("no onnxruntime") }
	h.until("failover attempted", func() bool { return h.e.fbErr != nil })
	if err := h.push(silence()); err == nil || !strings.Contains(err.Error(), "no onnxruntime") {
		t.Fatalf("Push = %v", err)
	}
	if h.e.Ready() {
		t.Fatal("ready with neither engine")
	}
}

// TestPryonPushNeverBlocks: a helper that stops reading cannot hold up the
// scorer goroutine. The writer channel fills, frames are refused with
// errBehind, and every Push returns at once.
func TestPryonPushNeverBlocks(t *testing.T) {
	h := newHarness(t, "noread")
	h.ready()
	start := time.Now()
	behind := 0
	for i := 0; i < 400; i++ { // ~1MB: well past any pipe buffer
		if err := h.e.Push(silence(), h.clk.Now()); errors.Is(err, errBehind) {
			behind++
		}
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("400 Pushes took %v", d)
	}
	if behind == 0 || h.e.win.drops != uint64(behind) {
		t.Fatalf("errBehind %d times, drops %d", behind, h.e.win.drops)
	}
}

// TestPryonCloseNeverHangs: Close runs on the config path. It must return in
// bounded time whatever state the helper is in, and leave nothing behind
// (checked by the harness). The clock stands still here, so no READY timeout
// gets to the helper before Close does.
func TestPryonCloseNeverHangs(t *testing.T) {
	for _, c := range []struct {
		name, mode string
		mustKill   bool
	}{
		{"full pipe", "noread", true},
		{"ignores EOF", "noexit", true},
		{"never ready, never reading", "noready,noread", true},
		{"healthy", "", false},
		{"already dead", "crashstart", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.mode)
			p := h.e.p
			switch c.mode {
			case "noread":
				h.ready()
				h.until("a full pipe", func() bool { return h.e.win.drops > 0 })
			case "noexit", "":
				h.ready()
				_ = h.push(silence())
			case "noready,noread":
				for i := 0; i < 10; i++ {
					_ = h.e.Push(silence(), h.clk.Now())
				}
			case "crashstart":
				h.until("the crash", func() bool { return h.e.p == nil })
			}
			if c.mode != "crashstart" && h.e.p != p {
				t.Fatalf("helper not the one started (state: %+v)", h.e.p)
			}
			start := time.Now()
			h.e.Close()
			if d, limit := time.Since(start), h.e.closeGrace+2*h.e.killWait; d > limit {
				t.Fatalf("Close took %v (limit %v)", d, limit)
			}
			if p.killedByUs != c.mustKill {
				t.Errorf("killed=%v, want %v", p.killedByUs, c.mustKill)
			}
		})
	}
}

func TestPryonSetPlaybackIsSticky(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	h.e.SetPlayback(true)
	_ = h.push(silence())
	h.until("audio_playback=1", func() bool { return h.log.has("got P audio_playback=1") })
	h.e.SetPlayback(false)
	_ = h.push(silence())
	h.until("audio_playback=0", func() bool { return h.log.has("got P audio_playback=0") })
	// After a restart the new helper is told again, when it matters.
	h.e.SetPlayback(true)
	_ = h.push(command(cmdCrash, 0, 0))
	h.until("crash", func() bool { return h.e.p == nil })
	h.clk.Add(time.Second)
	h.ready()
	_ = h.push(silence())
	h.until("audio_playback=1 again", func() bool { return h.log.count("got P audio_playback=1") >= 2 })
}

func TestPryonStderrIsRateLimited(t *testing.T) {
	h := newHarness(t, "spam")
	h.ready()
	h.until("the helper's log lines", func() bool { return h.log.has("KWS: line 19") })
	time.Sleep(50 * time.Millisecond)
	if n := h.log.count("KWS: line"); n != pryonStderrPerMinute {
		t.Fatalf("%d of 100 helper log lines logged, want %d", n, pryonStderrPerMinute)
	}
	if h.log.has("[pryon] [pryon]") {
		t.Error("prefix doubled")
	}
	var l lineLimiter
	l.perMinute = 2
	now := time.Unix(0, 0)
	for i := 0; i < 5; i++ {
		l.allow(now)
	}
	if ok, sup := l.allow(now.Add(time.Minute)); !ok || sup != 3 {
		t.Fatalf("next window: ok=%v suppressed=%d, want true 3", ok, sup)
	}
}

func TestPryonStatsLine(t *testing.T) {
	h := newHarness(t, "")
	h.ready()
	_ = h.push(command(cmdNear, h.e.fed, 0.7))
	h.until("the near miss", func() bool { return h.e.win.nearMiss == 1 })
	h.clk.Add(pryonStatsEvery)
	_ = h.push(silence())
	if !h.log.has("accepts=0 nearMiss=1 restarts=0 drops=0 stale=0") {
		t.Fatalf("no stats line:\n%s", h.log)
	}
}

func TestProcStatFields(t *testing.T) {
	stat := "4242 (pryon wake) (x)) S 1 4242 4242 0 -1 4194304 100 0 0 0 731 52 0 0 25 5 4 0 1234\n"
	f, ok := procStatFields(stat)
	if !ok || f[0] != "S" || f[1] != "1" || f[11] != "731" || f[12] != "52" {
		t.Fatalf("fields %v", f)
	}
	dir := t.TempDir()
	if err := os.MkdirAll(dir+"/4242", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/4242/stat", []byte(stat), 0o644); err != nil {
		t.Fatal(err)
	}
	if ticks, ok := readProcCPU(dir, 4242); !ok || ticks != 783 {
		t.Fatalf("ticks %d ok %v", ticks, ok)
	}
}

// --- through the Scorer -----------------------------------------------------------

// TestScorerOnPryonReportsTheMappedCaptureTime runs the whole chain the server
// runs: Scorer.PushBytesAt -> run() -> pryonEngine -> helper -> W line ->
// onCross. The crossing must carry Pryon's score and the capture time of the
// frame three before the one the word ended in — and, at the barge-in bar,
// fire on the second held frame, which is what the two-frame hold is for.
//
// The engine belongs to the scorer goroutine here, so the test never touches
// it: it knows the helper's sample positions because the first frame after
// which the Scorer reports Ready is the first one the engine sent (sample 0).
func TestScorerOnPryonReportsTheMappedCaptureTime(t *testing.T) {
	h := newHarness(t, "")
	type cross struct {
		score, bar float32
		at         time.Time
	}
	got := make(chan cross, 4)
	s := newScorer(h.e, 0.5, func(score, bar float32, at time.Time) { got <- cross{score, bar, at} })
	defer s.Close()
	s.SetBargeThreshold(0.3, func() bool { return true })

	var stamps []time.Time // stamps[i]: capture time of helper frame i
	pushOne := func(pcm []int16) {
		t.Helper()
		before := s.processed()
		at := h.clk.Add(80 * time.Millisecond)
		b := make([]byte, 2*len(pcm))
		for i, v := range pcm {
			b[2*i], b[2*i+1] = byte(uint16(v)), byte(uint16(v)>>8)
		}
		s.PushBytesAt(b, at)
		waitFor(t, "a frame to be scored", func() bool { return s.processed() > before })
		if s.Ready() {
			stamps = append(stamps, at)
		}
	}
	for i := 0; !s.Ready(); i++ {
		if i > 2000 {
			t.Fatal("scorer never ready")
		}
		pushOne(silence())
		time.Sleep(2 * time.Millisecond)
	}
	for len(stamps) < 10 {
		pushOne(silence())
	}
	// The command frame is helper frame 10; the word ends inside frame 8.
	end := uint64(8*pryonFrameSamples + 100)
	pushOne(command(cmdWake, end, 0.93))
	var c cross
	deadline := time.Now().Add(5 * time.Second)
wait:
	for {
		select {
		case c = <-got:
			break wait
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no crossing\n%s", h.log)
		}
		pushOne(silence())
	}
	if c.score < 0.929 || c.score > 0.931 || c.bar != 0.3 || !c.at.Equal(stamps[5]) {
		t.Fatalf("crossing %+v, want score 0.93 at the barge bar 0.3 at frame 5's stamp %v", c, stamps[5])
	}
	waitFor(t, "audio_playback=1 at the helper", func() bool { return h.log.has("got P audio_playback=1") })
	pushOne(silence())
	if st := s.Drain(); st.Crossings != 1 {
		t.Fatalf("%d crossings, want 1", st.Crossings)
	}
}
