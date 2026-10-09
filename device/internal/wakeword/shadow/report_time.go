package shadow

import (
	"os"
	"strings"
	"time"
)

// WakeReportTime is the capture instant to put in the oww_wake message:
// when the Echo heard the wake word, as far as the controller's arbitration is
// concerned.
//
// For openWakeWord that is the frame that crossed (`at`), reported some 60ms
// later. Pryon decides only after the word has ended and a little more audio
// has passed, so `at` is about 490ms old when the message leaves (measured on
// two Echoes, 2026-10-09). The controller holds a contest for 250ms counted
// from the first claim's capture instant (em_arbiter.contest, #790); with
// Pryon's `at` that hold has already run out when the claim arrives, the
// contest is decided with a single claim, and the second Echo cedes without
// its level being compared — the turn goes to whichever Echo's message arrived
// first, which between two Pryon Echoes is set by their 160ms block grids,
// not by distance.
//
// So a Pryon wake reports the moment Pryon made its decision: the hold then
// starts when the first claim actually arrives, the other Echo's claim (1-144ms
// later in the same measurements) joins the contest, and the louder Echo
// answers. Only the reported instant moves: the listen session still opens at
// `at` and the wake level is still measured up to `at`, so no audio is lost
// and the level is unchanged. EM_PRYON_REPORT_AT=word restores the word time.
//
// engineInfo is the scorer's Info(); Pryon's starts with "pryon, ".
func WakeReportTime(engineInfo string, at, now time.Time) time.Time {
	if !strings.HasPrefix(engineInfo, "pryon, ") || os.Getenv("EM_PRYON_REPORT_AT") == "word" {
		return at
	}
	if now.Before(at) {
		return at
	}
	return now
}
