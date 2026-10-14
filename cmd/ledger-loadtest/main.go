// Command ledger-loadtest drives concurrent traffic at an installment ledger
// service and checks that it stayed correct under it.
//
// It exits 0 only when every invariant held. Any broken invariant exits 1, and
// a run that could not be carried out at all exits 2.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vamsi513/ledger-loadtest/internal/loadtest"
)

func main() {
	var cfg loadtest.Config
	flag.StringVar(&cfg.BaseURL, "base-url", "http://127.0.0.1:8000", "base URL of the installment ledger service")
	flag.IntVar(&cfg.Workers, "workers", 16, "number of concurrent workers")
	flag.DurationVar(&cfg.Duration, "duration", 10*time.Second, "how long to send load, split evenly between the two phases")
	flag.DurationVar(&cfg.RequestTimeout, "timeout", 10*time.Second, "per request timeout")
	flag.Int64Var(&cfg.PrincipalCents, "principal-cents", 120000, "loan principal in cents")
	flag.IntVar(&cfg.APRBps, "apr-bps", 1200, "annual rate in basis points, 1200 is 12 percent")
	flag.IntVar(&cfg.InstallmentCount, "installments", 4, "number of installments")
	flag.StringVar(&cfg.Cadence, "cadence", "biweekly", "installment cadence")
	flag.StringVar(&cfg.StartDate, "start-date", "2026-01-15", "loan start date, YYYY-MM-DD")
	flag.IntVar(&cfg.RetryKeys, "retry-keys", 16, "distinct idempotency keys reused during the retry phase")
	flag.StringVar(&cfg.RunID, "run-id", "", "prefix for idempotency keys, defaults to a timestamp")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	report, err := loadtest.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "could not complete the run: %v\n", err)
		os.Exit(2)
	}

	if err := report.Write(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "could not write the report: %v\n", err)
		os.Exit(2)
	}
	if !report.Pass() {
		os.Exit(1)
	}
}
