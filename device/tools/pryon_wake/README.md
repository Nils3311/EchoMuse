# pryon_wake — the Echo's own wake word engine, as a helper process

Runs Amazon's Pryon wake word engine (`/system/lib/libpryon.so`, which the Echo
Dot Gen 2 already carries) for the EchoMuse server. The server starts it as a
child process, writes the wake stream to its stdin and reads detections from
its stdout. Inside the server it is one more engine behind `shadow.Scorer`
(`device/internal/wakeword/shadow/pryon.go`). The queue, thresholds, barge-in
rule, refractory period and on-device listening are the same as with
openWakeWord.

## Why

On a real capture from the Dot's centre mic (9 × "Alexa, wie spät ist es?",
one speaker, one room), Pryon found 9 of 9 and openWakeWord's `alexa_v0.1`
found 4 of 9 at 0.5. False alarms (TV, conversation) have **not** been
measured yet for either engine.

Why a separate process: `libpryon.so` is reverse engineered, with its own
threads and C++ exceptions. If it crashes, the helper restarts and the server
does not go down with it. The library is `dlopen()`ed, so building this needs
no Amazon file.

## Build

For the device (ARMv7, Android API 22, same toolchain as the server), in the
`echomuse-compiler` image:

```bash
device/tools/pryon_wake/build.sh       # -> device/tools/pryon_wake/pryon_wake
```

For the host, against the stub library, to test the protocol:

```bash
device/tools/pryon_wake/test/test_pryon_wake.py
cd device && go test ./internal/wakeword/shadow/ -run RealHelper
```

## Install on the device

Nothing to do since v2.18.2: the release build compiles the helper and embeds
it in the server, which writes it to `/data/local/share/echomuse/pryon/pryon_wake`
before starting Pryon (only when it is missing or differs;
`device/internal/wakeword/shadow/pryon_install.go`). A helper you built
yourself is overwritten there — run it from another path and point
`EM_PRYON_HELPER` at it. The server's shadow stats carry `engine`, which says
whether Pryon really runs or why it fell back.

By hand (servers before v2.18.2), from the dashboard terminal, with the binary
served from your machine:

```sh
busybox mkdir -p /data/local/share/echomuse/pryon && cd /data/local/share/echomuse/pryon
busybox wget -O pryon_wake.part http://<your-machine>:8000/pryon_wake && busybox chmod 755 pryon_wake.part && busybox mv pryon_wake.part pryon_wake
```

You also need a server build that contains the Pryon engine. To check the
helper by itself first, run
`./pryon_wake -f some.wav -x 1`, which feeds a 16 kHz mono WAV in real time
and prints the same lines as in service.

## Turning it on

1. Upload a copy of openWakeWord's `alexa_v0.1.onnx` as a custom model named
   **`pryon_alexa.onnx`** (dashboard → "+ Custom model").
2. In the **same** save, select it and set wake word detection to **"On this
   Echo"** (or `shadow` to compare without acting). Pryon only runs where the
   device scores the wake stream. In "On the controller" mode nothing changes.

The server picks Pryon for any model whose name starts with `pryon_`. If
Pryon cannot run, the server uses openWakeWord with that same
`pryon_alexa.onnx`, which is a working Alexa model. That happens when the
helper, library or manifest is missing, when no helper has said `READY`
within 20 s of the first start (generous for a cold boot; a restarted helper
gets 5 s, and missing that counts as a crash), on a fatal error line, or
after 5 crashes in a row. Pryon is only started where openWakeWord's runtime
and models are installed, so the fallback can take over; if they are not,
the device reports degraded, as it does today. Once on openWakeWord, the
device stays there until the model changes or the server restarts. The server
log names the engine and, if it fell back, the reason. Lines from the helper
start with `[pryon]`, including one every 30 s with `cpu=`, `accepts=`,
`nearMiss=`, `restarts=`, `drops=`, `stale=` and the latency from word end
to detection.

