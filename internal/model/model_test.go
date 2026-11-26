package model

import "testing"

func TestQuoteNamePreservesJoinedEmojiAndEscapesLiteralText(t *testing.T) {
	name := "👩‍💻 " + `\u200d` + "\n\x1b\""
	want := `"👩‍💻 \\u200d\n\x1b\""`
	if got := QuoteName(name); got != want {
		t.Fatalf("QuoteName = %q, want %q", got, want)
	}
}

func TestProbeLatencyRecognizesOnlyCompleted204Measurements(t *testing.T) {
	for _, test := range []struct {
		name   string
		sample Sample
		want   int
		ok     bool
	}{
		{name: "positive bench", sample: Sample{Kind: "bench", LatencyMS: 23, Attempts: 1}, want: 23, ok: true},
		{name: "download failed after probe", sample: Sample{Kind: "sweep", LatencyMS: 31, Attempts: 2, Failures: 1, Error: "download failed"}, want: 31, ok: true},
		{name: "real zero", sample: Sample{Kind: "confirm", LatencyMS: 0, Attempts: 2}, want: 0, ok: true},
		{name: "zero value", sample: Sample{Kind: "quick", LatencyMS: 0, Attempts: 1}},
		{name: "failed 204", sample: Sample{Kind: "bench", LatencyMS: 0, Attempts: 1, Failures: 1, Error: "204 probe: HTTP 504"}},
		{name: "failed first attempt with a stale delay", sample: Sample{Kind: "sweep", LatencyMS: 27, Attempts: 1, Failures: 1}},
		{name: "negative", sample: Sample{Kind: "sweep", LatencyMS: -1, Attempts: 2}},
		{name: "heartbeat", sample: Sample{Kind: "heartbeat", LatencyMS: 10, Attempts: 1}},
		{name: "geo", sample: Sample{Kind: "geo", LatencyMS: 10, Attempts: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := test.sample.ProbeLatency()
			if got != test.want || ok != test.ok {
				t.Fatalf("ProbeLatency = %d, %v; want %d, %v", got, ok, test.want, test.ok)
			}
		})
	}
}
