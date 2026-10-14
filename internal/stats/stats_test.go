package stats

import (
	"sync"
	"testing"
	"time"
)

func durations(ms ...int) []time.Duration {
	out := make([]time.Duration, len(ms))
	for i, v := range ms {
		out[i] = time.Duration(v) * time.Millisecond
	}
	return out
}

func TestPercentile(t *testing.T) {
	hundred := make([]time.Duration, 100)
	for i := range hundred {
		hundred[i] = time.Duration(i+1) * time.Millisecond
	}

	tests := []struct {
		name   string
		sorted []time.Duration
		p      float64
		want   time.Duration
	}{
		{"empty slice is zero", nil, 50, 0},
		{"single value p50", durations(7), 50, 7 * time.Millisecond},
		{"single value p99", durations(7), 99, 7 * time.Millisecond},
		{"two values p50 takes the first", durations(1, 2), 50, 1 * time.Millisecond},
		{"two values p95 takes the last", durations(1, 2), 95, 2 * time.Millisecond},
		{"p0 clamps to the minimum", durations(3, 5, 9), 0, 3 * time.Millisecond},
		{"p100 clamps to the maximum", durations(3, 5, 9), 100, 9 * time.Millisecond},
		{"beyond p100 clamps to the maximum", durations(3, 5, 9), 150, 9 * time.Millisecond},
		{"negative clamps to the minimum", durations(3, 5, 9), -10, 3 * time.Millisecond},
		{"hundred values p50 is the fiftieth", hundred, 50, 50 * time.Millisecond},
		{"hundred values p95 is the ninety fifth", hundred, 95, 95 * time.Millisecond},
		{"hundred values p99 is the ninety ninth", hundred, 99, 99 * time.Millisecond},
		{"three values p50 is the middle", durations(10, 20, 30), 50, 20 * time.Millisecond},
		{"four values p95 is the last", durations(1, 2, 3, 4), 95, 4 * time.Millisecond},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Percentile(tc.sorted, tc.p); got != tc.want {
				t.Fatalf("Percentile(p%v) = %v, want %v", tc.p, got, tc.want)
			}
		})
	}
}

func TestPercentileIsMonotonic(t *testing.T) {
	sorted := durations(1, 2, 3, 4, 5, 6, 7, 8, 9, 10)
	previous := time.Duration(0)
	for p := 1; p <= 100; p++ {
		got := Percentile(sorted, float64(p))
		if got < previous {
			t.Fatalf("p%d = %v went backwards from %v", p, got, previous)
		}
		previous = got
	}
}

func TestSummariseCountsAndPercentiles(t *testing.T) {
	r := NewRecorder()
	for i := 1; i <= 100; i++ {
		status := 201
		if i > 90 {
			status = 422
		}
		r.Add(Outcome{Status: status, Latency: time.Duration(i) * time.Millisecond})
	}
	r.Add(Outcome{Status: 0, Latency: 5 * time.Millisecond})

	s := r.Summarise(2 * time.Second)

	if s.Total != 101 {
		t.Fatalf("Total = %d, want 101", s.Total)
	}
	if s.ByStatus[201] != 90 {
		t.Fatalf("ByStatus[201] = %d, want 90", s.ByStatus[201])
	}
	if s.ByStatus[422] != 10 {
		t.Fatalf("ByStatus[422] = %d, want 10", s.ByStatus[422])
	}
	if s.Successes() != 90 {
		t.Fatalf("Successes() = %d, want 90", s.Successes())
	}
	if s.TransportErrors != 1 {
		t.Fatalf("TransportErrors = %d, want 1", s.TransportErrors)
	}
	if got, want := s.Throughput(), 50.5; got != want {
		t.Fatalf("Throughput() = %v, want %v", got, want)
	}
	if s.Min != 1*time.Millisecond {
		t.Fatalf("Min = %v, want 1ms", s.Min)
	}
	if s.Max != 100*time.Millisecond {
		t.Fatalf("Max = %v, want 100ms", s.Max)
	}
}

func TestSummariseEmpty(t *testing.T) {
	s := NewRecorder().Summarise(time.Second)
	if s.Total != 0 || s.P50 != 0 || s.Successes() != 0 || s.Throughput() != 0 {
		t.Fatalf("empty summary is not zeroed: %+v", s)
	}
}

func TestThroughputWithoutElapsedTime(t *testing.T) {
	r := NewRecorder()
	r.Add(Outcome{Status: 200, Latency: time.Millisecond})
	if got := r.Summarise(0).Throughput(); got != 0 {
		t.Fatalf("Throughput() with no elapsed time = %v, want 0", got)
	}
}

func TestRecorderIsSafeUnderConcurrency(t *testing.T) {
	r := NewRecorder()
	const writers, each = 8, 250

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range each {
				r.Add(Outcome{Status: 200 + w, Latency: time.Duration(i) * time.Microsecond})
			}
		}(w)
	}
	wg.Wait()

	s := r.Summarise(time.Second)
	if s.Total != writers*each {
		t.Fatalf("Total = %d, want %d", s.Total, writers*each)
	}
	for w := range writers {
		if s.ByStatus[200+w] != each {
			t.Fatalf("ByStatus[%d] = %d, want %d", 200+w, s.ByStatus[200+w], each)
		}
	}
}
