#!/usr/bin/env python3
"""Protocol tests for pryon_wake against the stub library. Usage: ./test_pryon_wake.py
Builds pryon_wake and the stub for the host first."""
import os, struct, subprocess, sys, tempfile, time

HERE = os.path.dirname(os.path.abspath(__file__))
SO = ".dylib" if sys.platform == "darwin" else ".so"
tmp = tempfile.mkdtemp()
BIN, LIB = os.path.join(tmp, "pryon_wake"), os.path.join(tmp, "libstub_pryon" + SO)

def build():
    subprocess.check_call(["cc", "-O1", "-Wall", "-Wextra", "-std=gnu11", os.path.join(HERE, "..", "pryon_wake.c"), "-o", BIN, "-ldl"])
    subprocess.check_call(["cc", "-O1", "-Wall", "-shared", "-fPIC", os.path.join(HERE, "stub_pryon.c"), "-o", LIB])

def frame(typ, payload=b""):
    return b"PW" + typ + b"\0" + struct.pack("<I", len(payload)) + payload

def audio(samples):
    return frame(b"A", struct.pack("<%dh" % len(samples), *samples))

def burst(amp, n=800):
    return [amp] * n

def run(frames, args=(), env=None, lib=None, timeout=20):
    e = dict(os.environ, **(env or {}))
    p = subprocess.run([BIN, "-l", lib or LIB, "-w", "1000", "-n", "0", *args], input=b"".join(frames),
                       capture_output=True, timeout=timeout, env=e)
    return p.returncode, p.stdout.decode().splitlines(), p.stderr.decode()

fails = 0
def check(name, cond, detail=""):
    global fails
    print(("ok   " if cond else "FAIL ") + name + ("" if cond else "  " + detail))
    if not cond: fails += 1

build()

# 1. READY, warm-up noise subtracted, accepted ALEXA reported on real-audio indexes.
sil = [0] * 16000
rc, out, err = run([audio(sil), audio(burst(8000)), audio(sil[:3200])])
check("exit 0 on EOF", rc == 0, f"rc={rc} {err}")
check("READY first", out and out[0].startswith("READY v=1 warmup=1000"), str(out))
w = [l for l in out if l.startswith("W ")]
check("one wake", len(w) == 1, str(out))
if w:
    f = w[0].split()
    # burst occupies real samples 16000..16800; the stub reports the end when the burst stops
    check("keyword ALEXA type 2", f[2] == "ALEXA" and f[3] == "2", w[0])
    check("indexes exclude warm-up", int(f[4]) == 16000 and int(f[5]) == 16800, w[0])
    check("score from the log line", abs(float(f[6]) - 0.95) < 1e-6, w[0])

# 2. STOP is ignored, near miss is counted separately.
rc, out, err = run([audio(sil), audio(burst(25000)), audio(sil[:3200]), audio(burst(2000)), audio(sil[:3200])])
check("STOP ignored", not any(l.startswith("W ") for l in out), str(out))
check("near miss reported as N", any(l.startswith("N ALEXA") for l in out), str(out))

# 3. Properties and reset.
rc, out, err = run([frame(b"P", b"audio_playback=1"), audio(sil[:3200]), frame(b"R"), audio(sil[:3200])], args=("-r", "1"))
check("property pushed", "stub: property audio_playback=1" in err, err)
check("reset -r 1 ends the session", "stub: SessionEnd" in err, err)
rc, out, err = run([audio(sil[:3200]), frame(b"R")], args=("-r", "0"))
check("reset -r 0 does nothing", "SessionEnd" not in err, err)

# 4. Failure modes.
rc, out, err = run([b"XX" + b"\0" * 6])
check("bad magic -> E IO, exit 7", rc == 7 and any(l.startswith("E IO") for l in out), f"rc={rc} {out}")
rc, out, err = run([audio(sil)], lib="/nonexistent/libpryon.so")
check("missing library -> E LIB, exit 3", rc == 3 and any(l.startswith("E LIB") for l in out), f"rc={rc} {out}")
rc, out, err = run([audio(sil)], args=("-m", "/x/missing.manifest"))
check("bad manifest -> E MODEL, exit 4", rc == 4 and any(l.startswith("E MODEL") for l in out), f"rc={rc} {out}")
rc, out, err = run([audio(sil), audio(burst(8000)), audio(sil[:3200])], env={"STUB_BAD": "1"})
check("implausible result -> E ABI, exit 6", rc == 6 and any(l.startswith("E ABI") for l in out), f"rc={rc} {out}")
rc, out, err = run([frame(b"A", b"\0\0\0")])
check("odd audio length -> E IO", rc == 7, f"rc={rc} {out}")
rc, out, err = run([frame(b"Z", b"abc"), audio(sil[:3200])])
check("unknown frame type skipped", rc == 0, f"rc={rc} {out} {err}")

# 5. Large frames split across reads (a pipe delivers 2560 bytes in pieces).
data = b"".join([audio(sil[:1280]) for _ in range(20)])
p = subprocess.Popen([BIN, "-l", LIB, "-w", "0", "-n", "0"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
for i in range(0, len(data), 7):
    p.stdin.write(data[i:i + 7]); p.stdin.flush()
p.stdin.close(); p.wait(timeout=10)
check("frames reassembled across tiny writes", p.returncode == 0, f"rc={p.returncode}")

# 6. Server vanishes: stdout closed -> exits instead of hanging.
p = subprocess.Popen([BIN, "-l", LIB, "-w", "0", "-n", "0"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
p.stdout.close()
p.stdin.write(audio(burst(8000)) + audio(sil[:3200])); p.stdin.flush()
try:
    p.wait(timeout=10); check("exits when stdout is gone", True)
except subprocess.TimeoutExpired:
    p.kill(); check("exits when stdout is gone", False, "still running")

print("%d failure(s)" % fails)
sys.exit(1 if fails else 0)
