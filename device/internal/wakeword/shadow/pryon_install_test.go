package shadow

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestInstallPryonHelperWritesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pryon", "pryon_wake")
	wrote, err := installPryonHelper(path, []byte("helper v1"))
	if err != nil || !wrote {
		t.Fatalf("installPryonHelper = %v, %v; want true, nil", wrote, err)
	}
	got, _ := os.ReadFile(path)
	if !bytes.Equal(got, []byte("helper v1")) {
		t.Fatalf("content = %q", got)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", st.Mode().Perm())
	}
	if _, err := os.Stat(path + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part left behind: %v", err)
	}
}

func TestInstallPryonHelperLeavesIdenticalFileAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pryon_wake")
	if err := os.WriteFile(path, []byte("same"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	wrote, err := installPryonHelper(path, []byte("same"))
	if err != nil || wrote {
		t.Fatalf("installPryonHelper = %v, %v; want false, nil", wrote, err)
	}
	st, _ := os.Stat(path)
	if !st.ModTime().Equal(old) {
		t.Fatalf("identical file was rewritten")
	}
}

func TestInstallPryonHelperReplacesDifferentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pryon_wake")
	if err := os.WriteFile(path, []byte("old helper"), 0o755); err != nil {
		t.Fatal(err)
	}
	wrote, err := installPryonHelper(path, []byte("new helper"))
	if err != nil || !wrote {
		t.Fatalf("installPryonHelper = %v, %v; want true, nil", wrote, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, []byte("new helper")) {
		t.Fatalf("content = %q", got)
	}
}

func TestInstallPryonHelperRestoresExecBit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pryon_wake")
	if err := os.WriteFile(path, []byte("same"), 0o644); err != nil {
		t.Fatal(err)
	}
	wrote, err := installPryonHelper(path, []byte("same"))
	if err != nil || !wrote {
		t.Fatalf("installPryonHelper = %v, %v; want true, nil", wrote, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %v, want 0755", st.Mode().Perm())
	}
}

func TestInstallPryonHelperNothingToInstall(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pryon_wake")
	wrote, err := installPryonHelper(path, nil)
	if err != nil || wrote {
		t.Fatalf("installPryonHelper(nil) = %v, %v; want false, nil", wrote, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file created without data")
	}
}

func TestInstallPryonHelperReportsUnwritableDir(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "pryon")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil { // a file where the directory should be
		t.Fatal(err)
	}
	if _, err := installPryonHelper(filepath.Join(blocker, "pryon_wake"), []byte("x")); err == nil {
		t.Fatal("want an error when the directory cannot be created")
	}
}

func withHelperSource(t *testing.T, data []byte) {
	t.Helper()
	old := pryonHelperSource
	pryonHelperSource = func() []byte { return data }
	t.Cleanup(func() { pryonHelperSource = old })
}

func TestEnsurePryonHelperInstallsEmbedded(t *testing.T) {
	t.Setenv("EM_PRYON_HELPER", "")
	withHelperSource(t, []byte("embedded"))
	path := filepath.Join(t.TempDir(), "pryon", "pryon_wake")
	note, err := ensurePryonHelper(pryonConfig{Helper: path})
	if err != nil || note == "" {
		t.Fatalf("ensurePryonHelper = %q, %v; want a note, nil", note, err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, []byte("embedded")) {
		t.Fatalf("content = %q", got)
	}
	// Second start: already in place, nothing to report.
	if note, err = ensurePryonHelper(pryonConfig{Helper: path}); err != nil || note != "" {
		t.Fatalf("second ensurePryonHelper = %q, %v; want \"\", nil", note, err)
	}
}

func TestEnsurePryonHelperLeavesOwnHelperAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my_helper")
	t.Setenv("EM_PRYON_HELPER", path)
	withHelperSource(t, []byte("embedded"))
	if note, err := ensurePryonHelper(pryonConfig{Helper: path}); err != nil || note != "" {
		t.Fatalf("ensurePryonHelper = %q, %v; want \"\", nil", note, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("own helper path was written")
	}
}

func TestEnsurePryonHelperWithoutEmbeddedHelper(t *testing.T) {
	t.Setenv("EM_PRYON_HELPER", "")
	withHelperSource(t, nil)
	path := filepath.Join(t.TempDir(), "pryon_wake")
	if note, err := ensurePryonHelper(pryonConfig{Helper: path}); err != nil || note != "" {
		t.Fatalf("ensurePryonHelper = %q, %v; want \"\", nil", note, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file created without an embedded helper")
	}
}

// openWithPryonInstalling prepares Open to install the helper itself: the
// "embedded" helper is this test binary (see TestMain), installed to helper.
func openWithPryonInstalling(t *testing.T, helper string) {
	t.Helper()
	pryonFiles(t)
	t.Setenv("EM_PRYON_HELPER", "")
	self, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	withHelperSource(t, self)
	old := pryonHelperPath
	pryonHelperPath = helper
	t.Cleanup(func() { pryonHelperPath = old })
}

func TestOpenInstallsEmbeddedHelper(t *testing.T) {
	stubOWW(t, nil)
	helper := filepath.Join(t.TempDir(), "pryon", "pryon_wake")
	openWithPryonInstalling(t, helper)
	s, err := Open(pryonModelPath, 0.5, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if !strings.HasPrefix(s.Info(), "pryon, model pryon_alexa") || !strings.Contains(s.Info(), "helper installed from server build") {
		t.Errorf("Info = %q", s.Info())
	}
	if st, err := os.Stat(helper); err != nil || st.Mode().Perm()&0o111 == 0 {
		t.Fatalf("helper not installed executable: %v", err)
	}
	waitFor(t, "the scorer to be ready", func() bool {
		s.Push(silence())
		time.Sleep(5 * time.Millisecond)
		return s.Ready()
	})
}

func TestOpenReportsFailedInstallInFallback(t *testing.T) {
	stems := stubOWW(t, nil)
	dir := t.TempDir()
	blocker := filepath.Join(dir, "pryon")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil { // a file where the directory should be
		t.Fatal(err)
	}
	openWithPryonInstalling(t, filepath.Join(blocker, "pryon_wake"))
	t.Setenv("EM_PRYON_FALLBACK", "oww") // the fallback is opt-in
	s, err := Open(pryonModelPath, 0.5, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if len(*stems) == 0 {
		t.Fatal("openWakeWord not loaded although the helper could not be installed")
	}
	if !strings.Contains(s.Info(), "pryon unavailable: helper not installed") || !strings.Contains(s.Info(), "install helper") {
		t.Errorf("Info = %q", s.Info())
	}
}
