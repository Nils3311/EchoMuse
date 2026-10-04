# Proposal: Pryon as an optional on-device wake word engine

Status: request for feedback, not a request to merge. Branch `pryon-backend`,
based on tag `v2.17.0`. All measurements come from **one Echo Dot 2nd gen**
(Fire OS 6 base, Android 7.1.2 / SDK 25 under emOS), firmware v2.17.0,
controller v2.25.0, one speaker, one room of living-room size. Where
something was not measured, the text says so.

## 1. Summary

The Echo Dot already carries Amazon's wake word engine, `libpryon.so`; this
branch lets the on-device scorer use it instead of openWakeWord, for the
"Alexa" wake word only. The library runs in a small helper process
(`pryon_wake`, C) that the server starts and talks to over pipes, and is only
`dlopen`ed from the device, never shipped. The existing `shadow.Scorer`
now drives an `engine` interface; openWakeWord is the first implementation and
behaves as before. A model named `pryon_*` selects Pryon, and openWakeWord
uses the same file if Pryon cannot run. On one recording from this device,
Pryon found 9 of 9 utterances where openWakeWord found 4 of 9 on the centre
microphone at its default threshold, but that is one recording and **false
alarm rates were not measured for either engine**. The diff is large (+4129
lines, 43% of it tests) with known gaps (section 7); please tell me whether
you want this direction at all, and in what shape (sections 10, 11), before
any further work.

## 2. Motivation

openWakeWord's `alexa_v0.1` missed a good share of "Alexa" utterances on the
centre microphone, the only channel EchoMuse scores while idle (`ch6`, see
`device/internal/beamformer/beamformer.go`). The Echo's firmware has its own
engine for exactly this word. Using it costs no model training and puts no
Amazon file in this repository. Lowering openWakeWord's threshold raises
recall, but I did not measure its false alarm cost either. Whether Pryon is
better in general, not just on one recording, is the open question.

## 3. Design