**Kill switch**, no new build needed: `touch /data/local/etc/echomuse/no_pryon`
(or `EM_PRYON=0`), then change the model or restart the server.

Settings (environment): `EM_PRYON_HELPER`, `EM_PRYON_LIB`, `EM_PRYON_MANIFEST`,
`EM_PRYON_WARMUP_MS` (3000), `EM_PRYON_RESET` (0: a mic restart is ignored by
the decoder; 1: the decoder session restarts and warms up again),
`EM_PRYON_AT_OFFSET_FRAMES` (3; see `wakeAt` in `pryon.go`),
`EM_PRYON_READY_TIMEOUT_MS` (20000, the first start only).

## Protocol (version 1)

The authoritative description is the comment at the top of `pryon_wake.c`.

- stdin: frames `'P' 'W' <type> <flags> <len:u32 LE> <payload>`. Type `A` is
  audio (S16LE mono 16 kHz), type `P` is a client property such as
  `audio_playback=1`, type `R` marks a gap in the audio, and EOF means shut
  down.
- stdout: `READY v=1 …` · `W <seq> ALEXA <type> <begin> <end> <score>`
  (accepted) · `N ALEXA <begin> <end> <score>` (near miss, only counted) ·
  `E <code> <text>` (fatal; LIB, SYM, MODEL, DECODER and ABI stop the server
  from trying Pryon again).
- stderr: the library's log lines.

`begin` and `end` count real audio samples since start. The warm-up noise is
not counted. Only `ALEXA` is reported. `STOP`, which the same model also
detects, is filtered out.

## Limits

- **The sensitivity slider no longer controls detection.** Pryon decides with
  its own threshold (0.923, or 0.864 while audio plays). The score it reports
  then has to clear the dashboard threshold as well, so the slider can only
  make Pryon *stricter*. At the default of 0.5 it has no effect.
- `libpryon.so` and its models belong to Amazon. They are **not** shipped
  here. The helper uses the copies already in the device's `/system`.
  `pryon_api.h` is a reverse-engineered subset (MIT, from echo-dot-assist).
- **No heartbeat.** A helper that keeps reading audio while the library's
  decoder has stopped producing results is not detected: no crash, no
  restart, just no wakes. (One that stops reading is restarted after 2 s.)
- **Several Echoes: Pryon wins the arbitration.** The time a Pryon wake
  reports is the capture time of the frame three frames before the word end,
  so that the session does not cut off the start of the command. The
  controller also reads that time as "heard at" for "first to hear wins".
  openWakeWord reports the frame where its score crossed, which is at or
  after the word end. A Pryon Echo therefore claims to have heard the same
  word about 320 to 560 ms earlier than an openWakeWord Echo. With a 300 ms
  arbitration window it wins even from across the house, and the two may be
  counted as separate utterances, so both answer. With a single Echo this
  does not matter. Fixing it needs separate session-start and heard-at
  times, which means a change to `server.go`.
- Only the en-US ALEXA model is mapped (`pryon_alexa`). For any other model,
  set `EM_PRYON_MANIFEST`.
- **Inherited descriptors.** The server's microphone descriptor is opened
  without close-on-exec, so every child the server starts, this helper
  included, inherits it. While anyone holds it, the next server cannot open
  the microphone. `pryon_wake` closes fds 3–1023 as its very first statement,
  before loading anything. That leaves three gaps: a descriptor numbered
  1024 or higher, the microseconds between `exec` and that statement, and a
  wrapper script set as `EM_PRYON_HELPER` (the shell keeps the microphone).
  Do not use a wrapper. The proper fix is close-on-exec on the PCM
  descriptor in the server.
- **Privacy: nobody has checked whether `libpryon.so` itself sends anything
  over the network.** The library imports `socket`/`connect`. Before relying
  on "On this Echo" as private listening, check the running helper for
  sockets (`busybox ls -l /proc/<pid>/fd | busybox grep socket`,
  `cat /proc/<pid>/net/tcp`).
