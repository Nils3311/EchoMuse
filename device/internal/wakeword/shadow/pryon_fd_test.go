package shadow

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// leakedFd opens a pipe WITHOUT close-on-exec, the way the microphone's PCM
// descriptor is opened (tinyalsa's open() in C, no O_CLOEXEC), and returns a
// probe: held reports whether some other process still has the write end,
// i.e. whether a read of the other end does NOT see EOF after this process
// has closed its own copy. Call release after the child has started.
func leakedFd(t *testing.T) (release func(), held func() bool) {
	t.Helper()
	var p [2]int
	syscall.ForkLock.RLock()
	err := syscall.Pipe(p[:])
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.SetNonblock(p[0], true); err != nil {
		t.Fatal(err)
	}
	r := os.NewFile(uintptr(p[0]), "leak-r")
	t.Cleanup(func() { r.Close() })
	release = func() { syscall.Close(p[1]) }
	held = func() bool {
		_ = r.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		var b [1]byte
		_, err := r.Read(b[:])
		return os.IsTimeout(err) // EOF: nobody holds the write end any more
	}
	return release, held
}

// TestHelperInheritsNonCloexecFds pins the platform fact the helper's design
// rests on: Go's exec passes ONLY stdin, stdout and stderr deliberately, but
// it does not close anything else — every descriptor without FD_CLOEXEC
// survives into the child. The fake helper (Go, no fd cleanup of its own)
// therefore holds the "microphone". This is why pryon_wake's first act is
// close_inherited_fds(), and why it must stay its first act.
func TestHelperInheritsNonCloexecFds(t *testing.T) {
	release, held := leakedFd(t)
	h := newHarness(t, "")
	release()
	h.ready()
	if !held() {
		t.Fatal("the child did not inherit a descriptor without FD_CLOEXEC — " +
			"if exec now closes them, the comments in spawn and pryon_wake.c are out of date")
	}
}

// TestRealHelperClosesInheritedFds: the real pryon_wake, given the same
// inherited "microphone", has let go of it by the time it says READY.
func TestRealHelperClosesInheritedFds(t *testing.T) {
	helper, lib := buildRealHelper(t)
	release, held := leakedFd(t)
	h := newHarnessWith(t, "", func(c *pryonConfig) {
		c.Helper, c.Lib, c.Manifest, c.WarmupMs = helper, lib, "/stub/pryon.manifest", 0
	})
	release()
	h.ready()
	if held() {
		t.Fatal("pryon_wake still holds a descriptor it inherited from the server")
	}
}