**3.1 Helper `pryon_wake`** (`device/tools/pryon_wake/pryon_wake.c`,
`pryon_api.h`). The library is reverse engineered, with its own threads and
C++ exceptions, so it runs in a child process: a crash costs a helper
restart, not the server (and with it the microphone and the dashboard
terminal). Protocol v1 is documented at the top of `pryon_wake.c` (binary frames on
stdin: audio, properties, gap; one text line each for `READY`, `W`, `N` and
`E` on stdout; the library's log on stderr). `begin`/`end` count real audio
samples. The helper first feeds ~3 s of low noise (`EM_PRYON_WARMUP_MS`), because
Pryon missed wake words in the first seconds of a stream otherwise (6.2). The
model set also detects `STOP`; only `ALEXA` is reported. `-f file.wav -x N`
is a test mode.

**3.2 Engine interface** (`shadow/engine.go`). The Scorer held a
`*wakeword.Detector`; it now holds an `engine` (`Push`, `Ready`, `Score`,
`Reset`, `SetPlayback`, `Info`, `Close`). `owwEngine` wraps the detector
unchanged. One semantic addition: `Score` also returns the capture time the
score refers to, because an asynchronous engine knows better than the queue
which audio a detection belongs to. `shadow.go` changes are that, plus
`ready = false` when an engine stops being ready without a `Reset`.
`NewScorer(inf, ...)` keeps its signature. This is a refactor with no
intended change for openWakeWord; the existing scorer tests run through it
untouched.

**3.4 Selection and fallback** (`open.go`, `pryon_open.go`). A model stem
starting with `pryon_` goes to `openWithPryon`. The model is still a real
`.onnx`: the user uploads a copy of `alexa_v0.1.onnx` named
`pryon_alexa.onnx` as a custom model. Controller and dashboard need no
change. The engine (`pryon.go`, `pryon_open.go`) never blocks the microphone
path: `Push` makes a non-blocking send into an 8-frame writer channel, and a
full channel is an error the Scorer counts. That same file is what the device falls back to, so a device that
cannot run Pryon still hears "Alexa". Pryon is started only where
openWakeWord could take over (`checkOWW`: runtime and three models present);
otherwise the device reports `degraded` as today (review finding B2, section
5).

**3.5 Kill switch:** `touch /data/local/etc/echomuse/no_pryon` or
`EM_PRYON=0`, then change the model or restart. No build needed.

**3.6 Timestamp formula** (`wakeAt` in `pryon.go`). A detection arrives
hundreds of ms after its audio, with a word end in helper samples. The
Scorer reports

    at = capture time of the frame holding sample (end - 3 * 1280)

because `listen.Gate.Open` sends every ringed frame captured strictly after
`at`, and the controller drops the first `VOICE_PREROLL_DISCARD` = 3 frames
(240 ms) of a wake-word session, a value tuned for openWakeWord, whose score
rises after the word ended. With the word ending in frame E: the gate sends
from E-2, the controller drops E-2..E, speech-to-text hears from E+1. No
interpolation, because the microphone stamps both 80 ms frames of a 160 ms
ALSA read with the same `time.Now()`. Checked on the recording of 6.1: in
all 9 cases "wie" starts 4 to 8 frames after the reported end. A command
spoken with no pause after "Alexa" was **not** tested end to end.
`EM_PRYON_AT_OFFSET_FRAMES` (default 3) exposes the constant.

**3.7 Reset and old detections.** An accepted wake is held for two scored
frames so the two-frame barge-in rule can see it. It is dropped if it is
from before the last `Reset`, older than the frame ring, or older than 1.2 s
when the scorer sees it (the listen gate rings 2 s). `EM_PRYON_RESET` chooses
whether a mic gap is ignored by the decoder (0, default) or restarts its
session with a new warm-up.

**3.8 Failover.** Helper, library or manifest missing: openWakeWord at once.
Fatal `E` code `LIB`, `SYM`, `MODEL`, `DECODER` or `ABI`: openWakeWord. No
`READY` within 20 s of the *first* start (`EM_PRYON_READY_TIMEOUT_MS`, generous
for a cold boot): openWakeWord; a restarted helper gets 5 s and a miss counts
as a crash. Crashes restart with backoff 1, 2, 4, 8, 16, 30 s; five in a
row, each within 60 s of starting: openWakeWord. A ready helper that refuses
every frame for 2 s, a foreign `SIGKILL` and an unreadable stdout all count
as crashes. After failover the device stays on openWakeWord until the model
changes or the server restarts; the log names the reason.

**3.9 Footnote: inherited file descriptors.** The microphone's PCM
descriptor is opened by tinyalsa in C without `O_CLOEXEC`, so every child
inherits it, and the next server cannot open the PCM while any process holds
it. A manual recording attempt during the investigation (not this code) left
a device deaf with a hissing amplifier until power was cycled. `pryon_wake`
closes fds 3 to 1023 as its first statement; `pryon_fd_test.go` pins both the
inheritance and the cleanup. Gaps are in section 7. The proper fix,
close-on-exec in the mic binding, is outside this change.

## 4. Scope of the diff

`git diff --stat v2.17.0..pryon-backend`: 19 files, +4129 / -30, 20 commits.
Largest, all under `device/`: `internal/wakeword/shadow/pryon.go` +1275,
`pryon_test.go` +1141, `tools/pryon_wake/pryon_wake.c` +413, `pryon_open_test.go`
+290, `pryon_open.go` +239. The other 14 files are under 150 lines each,
among them `engine.go` +97, `open.go` +53 -10, `shadow.go` +32 -20 and the
helper's README +141. About 2200 lines are code (Go, C, header, build
script), 1770 are tests.

Commits, oldest first. Reviewed in round 1 (the first 9): `63464dc`,
`cafe575` (helper, README); `8e20427` (engine interface); `b1a719b`,
`24120c7`, `2bce97d` (Pryon engine, selection, test with the real helper);
`7f051b0`, `01ec181`, `4a15cf6` (stale held wakes, orphan check, README).
Made in response to round 1 (the last 11): `7a4975a`, `3b20fde`, `85882c2`,
`62ab552`, `14b6a07`, `c0d3321`, `d3ca6b9`, `c1f0493` (fixes), `621432f`,
`e7d7782` (known gaps documented), `023e3be` (comment cleanup).

## 5. Testing done

**Tests (host, macOS).** 43 new tests: `pryon_test.go` 32 (plus a
`TestMain`), `pryon_open_test.go` 7, `pryon_fd_test.go` 2,
`pryon_helper_test.go` 2. `go test -count=1 -v ./internal/wakeword/shadow/` on the final head: 64
top-level tests pass (new and existing), none fail, none skipped. Most Pryon
tests use a fake helper process; three (`RealHelper`) build the real
`pryon_wake` against a stub library (`test/stub_pryon.c`) with the host `cc`
and are skipped without a compiler. The Python protocol test passes (19
checks): audio, properties, gaps, unknown frame types, frames split across
tiny writes, stdout closing. **No test uses the real `libpryon.so`.**

