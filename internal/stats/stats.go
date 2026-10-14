// Package stats collects per request outcomes and summarises them.
package stats

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Outcome is the result of one HTTP request. Status is 0 when the request
// never produced a response, for example on a timeout or a dial failure.
type Outcome struct {
	Status  int
	Latency time.Duration
	Failed  bool
}

// Recorder accumulates outcomes. It is safe for concurrent use.
type Recorder struct {
	mu        sync.Mutex
	latencies []time.Duration
	byStatus  map[int]int
	transport int
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder {
	return &Recorder{byStatus: make(map[int]int)}
}

// Add records one outcome.
func (r *Recorder) Add(o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.latencies = append(r.latencies, o.Latency)
	if o.Status == 0 {
		r.transport++
		return
	}
	r.byStatus[o.Status]++
}

// Summary is the aggregate view of everything recorded.
type Summary struct {
	Total           int
	ByStatus        map[int]int
	TransportErrors int
	Elapsed         time.Duration
	Min             time.Duration
	P50             time.Duration
	P95             time.Duration
	P99             time.Duration
	Max             time.Duration
}

// Successes counts 2xx responses.
func (s Summary) Successes() int {
	n := 0
	for code, count := range s.ByStatus {
		if code >= 200 && code < 300 {
			n += count
		}
	}
	return n
}

// Throughput returns requests per second over the elapsed wall time.
func (s Summary) Throughput() float64 {
	if s.Elapsed <= 0 {
		return 0
	}
	return float64(s.Total) / s.Elapsed.Seconds()
}

// Summarise sorts the recorded latencies and computes the percentiles.
func (r *Recorder) Summarise(elapsed time.Duration) Summary {
	r.mu.Lock()
	defer r.mu.Unlock()

	sorted := make([]time.Duration, len(r.latencies))
	copy(sorted, r.latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	byStatus := make(map[int]int, len(r.byStatus))
	for code, count := range r.byStatus {
		byStatus[code] = count
	}

	s := Summary{
		Total:           len(sorted),
		ByStatus:        byStatus,
		TransportErrors: r.transport,
		Elapsed:         elapsed,
		P50:             Percentile(sorted, 50),
		P95:             Percentile(sorted, 95),
		P99:             Percentile(sorted, 99),
	}
	if len(sorted) > 0 {
		s.Min = sorted[0]
		s.Max = sorted[len(sorted)-1]
	}
	return s
}

// Percentile returns the value at the given percentile of an already sorted
// slice, using the nearest rank method: the value at index
// ceil(p/100 * n) - 1. An empty slice yields zero.
func Percentile(sorted []time.Duration, p float64) time.Duration {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := int(math.Ceil(p / 100 * float64(n)))
	if rank < 1 {
		rank = 1
	}
	if rank > n {
		rank = n
	}
	return sorted[rank-1]
}
