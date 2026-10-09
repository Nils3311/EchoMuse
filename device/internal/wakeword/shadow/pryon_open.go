package shadow

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PryonPrefix selects the Pryon engine: a wake word model whose stem starts
// with it ("pryon_alexa") runs on the device's libpryon.so instead of
// openWakeWord.
//
// The model is still a real .onnx file. The controller distributes a custom
// model named pryon_alexa.onnx like any other — it is a copy of
// alexa_v0.1.onnx — and that is exactly what this device falls back to when
// Pryon cannot run here. A controller and a dashboard that know nothing about
// Pryon therefore work unchanged, and a device that cannot run it still hears
// "Alexa".
const PryonPrefix = "pryon_"

// Where the pieces live on the device. The helper comes inside the server
// build and is put in place next to the openWakeWord files before Pryon starts
// (pryon_install.go); the library and the models are Amazon's and come with
// the device's /system, which is why none of them is distributed here.
const (
	DefaultPryonHelper = "/data/local/share/echomuse/pryon/pryon_wake"
	DefaultPryonLib    = "/system/lib/libpryon.so"
	// DefaultPryonWarmupMs of low noise is fed before the first real audio:
	// measured, without about three seconds of lead-in the engine misses a
	// wake word in the first seconds.
	DefaultPryonWarmupMs = 3000
)

// pryonManifests maps a model stem to its keyword model. EM_PRYON_MANIFEST
// overrides it, for trying another model without a new build.
var pryonManifests = map[string]string{
	"pryon_alexa": "/system/local/models/keyword/en-US/ALEXA/pryon.manifest",
}

// pryonKillSwitch: if this file exists, Pryon is not used, whatever the model
// says — the way to turn it off on a device without a new build or a
// controller change (EM_PRYON=0 does the same from the environment).
// Variables only so tests can point them elsewhere.
var (
	pryonKillSwitch = "/data/local/etc/echomuse/no_pryon"
	// pryonHelperPath is where the embedded helper is installed and run from
	// unless EM_PRYON_HELPER names another one.
	pryonHelperPath = DefaultPryonHelper
	killPid         = func(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
)

// pryonFallbackAllowed: whether a device that cannot run Pryon may score with
// openWakeWord instead. Off unless EM_PRYON_FALLBACK=oww — a fleet that is
// meant to run Pryon everywhere wants a device without it to be visibly
// broken (degraded, not answering), not quietly worse at hearing "Alexa".
// The kill switch below is a deliberate choice of openWakeWord and is not
// affected.
func pryonFallbackAllowed() bool { return os.Getenv("EM_PRYON_FALLBACK") == "oww" }

// pryonDisabled reports whether the kill switch is set, and which one.
func pryonDisabled() (bool, string) {
	if os.Getenv("EM_PRYON") == "0" {
		return true, "EM_PRYON=0"
	}
	if _, err := os.Stat(pryonKillSwitch); err == nil {
		return true, pryonKillSwitch + " exists"
	}
	return false, ""
}

// pryonConfigFor reads the configuration for stem from the environment.
func pryonConfigFor(stem string) (pryonConfig, error) {
	cfg := pryonConfig{
		Helper:         envOr("EM_PRYON_HELPER", pryonHelperPath),
		Lib:            envOr("EM_PRYON_LIB", DefaultPryonLib),
		Manifest:       os.Getenv("EM_PRYON_MANIFEST"),
		WarmupMs:       DefaultPryonWarmupMs,
		AtOffsetFrames: DefaultPryonAtOffsetFrames,
	}
	if cfg.Manifest == "" {
		cfg.Manifest = pryonManifests[stem]
	}
	if cfg.Manifest == "" {
		return cfg, fmt.Errorf("pryon: no manifest for %q (set EM_PRYON_MANIFEST)", stem)
	}
	var err error
	if cfg.WarmupMs, err = envInt("EM_PRYON_WARMUP_MS", DefaultPryonWarmupMs, 0, 30000); err != nil {
		return cfg, err
	}
	if cfg.ResetMode, err = envInt("EM_PRYON_RESET", 0, 0, 1); err != nil {
		return cfg, err
	}
	if cfg.AtOffsetFrames, err = envInt("EM_PRYON_AT_OFFSET_FRAMES", DefaultPryonAtOffsetFrames, 0, 12); err != nil {
		return cfg, err
	}
	if cfg.FirstReadyMs, err = envInt("EM_PRYON_READY_TIMEOUT_MS", int(pryonFirstReadyTimeout/time.Millisecond), 1000, 300000); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def, lo, hi int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return def, fmt.Errorf("pryon: %s=%q, want %d..%d", key, v, lo, hi)
	}
	return n, nil
}

// checkPryon is the synchronous, cheap part of starting Pryon: is everything
// it needs on this device. Done before the helper starts so the log names the
// missing piece, and so the common "not installed" case falls back to
// openWakeWord at once instead of after a crash and a timeout.
func checkPryon(cfg pryonConfig) error {
	st, err := os.Stat(cfg.Helper)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("pryon: helper not installed at %s", cfg.Helper)
	}
	if _, err := os.Stat(cfg.Lib); err != nil {
		return fmt.Errorf("pryon: libpryon.so not found at %s", cfg.Lib)
	}
	if _, err := os.Stat(cfg.Manifest); err != nil {
		return fmt.Errorf("pryon: model manifest not found at %s", cfg.Manifest)
	}
	return nil
}

