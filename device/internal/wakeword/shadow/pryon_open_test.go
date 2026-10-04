package shadow

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubOWW replaces the openWakeWord loader (ONNX Runtime is not on a host)
// and records which stem it was asked for.
func stubOWW(t *testing.T, fail error) *[]string {
	t.Helper()
	var mu sync.Mutex
	var stems []string
	old := openOWW
	openOWW = func(dir, stem string) (*owwEngine, error) {
		mu.Lock()
		stems = append(stems, stem)
		mu.Unlock()
		if fail != nil {
			return nil, fail
		}
		o := newOWWEngine(&fakeInferer{})
		o.info = "fake onnxruntime, model " + stem
		return o, nil
	}
	t.Cleanup(func() { openOWW = old })
	return &stems
}

// pryonFiles makes a helper, library and manifest that pass checkPryon, and
// points the environment at them. The helper is this test binary (see
// TestMain), so the engine can really start it.
func pryonFiles(t *testing.T) (lib, manifest string) {
	t.Helper()
	dir := t.TempDir()
	lib, manifest = filepath.Join(dir, "libpryon.so"), filepath.Join(dir, "pryon.manifest")
	for _, p := range []string{lib, manifest} {
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("EM_PRYON_HELPER", os.Args[0])
	t.Setenv("EM_PRYON_LIB", lib)
	t.Setenv("EM_PRYON_MANIFEST", manifest)
	t.Setenv("EM_PRYON_WARMUP_MS", "0")
	t.Setenv("GO_WANT_HELPER", "1")
	t.Setenv("FAKE_PRYON", "")
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("EM_PRYON", "")
	// openWakeWord's files, which Open requires before it starts Pryon.
	oww := filepath.Join(dir, "oww")
	os.MkdirAll(oww, 0o755)
	for _, f := range []string{"libonnxruntime.so", "melspectrogram.onnx", "embedding_model.onnx", "pryon_alexa.onnx", "alexa_v0.1.onnx", "hey_pryon.onnx"} {
		os.WriteFile(filepath.Join(oww, f), nil, 0o644)
	}
	t.Setenv("EM_OWW_DIR", oww)
	old := pryonKillSwitch
	pryonKillSwitch = filepath.Join(dir, "no_pryon")
	t.Cleanup(func() { pryonKillSwitch = old })
	return lib, manifest
}

const pryonModelPath = "/app/data/oww_models/pryon_alexa.onnx"

func TestOpenRunsPryonForAPryonModel(t *testing.T) {
	stems := stubOWW(t, nil)
	pryonFiles(t)
	before := runtime.NumGoroutine()
	s, err := Open(pryonModelPath, 0.5, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !strings.HasPrefix(s.Info(), "pryon, model pryon_alexa") {
		t.Errorf("Info = %q", s.Info())
	}
	if len(*stems) != 0 {
		t.Errorf("openWakeWord loaded although Pryon started: %v", *stems)
	}
	// Not ready until the helper says READY, which arrives asynchronously.
	waitFor(t, "the scorer to be ready", func() bool {
		s.Push(silence())
		time.Sleep(5 * time.Millisecond)
		return s.Ready()
	})
	s.Close()
	waitFor(t, "goroutines to finish", func() bool { return runtime.NumGoroutine() <= before })
}

// TestOpenFallsBackToOpenWakeWord: every way Pryon can be unavailable ends in
// openWakeWord on the SAME <stem>.onnx — the pseudo-model the controller has
// already put on the device — with the reason in Info, logged once by the
// caller.
func TestOpenFallsBackToOpenWakeWord(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(t *testing.T, lib, manifest string)
		reason string
	}{
		{"helper missing", func(t *testing.T, _, _ string) {
			t.Setenv("EM_PRYON_HELPER", "/nonexistent/pryon_wake")
		}, "pryon unavailable: helper not installed at /nonexistent/pryon_wake"},
		{"helper not executable", func(t *testing.T, lib, _ string) {
			t.Setenv("EM_PRYON_HELPER", lib)
		}, "helper not installed at"},
		{"library missing", func(t *testing.T, _, _ string) {
			t.Setenv("EM_PRYON_LIB", "/nonexistent/libpryon.so")
		}, "pryon unavailable: libpryon.so not found at /nonexistent/libpryon.so"},
		{"manifest missing", func(t *testing.T, _, _ string) {
			t.Setenv("EM_PRYON_MANIFEST", "/nonexistent/pryon.manifest")
		}, "model manifest not found at /nonexistent/pryon.manifest"},
		{"bad setting", func(t *testing.T, _, _ string) {
			t.Setenv("EM_PRYON_RESET", "2")
		}, "EM_PRYON_RESET"},
		{"switched off by environment", func(t *testing.T, _, _ string) {
			t.Setenv("EM_PRYON", "0")
		}, "pryon disabled: EM_PRYON=0"},
		{"switched off by file", func(t *testing.T, _, _ string) {
			if err := os.WriteFile(pryonKillSwitch, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, "pryon disabled: " + "", // path checked below
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stems := stubOWW(t, nil)
			lib, manifest := pryonFiles(t)
			c.setup(t, lib, manifest)
			s, err := Open(pryonModelPath, 0.5, nil)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer s.Close()
			if len(*stems) != 1 || (*stems)[0] != "pryon_alexa" {
				t.Fatalf("openWakeWord loaded for %v, want [pryon_alexa]", *stems)
			}
			info := s.Info()
			if !strings.HasPrefix(info, "fake onnxruntime, model pryon_alexa (pryon ") ||
				!strings.Contains(info, c.reason) {
				t.Errorf("Info = %q, want the openWakeWord engine and %q", info, c.reason)
			}
			if c.name == "switched off by file" && !strings.Contains(info, pryonKillSwitch+" exists") {
				t.Errorf("Info = %q does not name the kill switch file", info)
			}
		})
	}
}

// TestOpenNeedsTheFallbackFirst: Pryon is only started where openWakeWord
// could take over. If the fallback's files are missing, a later failover
// would leave a loaded scorer that never scores — deaf, but reported as
// "local". Refusing up front gives today's error, and the device reports
// degraded as it always did.
func TestOpenNeedsTheFallbackFirst(t *testing.T) {
	stubOWW(t, nil)
	pryonFiles(t)
	for _, missing := range []string{"libonnxruntime.so", "melspectrogram.onnx", "embedding_model.onnx", "pryon_alexa.onnx"} {
		t.Run(missing, func(t *testing.T) {
			dir := t.TempDir()
			for _, f := range []string{"libonnxruntime.so", "melspectrogram.onnx", "embedding_model.onnx", "pryon_alexa.onnx"} {
				if f != missing {
					os.WriteFile(filepath.Join(dir, f), nil, 0o644)
				}
			}
			t.Setenv("EM_OWW_DIR", dir)
			s, err := Open(pryonModelPath, 0.5, nil)
			if err == nil {
				s.Close()
				t.Fatalf("Open started %q without %s for the fallback", s.Info(), missing)
			}
			if !strings.Contains(err.Error(), filepath.Join(dir, missing)) {
				t.Errorf("error %q does not name %s", err, missing)
			}
		})
	}
}

// TestOpenFallbackFailureIsTodaysError: Pryon unavailable AND openWakeWord
// unavailable is the same error as before, so the device reports degraded
// exactly as it would have.
func TestOpenFallbackFailureIsTodaysError(t *testing.T) {
	oww := errors.New("shadow: classifier model not installed at /x/pryon_alexa.onnx")
	stubOWW(t, oww)
	pryonFiles(t)
	t.Setenv("EM_PRYON_LIB", "/nonexistent/libpryon.so")
	_, err := Open(pryonModelPath, 0.5, nil)
	if !errors.Is(err, oww) || !strings.Contains(err.Error(), "libpryon.so not found") {
		t.Fatalf("Open = %v", err)
	}
}

// TestOpenLeavesOtherModelsAlone: no prefix, no Pryon, not even a mention.
func TestOpenLeavesOtherModelsAlone(t *testing.T) {
	stems := stubOWW(t, nil)
	pryonFiles(t)
	for _, m := range []string{"alexa_v0.1", "/app/data/oww_models/hey_pryon.onnx"} {
		s, err := Open(m, 0.5, nil)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(s.Info(), "pryon (") || strings.Contains(s.Info(), "pryon unavailable") {
			t.Errorf("%s: Info = %q", m, s.Info())
		}
		s.Close()
	}
	if len(*stems) != 2 || (*stems)[0] != "alexa_v0.1" || (*stems)[1] != "hey_pryon" {
		t.Errorf("stems %v", *stems)
	}
}

func TestPryonConfigFromEnvironment(t *testing.T) {
	for _, k := range []string{"EM_PRYON_HELPER", "EM_PRYON_LIB", "EM_PRYON_MANIFEST",
		"EM_PRYON_WARMUP_MS", "EM_PRYON_RESET", "EM_PRYON_AT_OFFSET_FRAMES"} {
		t.Setenv(k, "")
	}
	cfg, err := pryonConfigFor("pryon_alexa")
	if err != nil {
		t.Fatal(err)
	}
	want := pryonConfig{Helper: DefaultPryonHelper, Lib: DefaultPryonLib,
		Manifest: "/system/local/models/keyword/en-US/ALEXA/pryon.manifest",
		WarmupMs: 3000, ResetMode: 0, AtOffsetFrames: 3}
	if cfg != want {
		t.Errorf("defaults %+v, want %+v", cfg, want)
	}
	if _, err := pryonConfigFor("pryon_hey_computer"); err == nil {
		t.Error("an unknown pryon_ model without EM_PRYON_MANIFEST got a manifest")
	}
	t.Setenv("EM_PRYON_MANIFEST", "/m")
	t.Setenv("EM_PRYON_WARMUP_MS", "1500")
	t.Setenv("EM_PRYON_RESET", "1")
	t.Setenv("EM_PRYON_AT_OFFSET_FRAMES", "4")
	cfg, err = pryonConfigFor("pryon_hey_computer")
	if err != nil || cfg.Manifest != "/m" || cfg.WarmupMs != 1500 || cfg.ResetMode != 1 || cfg.AtOffsetFrames != 4 {
		t.Errorf("overrides: %+v %v", cfg, err)
	}
	t.Setenv("EM_PRYON_WARMUP_MS", "soon")
	if _, err := pryonConfigFor("pryon_alexa"); err == nil {
		t.Error("a bad EM_PRYON_WARMUP_MS was accepted")
	}
}

// TestKillOrphanHelpers: only a pryon_wake whose parent is init is killed —
// not one started from a shell, not anything else.
func TestKillOrphanHelpers(t *testing.T) {
	root := t.TempDir()
	proc := func(pid, cmdline, stat string) {
		d := filepath.Join(root, pid)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "cmdline"), []byte(cmdline), 0o644)
		os.WriteFile(filepath.Join(d, "stat"), []byte(stat), 0o644)
	}
	proc("100", "/data/local/share/echomuse/pryon/pryon_wake\x00-l\x00/system/lib/libpryon.so\x00",
		"100 (pryon_wake) S 1 100 100 0 -1 0")
	proc("101", "./pryon_wake\x00-f\x00c6.wav\x00", "101 (pryon_wake) S 4711 101 101 0 -1 0")
	proc("102", "/data/local/bin/server\x00", "102 (server) S 1 102 102 0 -1 0")
	proc("103", "", "103 (kworker) S 2 0 0 0 -1 0")
	os.MkdirAll(filepath.Join(root, "self"), 0o755)
	// This process, as if it were an orphaned helper.
	self := strconv.Itoa(os.Getpid())
	proc(self, "/x/pryon_wake\x00", self+" (pryon_wake) S 1 0 0 0 -1 0")

	oldRoot, oldKill := procRoot, killPid
	t.Cleanup(func() { procRoot, killPid = oldRoot, oldKill })
	procRoot = root
	var killed []int
	killPid = func(pid int) error { killed = append(killed, pid); return nil }
	var logs logSink
	if n := killOrphanHelpers("pryon_wake", logs.logf); n != 1 || len(killed) != 1 || killed[0] != 100 {
		t.Fatalf("killed %v (n=%d), want [100]", killed, n)
	}
	if !logs.has("killed orphaned pryon_wake (pid 100)") {
		t.Errorf("not logged: %s", &logs)
	}
	procRoot = filepath.Join(root, "missing")
	if n := killOrphanHelpers("pryon_wake", logs.logf); n != 0 {
		t.Errorf("no /proc: killed %d", n)
	}
}
