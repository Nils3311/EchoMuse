package shadow

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Without EM_PRYON_FALLBACK=oww a device that cannot run Pryon fails Open
// (and reports degraded) instead of scoring with openWakeWord.
func TestOpenWithoutFallbackFails(t *testing.T) {
	stems := stubOWW(t, nil)
	pryonFiles(t)
	t.Setenv("EM_PRYON_FALLBACK", "")
	t.Setenv("EM_PRYON_LIB", "/nonexistent/libpryon.so")
	s, err := Open(pryonModelPath, 0.5, nil)
	if err == nil {
		s.Close()
		t.Fatalf("Open succeeded with %q, want an error", s.Info())
	}
	if !strings.Contains(err.Error(), "libpryon.so not found") || !strings.Contains(err.Error(), "no openWakeWord fallback") {
		t.Errorf("err = %v", err)
	}
	if len(*stems) != 0 {
		t.Errorf("openWakeWord loaded although the fallback is off: %v", *stems)
	}
}

// The kill switch is a deliberate choice of openWakeWord and still works.
func TestOpenKillSwitchStillUsesOpenWakeWord(t *testing.T) {
	stems := stubOWW(t, nil)
	pryonFiles(t)
	t.Setenv("EM_PRYON_FALLBACK", "")
	t.Setenv("EM_PRYON", "0")
	s, err := Open(pryonModelPath, 0.5, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if len(*stems) != 1 || !strings.Contains(s.Info(), "pryon disabled: EM_PRYON=0") {
		t.Errorf("stems %v Info %q", *stems, s.Info())
	}
}

// Open turns the fallback off on the engine it builds.
func TestOpenSetsNoFallback(t *testing.T) {
	stubOWW(t, nil)
	pryonFiles(t)
	for _, c := range []struct {
		env  string
		want bool
	}{{"", true}, {"oww", false}} {
		t.Run(fmt.Sprintf("EM_PRYON_FALLBACK=%q", c.env), func(t *testing.T) {
			t.Setenv("EM_PRYON_FALLBACK", c.env)
			s, err := Open(pryonModelPath, 0.5, nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()
			e, ok := s.eng.(*pryonEngine)
			if !ok {
				t.Fatalf("engine %T, want *pryonEngine", s.eng)
			}
			if e.noFallback != c.want {
				t.Errorf("noFallback = %v, want %v", e.noFallback, c.want)
			}
		})
	}
}

// A helper that keeps crashing is restarted for good, on the capped backoff,
// and openWakeWord is never loaded.
func TestPryonNoFallbackKeepsRestarting(t *testing.T) {
	h := newHarness(t, "crashstart")
	h.e.noFallback = true
	crashes := pryonMaxCrashes + 3
	for i := 0; i < crashes; i++ {
		h.until(fmt.Sprintf("crash %d noticed", i+1), func() bool { return h.e.p == nil })
		want := pryonBackoff[min(i, len(pryonBackoff)-1)]
		if got := h.e.nextStart.Sub(h.clk.Now()); got != want {
			t.Fatalf("crash %d: restart in %v, want %v", i+1, got, want)
		}
		_ = h.e.Push(silence(), h.clk.Add(want))
		if h.e.p == nil {
			t.Fatalf("crash %d: not restarted after %v", i+1, want)
		}
	}
	if h.fallbacks() != 0 || h.e.failing || h.e.inner != nil {
		t.Fatalf("fell back: fallbacks %d failing %v", h.fallbacks(), h.e.failing)
	}
	if h.e.restarts != uint64(crashes) {
		t.Errorf("restarts %d, want %d", h.e.restarts, crashes)
	}
	if !strings.HasPrefix(h.e.Info(), "pryon, ") {
		t.Errorf("Info = %q, want the Pryon line", h.e.Info())
	}
}

// A fatal error from the helper is a crash like any other: retried, no
// failover.
func TestPryonNoFallbackFatalIsRetried(t *testing.T) {
	h := newHarness(t, "fatal")
	h.e.noFallback = true
	h.until("fatal exit noticed", func() bool { return h.e.p == nil && h.e.restarts == 1 })
	if h.fallbacks() != 0 || h.e.failing {
		t.Fatalf("fell back on a fatal error")
	}
	if !strings.Contains(h.e.lastErr, "LIB fake: cannot load") {
		t.Errorf("lastErr = %q", h.e.lastErr)
	}
	_ = h.e.Push(silence(), h.clk.Add(pryonBackoff[0]+time.Millisecond))
	if h.e.p == nil {
		t.Fatal("not restarted after a fatal error")
	}
}

// Without a fallback openWakeWord's files are not needed: their absence must
// not keep Pryon from running.
func TestOpenWithoutFallbackDoesNotNeedOpenWakeWord(t *testing.T) {
	stems := stubOWW(t, nil)
	pryonFiles(t)
	t.Setenv("EM_PRYON_FALLBACK", "")
	t.Setenv("EM_OWW_DIR", t.TempDir()) // empty: no runtime, no models
	s, err := Open(pryonModelPath, 0.5, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if !strings.HasPrefix(s.Info(), "pryon, ") || len(*stems) != 0 {
		t.Errorf("Info %q stems %v", s.Info(), *stems)
	}
}

// The kill switch still needs openWakeWord's files, and says so.
func TestKillSwitchStillNeedsOpenWakeWord(t *testing.T) {
	stubOWW(t, nil)
	pryonFiles(t)
	t.Setenv("EM_PRYON", "0")
	dir := t.TempDir()
	t.Setenv("EM_OWW_DIR", dir)
	s, err := Open(pryonModelPath, 0.5, nil)
	if err == nil {
		s.Close()
		t.Fatalf("Open started %q with the kill switch and no openWakeWord", s.Info())
	}
}

// After five crashes in a row the log says plainly that this Echo is deaf.
func TestPryonNoFallbackWarnsOnce(t *testing.T) {
	h := newHarness(t, "crashstart")
	h.e.noFallback = true
	for i := 0; i < pryonMaxCrashes+2; i++ {
		h.until(fmt.Sprintf("crash %d", i+1), func() bool { return h.e.p == nil })
		_ = h.e.Push(silence(), h.clk.Add(pryonBackoff[min(i, len(pryonBackoff)-1)]))
	}
	if n := strings.Count(h.log.String(), "hears no wake word"); n != 1 {
		t.Errorf("deaf warning logged %d times, want 1:\n%s", n, h.log.String())
	}
}