// killOrphanHelpers SIGKILLs helpers left over from an earlier server: a
// pryon_wake whose parent is init. One that survived its server (PDEATHSIG and
// stdin EOF both failed) may still hold a copy of the microphone's descriptor
// from before it closed them, and pcm_open blocks while anyone holds the PCM —
// the "deaf device and a hissing amplifier" incident. Only orphans, so a
// helper someone started by hand from the dashboard terminal (whose parent is
// that shell) is left alone. A no-op where /proc is not there.
func killOrphanHelpers(base string, logf func(string, ...any)) int {
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return 0
	}
	killed := 0
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		// Never this process: where the helper is the server's own binary (the
		// tests run the test binary as a fake helper), an orphaned server
		// would otherwise match.
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join(procRoot, ent.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		argv0 := string(bytes.SplitN(cmdline, []byte{0}, 2)[0])
		if filepath.Base(argv0) != base {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(procRoot, ent.Name(), "stat"))
		if err != nil {
			continue
		}
		f, ok := procStatFields(string(stat))
		if !ok || len(f) < 2 || f[1] != "1" {
			continue
		}
		if err := killPid(pid); err == nil {
			killed++
			logf("[pryon] killed orphaned %s (pid %d)", base, pid)
		}
	}
	return killed
}

// openPryonEngine checks that Pryon can run here, clears out orphans and
// starts the helper. It returns as soon as the helper has been STARTED, not
// when it is ready: Open runs on the control link's read loop, and waiting the
// up to five seconds a helper may take to say READY would hold up pings and
// listen_close behind it. The engine reports not ready until
// READY arrives, and a helper that never says it is handled like a crash —
// on the first start, by falling back to openWakeWord.
//
// note, when not empty, is appended to Info before the helper starts, so a
// failover that sets its own Info cannot be overwritten by it afterwards.
func openPryonEngine(stem string, cfg pryonConfig, fallback func() (engine, error), note string) (*pryonEngine, error) {
	if err := checkPryon(cfg); err != nil {
		return nil, err
	}
	e := newPryonEngine(stem, cfg, fallback)
	e.noFallback = !pryonFallbackAllowed()
	if note != "" {
		e.setInfo(e.Info() + ", " + note)
	}
	killOrphanHelpers(filepath.Base(cfg.Helper), e.logf)
	err := e.start(e.now())
	if errors.Is(err, syscall.ETXTBSY) {
		// The helper was just written. If another goroutine forked while the
		// write descriptor was open, the child still holds it until it execs
		// (golang.org/issue/22315); that is over within milliseconds.
		time.Sleep(50 * time.Millisecond)
		err = e.start(e.now())
	}
	if err != nil {
		_ = e.Close()
		return nil, fmt.Errorf("pryon: helper did not start: %w", err)
	}
	return e, nil
}

// openWithPryon is Open for a pryon_ model: Pryon if it can run here,
// otherwise openWakeWord on the same <stem>.onnx, with the reason in Info.
func openWithPryon(dir, stem string, threshold float32,
	onCross func(score, threshold float32, at time.Time)) (*Scorer, error) {
	// openWakeWord's files are checked first only where openWakeWord may be
	// used: with EM_PRYON_FALLBACK=oww it is what a Pryon failure at run time
	// falls back to, in the background, long after this call has returned,
	// and without it a failover would leave a loaded scorer that never scores.
	// Without a fallback (the default) Pryon does not need them, and their
	// absence must not keep Pryon from running. The kill switch below checks
	// them itself. Only a cheap check: loading ORT here would cost 12MB.
	if pryonFallbackAllowed() {
		if err := checkOWW(dir, stem); err != nil {
			return nil, err
		}
	}
	var reason string
	if off, why := pryonDisabled(); off {
		if err := checkOWW(dir, stem); err != nil {
			return nil, err
		}
		reason = "disabled: " + why
	} else {
		cfg, err := pryonConfigFor(stem)
		var installed string
		if err == nil {
			// A failed install is not fatal by itself: an older helper may
			// still be there, and checkPryon below decides.
			var ierr error
			if installed, ierr = ensurePryonHelper(cfg); ierr != nil {
				installed = strings.TrimPrefix(ierr.Error(), "pryon: ")
			}
		}
		if err == nil {
			fallback := func() (engine, error) {
				o, err := openOWW(dir, stem)
				if err != nil {
					return nil, err // not a typed nil inside the interface
				}
				return o, nil
			}
			var eng *pryonEngine
			if eng, err = openPryonEngine(stem, cfg, fallback, installed); err == nil {
				return newScorer(eng, threshold, onCross), nil
			}
		}
		reason = "unavailable: " + strings.TrimPrefix(err.Error(), "pryon: ")
		if installed != "" {
			reason += "; " + installed
		}
		if !pryonFallbackAllowed() {
			return nil, fmt.Errorf("pryon %s (no openWakeWord fallback; EM_PRYON_FALLBACK=oww allows one)", reason)
		}
	}
	o, err := openOWW(dir, stem)
	if err != nil {
		return nil, fmt.Errorf("%w (pryon %s)", err, reason)
	}
	o.info = fmt.Sprintf("%s (pryon %s)", o.info, reason)
	return newScorer(o, threshold, onCross), nil
}
