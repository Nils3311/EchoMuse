package shadow

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
)

// The pryon_wake helper travels inside the server binary. The release build
// compiles device/tools/pryon_wake/pryon_wake.c into pryonbin/ just before
// `go build`, so every server that ships carries the helper built from the
// same commit, and the server puts it in place itself before starting Pryon.
// Without this the helper had to be copied onto each Echo by hand, and a
// device that missed that step fell back to openWakeWord without anyone
// noticing — the dashboard still said "pryon alexa".
//
// pryonbin/ holds only .gitkeep in the repository: a host build or test, and
// any build that did not compile the helper, embeds nothing and behaves as
// before (helper expected on the device, fallback if it is not there).
//
//go:embed all:pryonbin
var pryonBin embed.FS

const pryonBinName = "pryonbin/pryon_wake"

// pryonHelperSource yields the helper to install; a variable so tests can
// stand in for a build that embeds one.
var pryonHelperSource = embeddedPryonHelper

// embeddedPryonHelper is the helper built into this server, or nil if the
// build did not include one.
func embeddedPryonHelper() []byte {
	b, err := pryonBin.ReadFile(pryonBinName)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// installPryonHelper makes path hold exactly data, executable. It does
// nothing if the file is already identical, and otherwise writes a sibling
// .part file and renames it over path, so a helper that is running keeps its
// old inode and a reader never sees a half-written file. There is no fsync:
// a file left empty by a power cut differs from the embedded one and is
// rewritten on the next start. Reports whether it wrote anything.
func installPryonHelper(path string, data []byte) (bool, error) {
	if len(data) == 0 {
		return false, nil
	}
	if st, err := os.Stat(path); err == nil && st.Size() == int64(len(data)) && st.Mode().Perm()&0o111 != 0 {
		if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, data) {
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, fmt.Errorf("pryon: install helper: %w", err)
	}
	tmp := path + ".part"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("pryon: install helper: %w", err)
	}
	// WriteFile leaves the mode of an existing file and applies the umask to
	// a new one; set it explicitly.
	if err := os.Chmod(tmp, 0o755); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("pryon: install helper: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("pryon: install helper: %w", err)
	}
	return true, nil
}

// ensurePryonHelper installs the embedded helper at cfg.Helper. A helper
// chosen through EM_PRYON_HELPER is someone's own and is left alone, as is
// everything when this build carries no helper. The returned note is for
// Info: "" when nothing was done.
func ensurePryonHelper(cfg pryonConfig) (string, error) {
	if os.Getenv("EM_PRYON_HELPER") != "" || cfg.Helper == "" {
		return "", nil
	}
	data := pryonHelperSource()
	if data == nil {
		return "", nil
	}
	wrote, err := installPryonHelper(cfg.Helper, data)
	if err != nil {
		return "", err
	}
	if wrote {
		return "helper installed from server build", nil
	}
	return "", nil
}
