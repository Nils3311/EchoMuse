package shadow

import (
	"io"
	"time"

	"github.com/wilbowes/EchoMuse/internal/wakeword"
)

// engine is what Scorer.run drives: the part of wake word detection that
// differs between backends. Everything around it — the never-blocking queue,
// drop accounting, thresholds, the barge-in two-frame rule, the refractory
// period, the bench trace — belongs to the Scorer and is shared, because that
// is the part that took a year of hardware incidents to get right and must not
// exist twice.
//
// Every method except Info is called ONLY from the scorer goroutine (Close only
// after it has exited), which is the contract *wakeword.Detector already had:
// an engine needs no locking of its own for anything it touches there. Info is
// the exception, because the server reads it for a log line from its own
// goroutine.
type engine interface {
	// Push hands over one frame (normally 80ms) and the time it was captured.
	// It must never block for long: the scorer goroutine is the only thing
	// draining the queue the mic goroutine feeds, and a stall here becomes
	// drops there.
	Push(pcm []int16, at time.Time) error
	// Ready reports whether the engine has enough context to score.
	Ready() bool
	// Score returns the current score AND the capture time it refers to. For
	// openWakeWord that is simply the frame just pushed. An engine that
	// reports detections asynchronously (Pryon) knows better than the queue
	// which audio a detection belongs to, so the time is the engine's to say.
	Score() (score float32, at time.Time, err error)
	// Reset marks a gap in the stream (StartMic): audio from before it must
	// not contribute to a score after it.
	Reset()
	// SetPlayback tells the engine whether the speaker is playing and the
	// barge-in bar is in force. openWakeWord ignores it — the Scorer already
	// lowers its bar — but an engine with its own internal threshold can be
	// told to lower that too.
	SetPlayback(on bool)
	// Info describes the engine for a log line. Safe from any goroutine.
	Info() string
	// Close releases the engine's resources. Called once, after the scorer
	// goroutine has exited.
	Close() error
}

// owwEngine is the openWakeWord engine: a *wakeword.Detector, unchanged. It
// exists so the Scorer can hold an engine, and adds nothing to the detector's
// behaviour — the existing scorer tests run through it untouched.
type owwEngine struct {
	det *wakeword.Detector
	// lastAt is the capture time of the frame last pushed, which is the frame
	// the next Score is about.
	lastAt time.Time
	// closer releases the inference engine, when one owns resources (the ORT
	// sessions). Nil for an injected Inferer, e.g. in tests.
	closer io.Closer
	// info describes the loaded engine for logging: runtime version, model,
	// whether XNNPACK attached. Set once at construction, never mutated, which
	// is what makes Info safe from any goroutine.
	info string
}

func newOWWEngine(inf wakeword.Inferer) *owwEngine {
	return &owwEngine{det: wakeword.New(inf)}
}

func (o *owwEngine) Push(pcm []int16, at time.Time) error {
	o.lastAt = at
	_, err := o.det.Push(pcm)
	return err
}

func (o *owwEngine) Ready() bool { return o.det.Ready() }

func (o *owwEngine) Score() (float32, time.Time, error) {
	score, err := o.det.Score()
	return score, o.lastAt, err
}

func (o *owwEngine) Reset() { o.det.Reset() }

// SetPlayback is a no-op: the Scorer applies the barge-in bar itself
// (effectiveThresholdLocked), and openWakeWord has no threshold of its own.
func (o *owwEngine) SetPlayback(bool) {}

func (o *owwEngine) Info() string { return o.info }

func (o *owwEngine) Close() error {
	if o.closer != nil {
		return o.closer.Close()
	}
	return nil
}
