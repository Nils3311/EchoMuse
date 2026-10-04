package shadow

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// buildRealHelper compiles device/tools/pryon_wake and its stub libpryon for
// this host, or skips: the test is about the real helper's side of protocol
// v1, and needs nothing but a C compiler (no device, no Amazon file).
func buildRealHelper(t *testing.T) (helper, lib string) {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("no C compiler (cc) on this host: the real pryon_wake cannot be built here")
	}
	src := filepath.Join("..", "..", "..", "tools", "pryon_wake")
	dir := t.TempDir()
	helper = filepath.Join(dir, "pryon_wake")
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	lib = filepath.Join(dir, "libstub_pryon"+ext)
	for _, args := range [][]string{
		{"-O1", "-std=gnu11", filepath.Join(src, "pryon_wake.c"), "-o", helper, "-ldl", "-lpthread"},
		{"-O1", "-shared", "-fPIC", filepath.Join(src, "test", "stub_pryon.c"), "-o", lib},
	} {
		if out, err := exec.Command(cc, args...).CombinedOutput(); err != nil {
			t.Skipf("building the real pryon_wake for this host failed (%v): %s", err, out)
		}
	}
	return helper, lib
}

// burst is a frame whose first n samples are at amplitude a and the rest
// silent. The stub library reports a keyword where a loud run of 400+ samples
// ends: >= 20000 STOP, >= 4000 ALEXA accepted, >= 1500 ALEXA near miss.
func burst(a int16, n int) []int16 {
	f := silence()
	for i := 0; i < n; i++ {
		f[i] = a
	}
	return f
}

// TestRealHelperEndToEnd runs the engine against the real pryon_wake on the
// stub library: READY after the warm-up, the end index of a detection minus
// the warm-up noise, the at mapping, the score from the library's log line,
// STOP filtered out, a near miss counted, the property reaching the library,
// and a clean exit on Close.
func TestRealHelperEndToEnd(t *testing.T) {
	helper, lib := buildRealHelper(t)
	h := newHarnessWith(t, "", func(c *pryonConfig) {
		c.Helper, c.Lib, c.Manifest, c.WarmupMs = helper, lib, "/stub/pryon.manifest", 1000
	})
	h.ready()
	h.e.SetPlayback(true)
	for i := 0; i < 10; i++ {
		if err := h.push(silence()); err != nil {
			t.Fatal(err)
		}
	}
	h.until("the property at the library", func() bool { return h.log.has("stub: property audio_playback=1") })

	// The keyword ends 800 samples into frame k: at must be frame k-3's stamp.
	k := h.e.fed / pryonFrameSamples
	if err := h.push(burst(8000, 800)); err != nil {
		t.Fatal(err)
	}
	h.until("the wake", func() bool { return h.e.hold.left > 0 })
	want, ok := h.e.ring.lookup((k - 3) * pryonFrameSamples)
	if !ok {
		t.Fatal("frame k-3 not in the ring")
	}
	score, at, _ := h.e.Score()
	if score < 0.949 || score > 0.951 || !at.Equal(want.at) || want.start != (k-3)*pryonFrameSamples {
		t.Fatalf("wake score %.3f at %v, want 0.950 at frame %d's stamp %v", score, at, k-3, want.at)
	}

	h.e.Score() // second held frame
	if err := h.push(burst(25000, 800)); err != nil {
		t.Fatal(err)
	}
	if err := h.push(burst(2000, 800)); err != nil {
		t.Fatal(err)
	}
	h.until("the near miss", func() bool { return h.e.win.nearMiss == 1 })
	if h.e.win.accepts != 1 || h.e.hold.left != 0 {
		t.Fatalf("STOP counted or held: accepts=%d hold=%d", h.e.win.accepts, h.e.hold.left)
	}

	p := h.e.p
	h.e.Close()
	if p.killedByUs || p.exitErr != nil {
		t.Fatalf("pryon_wake did not exit cleanly on EOF: killed=%v err=%v", p.killedByUs, p.exitErr)
	}
}

// TestRealHelperMissingLibraryFailsOver: the real helper's E LIB is fatal.
func TestRealHelperMissingLibraryFailsOver(t *testing.T) {
	helper, _ := buildRealHelper(t)
	h := newHarnessWith(t, "", func(c *pryonConfig) {
		c.Helper, c.Lib, c.Manifest = helper, "/nonexistent/libpryon.so", "/stub/pryon.manifest"
	})
	h.until("failover", func() bool { return h.e.inner != nil })
	if h.e.restarts != 0 {
		t.Errorf("restarted %d times on a fatal error", h.e.restarts)
	}
}