**Race detector.** `-race -count=5 ./internal/wakeword/...` and `-race
-count=3 -cpu=1,4` passed on the final head (author's runs), `-race -count=3`
again when this text was written. The reviewer ran `-race -count=10 -cpu=1,4`
under CPU load on the 9-commit state without flaky results. `gofmt` and
`go vet` are clean on the host; a full `go vet` for linux/arm was not
possible without the device toolchain. **On the device:** 6.3 and 6.4, by
hand, short sessions.

**Mutation tests** (reviewer, on a copy). Eight mutations; six made tests
fail (`at` formula +1 and -1 frame, offset 3 to 2, hold 2 to 1, reset check
removed, age check removed). Two survived: a redundant `drain(discard)` in
`Reset` (duplicate of `resetFed`, not a test gap) and `s.ready = false` in
`Scorer.run` (a real gap, closed in `14b6a07`).

**Reviews.** A second, independent AI model instance (not a human), uninvolved in the
implementation, reviewed the plan before any code and the code at 9 commits (round 1). The
plan review changed the timestamp design (no interpolation, the E-3 rule),
required dropping old detections on reset, replaced a drop-count acceptance
criterion and flagged the `libpryon` network imports. The code review found
two blockers and smaller points:

| Finding | Change |
|---|---|
| B1: helper that stops reading stdin never restarted; device deaf, reports `local` | watchdog, `7a4975a` |
| B2: failing openWakeWord fallback leaves device deaf, reports `local` | Pryon only where openWakeWord can take over, `3b20fde` |
| C2: missing `READY` at first start (5 s) may be a slow cold boot | 20 s for the first helper, `62ab552` |
| C3: wrong comment on inherited descriptors | corrected, pinned in tests, `85882c2` |
| C4/C5: oversized stdout line; foreign SIGKILL not counted | both counted as crashes, `c1f0493`, `c0d3321` |
| NaN score never crosses; `ready = false` untested | `d3ca6b9`; `14b6a07` |
| C6 no heartbeat; C7 arbitration head start | **documented only**, `621432f`, `e7d7782` |

**The second review round, over the 11 commits made in response to round 1,
was not carried out.** Those fixes have their own tests and the runs above, but
nobody other than the author has read them.

## 6. Results

### 6.1 Replay of a real recording on the device

One 110 s recording of the raw 9-channel microphone stream: 9 times "Alexa,
wie spät ist es?" (German command after the English wake word), different
distances (not recorded per utterance), all in the first 30 s because the
speaker was faster than planned. +24 dB gain and conversion to S16 as in the
EchoMuse pipeline. Played back offline, not live.

| Engine, channel | Threshold | Found of 9 |
|---|---|---|
| Pryon, each of the 7 microphone channels `ch0`..`ch6` | own (0.923) | 9, no extra detections |
| openWakeWord `alexa_v0.1`, centre `ch6` (EchoMuse idle) | 0.5 | 4 |
| openWakeWord, best single ring microphones (`ch0`, `ch1`, `ch5`) | 0.5 | 6 |
| openWakeWord, `ch2`, `ch3` | 0.5 | 5 |
| openWakeWord, worst channel (`ch4`) | 0.5 | 4 |
| openWakeWord, centre | 0.3 | 5 |
| openWakeWord, best channel (`ch1`) | 0.3 | 8 |

openWakeWord missed one utterance on every channel. One recording, one
speaker, one room; not a controlled A/B test; says nothing about false
alarms.

### 6.2 Five phone recordings

Five recordings of "what time is it" at different distances. openWakeWord on
a Mac detected 4 of 5 (scores of at least 0.997); the weakest scored 0.465,
below 0.5. Pryon needed about 3 s of lead-in noise (5 of 5 with, 1 of 5
without), hence the warm-up. Here openWakeWord was nearly as good as Pryon;
the difference showed on the real microphone path.

### 6.3 `pryon_wake` on the device

File mode and real-time stdin mode: 9 of 9 on the recording of 6.1, with
identical word ends and scores. CPU during playback about 0.31 core (10.1 s
CPU over 32.8 s). The dashboard shows about 0.4 core for on-device detection
with openWakeWord; **I did not measure that number**.

### 6.4 Live, first minutes after installation

Mode "On this Echo", model `pryon_alexa`. The 30 s log line showed
`restarts=0 drops=0 stale=0`; latency from word end to wake message 68 ms
median, 210 ms maximum; `ready after 535ms`; scores of accepted wakes 0.90
to 0.98. The logged helper CPU was 34 to 35%. In `pryon.go` that figure is the
helper's user plus system time over the 30 s window as a percentage of **one
core**, all threads together, so it is about 0.35 core, against 0.31 core in
the replay of 6.3 (which included model load); both are single short
measurements. The user reports it works "much better than before". That
is an impression, not a measurement, and the reference setup was not
recorded precisely.

### 6.5 Not measured

False alarm rate (skipped on purpose) for Pryon **and** openWakeWord; more
than one speaker; other rooms; background noise such as a television;
long-term stability; cold start after boot; mute/unmute; barge-in with
Pryon; a mixed fleet, arbitration, several Echoes; an Echo Dot 3rd gen.

## 7. Known limitations, open, uncertain

- **False alarms unknown.** The main gap. An engine that hears better may
  also hear more. `shadow` mode would allow a cheap comparison over hours; I
  have not run it.
- **The sensitivity slider no longer controls detection.** Pryon decides with
  its own threshold (0.923, 0.864 while audio plays) and its score also has
  to clear the dashboard threshold, so the slider can only make Pryon
  stricter; at the default 0.5 it has no effect. The two-frame barge-in rule
  is ineffective too (every accepted wake is held two frames); barge-in
  protection is Pryon's own playback threshold. How often TTS echo triggers
  it is unmeasured.
- **No heartbeat (C6).** A helper that keeps reading while the decoder has
  stopped producing results is not detected: no crash, no restart, no wakes.
- **Arbitration head start with several Echoes (C7).** The reported time is
  also read by the controller as "heard at". It is ~240 ms before the word
  end, while an openWakeWord Echo reports its crossing at or after it. A
  Pryon Echo thus claims to hear 320 to 560 ms earlier, wins the 300 ms
  window even from far away, and the two may count as separate utterances.
  Needs separate "session start" and "heard at" times: a `server.go` change.
  Irrelevant with one Echo.
- **No delayed second Pryon attempt after fallback** (after a slow boot that
  is a silent downgrade, only logged). **ORT present but not loadable**
  reports `local`, not `degraded`: `checkOWW` only checks that files exist.
- **Microphone fd without CLOEXEC, protected only by the helper.** Gaps:
  fds numbered 1024 or higher, the instant between `exec` and the helper's
  first statement, and a wrapper script as `EM_PRYON_HELPER`. `close_range`
  in C was suggested; not done.
- **Linux path barely tested.** `pryon_sys_linux.go` (process groups,
  parent-death signal) ran on the device in a short test only; local tests
  ran on macOS. The `device-tests` job in `ci.yml` runs `go test -race` on
  `ubuntu-latest` and should cover it, but this branch has never been
  pushed, so that has not happened.
- **Privacy statement open.** `libpryon.so` imports `socket` and `connect`.
  Whether the library itself sends anything over the network is **unchecked**.
  Until it is, "On this Echo" must not be described as private listening
  with Pryon. The README says how to check a running helper.
- **Reverse-engineered API.** `pryon_api.h` describes structures inferred for
  32-bit ARM and one library version, guarded by plausibility checks
  (`E ABI`, exit code 6). Other library versions are untested. Android base
  API 25 tested; Fire OS 5 based devices untested.
- **Latency budget.** A detection must arrive within ~0.9 s of the frame
  chosen as `at`. Live: 68 ms median, 210 ms max over a few minutes; nothing
  known for long runs or a loaded device.
- **Size and rollout.** ~2200 lines of code for one keyword on one device
  model (section 10). The helper is not part of the Local Build; it is
  copied by hand.

**Separate observation, not part of this change.** Stored conversation
recordings after wake-up on this device are quiet: largest peak -21 to -24
dBFS, noise floor -48 to -55 dBFS (16 to 22 dB apart). AGC applies only to
push-to-talk turns, and `micGainDb` is at its default of 24 dB. This is
unrelated to the wake word engine and not changed here; it would be its own
issue. I have not measured whether it affects transcription.

## 8. Legal, licensing, privacy

- `libpryon.so` and its models belong to Amazon. **Nothing from Amazon is in
  this repository**: no library, no model, no binary. The helper `dlopen`s the
  copy in the device's `/system`.
- `pryon_api.h` is a subset of C declarations from the echo-dot-assist
  project (MIT), listed in `NOTICE.md`. `CONTRIBUTING.md` also asks for the
  licence text beside the code; that file is **not** added yet.
- Whether calling an undocumented vendor library through its exports is
  acceptable for this project is for the maintainers to decide. This is not
  legal advice.
- Privacy: see section 7; no privacy claim for the Pryon path until the
  library's network behaviour is checked.

## 9. How to try it

Needs a rooted Echo Dot 2nd gen, the dashboard terminal and the toolchain
from `CONTRIBUTING.md`.

1. Build the server (`cd device && ./compile.sh`) and deploy it with the
   dashboard's Updates > Local Build. A second Local Build overwrites the slot
   that held the release, so the rollback target changes.
2. Build the helper in the `echomuse-compiler` image
   (`device/tools/pryon_wake/build.sh`) and copy it to
   `/data/local/share/echomuse/pryon/pryon_wake`, mode 755 (commands in the
   helper's README). Check it alone: `./pryon_wake -f some.wav -x 1`.
3. Upload a copy of `alexa_v0.1.onnx` as custom model `pryon_alexa.onnx`.
4. In the **same** save, select it and set detection to "On this Echo" (or
   `shadow` to compare without acting). In "On the controller" mode nothing
   changes.

Signs that Pryon runs: the `[shadow] on-device wake word scoring (...)` line
names the engine; `[pryon] helper pid N ready after Nms`; `[pryon] wake
score=...` per detection; a `[pryon]` statistics line every 30 s (fields in
the helper's README). After a fallback the log shows `giving up on Pryon`.

Going back: select another wake word model; or create
`/data/local/etc/echomuse/no_pryon` (or `EM_PRYON=0`) and change the model or
restart; or roll back to the previous server slot. The `pryon_alexa.onnx`
copy keeps working as an openWakeWord "Alexa" model.

## 10. Suggested changes before an upstream merge

1. **Split into three PRs:** (a) the `engine` refactor without behaviour
   change, which stands alone; (b) `pryon_wake` with tests and README; (c)
   the Pryon engine and selection.
2. **Trim `pryon.go`.** The reviewer estimated 25 to 30% could go without
   safety loss (on the version before the fixes, 1191 lines): CPU reading and
   latency percentiles in the statistics line (~60 lines, keep the
   counters); one of `drain(discard)` and `resetFed`; merge the two frame
   encoders and the two log rate limiters; one restart policy type; drop or
   document `killOrphanHelpers` (it kills a helper started by hand with
   `setsid nohup`).
3. **Model metadata instead of a name convention:** the controller already
   reads ONNX metadata (`wake_word`, `language`); keys such as `engine` and
   `pryon_manifest` would remove the prefix rule and fit "negotiate by
   capability" better.
4. **Runtime detection rather than a build tag** (my preference: one
   artifact, active only where the library and model exist), possibly with a
   device capability such as `oww_engine_pryon` announced to the controller;
   that part is a controller change.
5. **CI under Linux** for the real-helper and fd tests; verify what
   `device-tests` already covers.
6. **Dashboard hint:** which engine runs, and that the slider only tightens
   Pryon.
7. Close the gaps from section 7 that need `server.go` or the mic binding:
   separate "session start" and "heard at" times (C7), `FD_CLOEXEC` on the PCM
   descriptor, a protocol heartbeat (C6), the licence text.
8. Run the second review round and the false alarm measurement first. Most
   useful feedback meanwhile: a few hours in `shadow` mode with a television
   or radio running, other rooms or speakers, an Echo Dot 3rd gen.

I did not touch `CHANGELOG.md`, `CLAUDE.md` or version pins, per
`CONTRIBUTING.md`. Nothing in this branch changes the controller.

## 11. Open questions for the maintainers

1. Is an engine that uses a proprietary library already on the device,
   loaded at run time and not redistributed, acceptable in this project? If
   not, this stays a fork.
2. If yes: is the engine interface refactor (3.2) welcome on its own?
3. Is selection by `pryon_` name prefix acceptable as a first step, or should
   it wait for model metadata or a device capability?
4. Should the arbitration time and the session-start time be separated in
   `onCross` (C7)? That touches the device-controller interface.
5. How should false alarms be measured so results from different people are
   comparable? Does the project have a corpus or procedure for openWakeWord?
6. Does anyone know whether `libpryon.so` opens network connections itself,
   and which library versions circulate on Fire OS 5 and 6 devices?
7. Should this start as an issue labelled `needs-design`? This text is meant
   as its content as well.
