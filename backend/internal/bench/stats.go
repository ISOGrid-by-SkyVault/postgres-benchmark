package bench

import (
	"math"
	"slices"
	"time"
)

// Measurement is the raw outcome of one action.
type Measurement struct {
	// Timed is true when the action ran many operations and the latency
	// distribution is meaningful, false for one-shot actions.
	Timed      bool
	Operations int64
	Errors     int64
	Rows       int64
	Wall       time.Duration
	// Latencies of the successful operations of a timed action.
	Latencies []time.Duration
	// ErrorSample is the first error seen, if any.
	ErrorSample string
}

// Summary is a Measurement reduced to the numbers that are stored.
type Summary struct {
	DurationMs   float64
	OpsPerSecond float64
	AvgMs        float64
	P50Ms        float64
	P95Ms        float64
	P99Ms        float64
	MinMs        float64
	MaxMs        float64
}

// Summarize computes throughput and latency percentiles.
func Summarize(m Measurement) Summary {
	s := Summary{DurationMs: ms(m.Wall)}
	if m.Wall > 0 {
		s.OpsPerSecond = float64(m.Operations) / m.Wall.Seconds()
	}
	if len(m.Latencies) == 0 {
		return s
	}

	sorted := slices.Clone(m.Latencies)
	slices.Sort(sorted)

	var total time.Duration
	for _, d := range sorted {
		total += d
	}
	s.AvgMs = ms(total) / float64(len(sorted))
	s.MinMs = ms(sorted[0])
	s.MaxMs = ms(sorted[len(sorted)-1])
	s.P50Ms = ms(percentile(sorted, 50))
	s.P95Ms = ms(percentile(sorted, 95))
	s.P99Ms = ms(percentile(sorted, 99))
	return s
}

// percentile uses the nearest-rank method on an ascending slice.
func percentile(sorted []time.Duration, p float64) time.Duration {
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	return sorted[min(max(rank, 1), len(sorted))-1]
}

func ms(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
