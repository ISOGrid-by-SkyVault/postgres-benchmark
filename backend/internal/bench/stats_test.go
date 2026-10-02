package bench

import (
	"testing"
	"time"
)

func TestSummarizePercentiles(t *testing.T) {
	// 1ms .. 100ms, shuffled order must not matter
	latencies := make([]time.Duration, 0, 100)
	for i := 100; i >= 1; i-- {
		latencies = append(latencies, time.Duration(i)*time.Millisecond)
	}

	s := Summarize(Measurement{Timed: true, Operations: 100, Wall: 2 * time.Second, Latencies: latencies})

	checks := []struct {
		name      string
		got, want float64
	}{
		{"OpsPerSecond", s.OpsPerSecond, 50},
		{"DurationMs", s.DurationMs, 2000},
		{"MinMs", s.MinMs, 1},
		{"MaxMs", s.MaxMs, 100},
		{"AvgMs", s.AvgMs, 50.5},
		{"P50Ms", s.P50Ms, 50},
		{"P95Ms", s.P95Ms, 95},
		{"P99Ms", s.P99Ms, 99},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
}

func TestSummarizeSingleSample(t *testing.T) {
	s := Summarize(Measurement{Timed: true, Operations: 1, Wall: time.Second, Latencies: []time.Duration{7 * time.Millisecond}})
	if s.P50Ms != 7 || s.P99Ms != 7 || s.MinMs != 7 || s.MaxMs != 7 {
		t.Errorf("single sample should give 7ms everywhere, got %+v", s)
	}
}

func TestSummarizeWithoutLatencies(t *testing.T) {
	s := Summarize(Measurement{Operations: 10_000, Wall: 500 * time.Millisecond})
	if s.OpsPerSecond != 20_000 {
		t.Errorf("OpsPerSecond = %v, want 20000", s.OpsPerSecond)
	}
	if s.P50Ms != 0 || s.MaxMs != 0 {
		t.Errorf("latency fields should stay zero, got %+v", s)
	}
}
