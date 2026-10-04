package shadow

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wilbowes/EchoMuse/internal/wakeword/ort"
)

// DefaultDir is where the ONNX Runtime library and the models live on the
// device. Overridable with EM_OWW_DIR, mainly so the same binary can be
// pointed at a scratch directory while iterating.
//
// The library is NOT part of the firmware: it is 12.3MB, it changes far less
// often than the binary, and embedding it would double both the OTA payload and
// the space taken by the A/B slots. It is installed out of band, and its
// absence is an ordinary, expected condition — see Open.
const DefaultDir = "/data/local/share/echomuse/oww"

// Dir returns the configured model directory.
func Dir() string {
	if d := os.Getenv("EM_OWW_DIR"); d != "" {
		return d
	}
	return DefaultDir
}

// ModelStem turns the controller's owwModel value into the classifier filename
// stem, mirroring em_oww_models.prediction_key EXACTLY.
//
// That value is a bare name for built-in models ("hey_mycroft_v0.1") and a file
// PATH for custom ones. Python's rule is: strip to the filename stem only when
// the value ends in ".onnx", otherwise pass it through untouched.
//
// The untouched case is the whole point, and getting it wrong is not
// hypothetical — an earlier version used filepath.Ext, which sees ".1" as the
// extension of "hey_mycroft_v0.1" and went looking for hey_mycroft_v0.onnx. A
// version suffix is not a file extension.
func ModelStem(owwModel string) string {
	name := strings.TrimSpace(owwModel)
	if !strings.HasSuffix(name, ".onnx") {
		return name
	}
	return strings.TrimSuffix(filepath.Base(name), ".onnx")
}

// Open loads ONNX Runtime and the three models for owwModel, and starts a
// Scorer. A model whose stem starts with PryonPrefix runs on Pryon instead,
// falling back to openWakeWord on the same model file whenever Pryon cannot
// run (see openWithPryon).
//
// A missing library or model returns an error and is NOT a failure of the
// device: shadow mode is off until someone installs them, the caller logs it
// once and carries on with controller-side wake word. That is the whole reason
// the runtime is dlopen'd rather than linked.
func Open(owwModel string, threshold float32, onCross func(score, threshold float32, at time.Time)) (*Scorer, error) {
	dir := Dir()
	stem := ModelStem(owwModel)
	if stem == "" {
		return nil, fmt.Errorf("shadow: no wake word model configured")
	}
	if strings.HasPrefix(stem, PryonPrefix) {
		return openWithPryon(dir, stem, threshold, onCross)
	}
	eng, err := openOWW(dir, stem)
	if err != nil {
		return nil, err
	}
	return newScorer(eng, threshold, onCross), nil
}

// openOWW is the openWakeWord loader. A variable only so tests can stand in
// for ONNX Runtime, which a host running the tests does not have.
var openOWW = openOWWEngine

func owwModels(dir, stem string) ort.Models {
	return ort.Models{
		Melspec:    filepath.Join(dir, "melspectrogram.onnx"),
		Embedding:  filepath.Join(dir, "embedding_model.onnx"),
		Classifier: filepath.Join(dir, stem+".onnx"),
	}
}

// checkOWWModels is checked up front so the log names the missing file. ORT's
// own error for a bad path mentions neither which of the three it was nor what
// was expected there, and this is a path people will get wrong while
// installing by hand.
func checkOWWModels(models ort.Models) error {
	for what, p := range map[string]string{
		"melspectrogram model": models.Melspec,
		"embedding model":      models.Embedding,
		"classifier model":     models.Classifier,
	} {
		if _, err := os.Stat(p); err != nil {
			return fmt.Errorf("shadow: %s not installed at %s", what, p)
		}
	}
	return nil
}

// checkOWW is the cheap part of openOWWEngine: are the runtime and the three
// models there. Pryon is only started where this holds, so that openWakeWord
// can actually take over when Pryon fails at run time (see openWithPryon).
func checkOWW(dir, stem string) error {
	if err := checkOWWModels(owwModels(dir, stem)); err != nil {
		return err
	}
	lib := filepath.Join(dir, "libonnxruntime.so")
	if _, err := os.Stat(lib); err != nil {
		return fmt.Errorf("shadow: onnxruntime not installed at %s", lib)
	}
	return nil
}

// openOWWEngine loads ONNX Runtime and the three models for stem.
func openOWWEngine(dir, stem string) (*owwEngine, error) {
	models := owwModels(dir, stem)
	if err := checkOWWModels(models); err != nil {
		return nil, err
	}

	lib := filepath.Join(dir, "libonnxruntime.so")
	rt, err := ort.Open(lib)
	if err != nil {
		return nil, fmt.Errorf("shadow: %w", err)
	}
	inf, err := rt.NewInferer(models, ort.DefaultOptions())
	if err != nil {
		return nil, fmt.Errorf("shadow: %w", err)
	}

	eng := newOWWEngine(inf)
	eng.closer = inf
	eng.info = fmt.Sprintf("onnxruntime %s, model %s, xnnpack=%v",
		rt.Version(), stem, inf.XNNPACKActive())
	return eng, nil
}
