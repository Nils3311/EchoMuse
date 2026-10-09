package shadow

import (
	"testing"
	"time"
)

func TestWakeReportTime(t *testing.T) {
	at := time.Unix(1000, 0)
	now := at.Add(490 * time.Millisecond)
	pryon := "pryon, model pryon_alexa, manifest /m, warm-up 3000ms, reset R0, at-offset 3 frames"
	cases := []struct {
		name, info, env string
		at, now, want   time.Time
	}{
		{"pryon reports decision time", pryon, "", at, now, now},
		{"pryon with note still pryon", pryon + ", helper installed from server build", "", at, now, now},
		{"word time on request", pryon, "word", at, now, at},
		{"openWakeWord keeps word time", "onnxruntime 1.19.2, model pryon_alexa (pryon unavailable: helper not installed at /x)", "", at, now, at},
		{"pryon failover keeps word time", "pryon failed (crash), loading openWakeWord", "", at, now, at},
		{"clock never goes back", pryon, "", now, at, now},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("EM_PRYON_REPORT_AT", c.env)
			if got := WakeReportTime(c.info, c.at, c.now); !got.Equal(c.want) {
				t.Errorf("WakeReportTime = %v, want %v", got, c.want)
			}
		})
	}
}
