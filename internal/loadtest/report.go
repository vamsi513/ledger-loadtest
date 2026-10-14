package loadtest

import (
	"fmt"
	"io"
	"sort"
	"time"
)

func millis(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d.Microseconds())/1000)
}

// lineWriter writes formatted lines and remembers the first failure, so the
// report can be built as a straight sequence of lines and still report a
// broken pipe to the caller.
type lineWriter struct {
	w   io.Writer
	err error
}

func (l *lineWriter) printf(format string, args ...any) {
	if l.err != nil {
		return
	}
	_, l.err = fmt.Fprintf(l.w, format, args...)
}

func (l *lineWriter) blank() { l.printf("\n") }

// Write prints the report in a fixed order so two runs can be compared by eye.
func (r *Report) Write(w io.Writer) error {
	out := &lineWriter{w: w}
	s := r.Summary

	out.printf("target            %s\n", r.Config.BaseURL)
	out.printf("workers           %d\n", r.Config.Workers)
	out.printf("duration          %s requested, %s elapsed\n",
		r.Config.Duration, s.Elapsed.Round(time.Millisecond))
	out.printf("loan              principal %d cents, apr %d bps, %d installments, %s\n",
		r.Config.PrincipalCents, r.Config.APRBps, r.Config.InstallmentCount, r.Config.Cadence)
	out.blank()

	out.printf("requests sent     %d\n", s.Total)
	out.printf("throughput        %.1f req/s\n", s.Throughput())
	out.printf("succeeded (2xx)   %d\n", s.Successes())
	out.printf("transport errors  %d\n", s.TransportErrors)
	out.blank()

	out.printf("by status code\n")
	codes := make([]int, 0, len(s.ByStatus))
	for code := range s.ByStatus {
		codes = append(codes, code)
	}
	sort.Ints(codes)
	for _, code := range codes {
		out.printf("  %-3d             %d\n", code, s.ByStatus[code])
	}
	out.blank()

	out.printf("latency\n")
	out.printf("  min             %s\n", millis(s.Min))
	out.printf("  p50             %s\n", millis(s.P50))
	out.printf("  p95             %s\n", millis(s.P95))
	out.printf("  p99             %s\n", millis(s.P99))
	out.printf("  max             %s\n", millis(s.Max))
	out.blank()

	out.printf("retry phase       loan %s, %d distinct keys, paid %d of %d cents\n",
		r.RetryLoan.ID, r.RetryKeyCount, r.RetryLoan.PaidCents, r.RetryLoan.TotalCents)
	out.printf("overpay phase     loan %s, %d distinct keys, paid %d of %d cents, status %s\n",
		r.OverpayLoan.ID, r.OverpayKeyCount, r.OverpayLoan.PaidCents,
		r.OverpayLoan.TotalCents, r.OverpayLoan.Status)
	out.blank()

	out.printf("invariants\n")
	for _, result := range r.Invariants {
		verdict := "PASS"
		if !result.Pass {
			verdict = "FAIL"
		}
		out.printf("  %-4s %-52s %s\n", verdict, result.Name, result.Detail)
	}
	out.blank()

	if r.Pass() {
		out.printf("RESULT PASS\n")
	} else {
		out.printf("RESULT FAIL, %d invariant(s) broken\n", len(r.Invariants.Failed()))
	}
	return out.err
}
