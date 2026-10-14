// Package loadtest drives concurrent traffic at an installment ledger service
// and then checks that the service kept its promises.
//
// The run has two phases, because the two properties under test need opposite
// traffic shapes. The retry phase sends a small set of idempotency keys over
// and over, so the same key is in flight several times at once. The overpay
// phase sends a stream of distinct keys whose amounts together come to more
// than the loan is worth, so the workers race for the last installment.
package loadtest

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/vamsi513/ledger-loadtest/internal/client"
	"github.com/vamsi513/ledger-loadtest/internal/invariant"
	"github.com/vamsi513/ledger-loadtest/internal/stats"
)

// Config is everything the run needs.
type Config struct {
	BaseURL          string
	Workers          int
	Duration         time.Duration
	RequestTimeout   time.Duration
	PrincipalCents   int64
	APRBps           int
	InstallmentCount int
	Cadence          string
	StartDate        string
	RetryKeys        int
	RunID            string
}

// Defaults fills in anything the caller left at its zero value.
func (c *Config) Defaults() {
	if c.Workers <= 0 {
		c.Workers = 16
	}
	if c.Duration <= 0 {
		c.Duration = 10 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 10 * time.Second
	}
	if c.PrincipalCents <= 0 {
		c.PrincipalCents = 1_200_00
	}
	if c.InstallmentCount <= 0 {
		c.InstallmentCount = 4
	}
	if c.Cadence == "" {
		c.Cadence = "biweekly"
	}
	if c.StartDate == "" {
		c.StartDate = "2026-01-15"
	}
	if c.RetryKeys <= 0 {
		c.RetryKeys = 16
	}
	if c.RunID == "" {
		c.RunID = fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
}

// Report is the outcome of a whole run.
type Report struct {
	Config          Config
	Summary         stats.Summary
	Invariants      invariant.Results
	RetryLoan       client.Loan
	OverpayLoan     client.Loan
	RetryKeyCount   int
	OverpayKeyCount int
}

// Pass reports whether every invariant held.
func (r *Report) Pass() bool { return r.Invariants.AllPass() }

type job struct {
	loanID      string
	key         string
	amountCents int64
}

type outcome struct {
	key         string
	amountCents int64
	status      int
	latency     time.Duration
	paymentID   string
}

// Run creates the loans, drives both phases, reads the loans and ledgers back
// and returns the report. A non nil error means the run could not be carried
// out at all, which is different from a run that completed and failed a check.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	cfg.Defaults()
	api := client.New(cfg.BaseURL, cfg.RequestTimeout, cfg.Workers*2)

	if err := api.Health(ctx); err != nil {
		return nil, fmt.Errorf("service is not healthy at %s: %w", cfg.BaseURL, err)
	}

	loanRequest := client.CreateLoanRequest{
		PrincipalCents:   cfg.PrincipalCents,
		APRBps:           cfg.APRBps,
		InstallmentCount: cfg.InstallmentCount,
		Cadence:          cfg.Cadence,
		StartDate:        cfg.StartDate,
	}

	retryLoan, _, err := api.CreateLoan(ctx, cfg.RunID+"-loan-retry", loanRequest)
	if err != nil {
		return nil, fmt.Errorf("create loan for the retry phase: %w", err)
	}
	overpayLoan, _, err := api.CreateLoan(ctx, cfg.RunID+"-loan-overpay", loanRequest)
	if err != nil {
		return nil, fmt.Errorf("create loan for the overpay phase: %w", err)
	}

	recorder := stats.NewRecorder()
	retryOutcomes := make(map[string]*invariant.KeyOutcome)
	overpayOutcomes := make(map[string]*invariant.KeyOutcome)

	half := cfg.Duration / 2
	started := time.Now()

	// Retry phase. Each key asks for a slice of the loan small enough that
	// every key can be accepted without closing the loan, so the phase can
	// keep resending the same keys for its whole duration.
	retryAmount := retryLoan.TotalCents / int64(cfg.RetryKeys*2)
	if retryAmount < 1 {
		retryAmount = 1
	}
	retryKeys := make([]string, cfg.RetryKeys)
	for i := range retryKeys {
		retryKeys[i] = fmt.Sprintf("%s-retry-%d", cfg.RunID, i)
	}
	phase(ctx, api, recorder, retryOutcomes, half, cfg.Workers, func(i int) job {
		return job{loanID: retryLoan.ID, key: retryKeys[i%len(retryKeys)], amountCents: retryAmount}
	})

	// Overpay phase. Every key is distinct and asks for a quarter of the loan,
	// so after four are applied the rest must be refused.
	overpayAmount := overpayLoan.TotalCents / 4
	if overpayAmount < 1 {
		overpayAmount = 1
	}
	phase(ctx, api, recorder, overpayOutcomes, cfg.Duration-half, cfg.Workers, func(i int) job {
		return job{loanID: overpayLoan.ID, key: fmt.Sprintf("%s-overpay-%d", cfg.RunID, i), amountCents: overpayAmount}
	})

	elapsed := time.Since(started)

	// Read everything back with a context that outlives the load, so the
	// verification step still runs when the load ran to its deadline.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	finalRetry, _, err := api.GetLoan(readCtx, retryLoan.ID)
	if err != nil {
		return nil, fmt.Errorf("read back the retry loan: %w", err)
	}
	finalOverpay, _, err := api.GetLoan(readCtx, overpayLoan.ID)
	if err != nil {
		return nil, fmt.Errorf("read back the overpay loan: %w", err)
	}
	retryLedger, _, err := api.GetLedger(readCtx, retryLoan.ID)
	if err != nil {
		return nil, fmt.Errorf("read back the retry ledger: %w", err)
	}
	overpayLedger, _, err := api.GetLedger(readCtx, overpayLoan.ID)
	if err != nil {
		return nil, fmt.Errorf("read back the overpay ledger: %w", err)
	}

	report := &Report{
		Config:          cfg,
		Summary:         recorder.Summarise(elapsed),
		RetryLoan:       finalRetry,
		OverpayLoan:     finalOverpay,
		RetryKeyCount:   len(retryOutcomes),
		OverpayKeyCount: len(overpayOutcomes),
	}
	report.Invariants = check(finalRetry, finalOverpay, retryLedger, overpayLedger, retryOutcomes, overpayOutcomes)
	return report, nil
}

func check(
	retryLoan, overpayLoan client.Loan,
	retryLedger, overpayLedger client.Ledger,
	retryOutcomes, overpayOutcomes map[string]*invariant.KeyOutcome,
) invariant.Results {
	prefix := func(tag string, r invariant.Result) invariant.Result {
		r.Name = tag + ": " + r.Name
		return r
	}
	return invariant.Results{
		prefix("retry loan", invariant.InstallmentsSumToTotal(retryLoan)),
		prefix("retry loan", invariant.NoNegativeBalance(retryLoan)),
		prefix("retry loan", invariant.NoOverpayment(retryLoan)),
		prefix("retry loan", invariant.LedgerBalances(retryLedger)),
		prefix("retry loan", invariant.IdempotencyAppliedAtMostOnce(retryOutcomes)),
		prefix("retry loan", invariant.PaidMatchesAcceptedKeys(retryLoan, retryOutcomes)),
		prefix("overpay loan", invariant.InstallmentsSumToTotal(overpayLoan)),
		prefix("overpay loan", invariant.NoNegativeBalance(overpayLoan)),
		prefix("overpay loan", invariant.NoOverpayment(overpayLoan)),
		prefix("overpay loan", invariant.LedgerBalances(overpayLedger)),
		prefix("overpay loan", invariant.IdempotencyAppliedAtMostOnce(overpayOutcomes)),
		prefix("overpay loan", invariant.PaidMatchesAcceptedKeys(overpayLoan, overpayOutcomes)),
	}
}

// phase runs a worker pool for the given duration. Workers read jobs from a
// channel and write outcomes to another, and a single collector owns the maps
// so no lock is needed around them.
//
// Reaching the deadline is how a phase normally ends, and an interrupt is a
// deliberate early stop, so neither is an error: both leave the recorded
// outcomes intact for the caller to verify and report.
func phase(
	ctx context.Context,
	api *client.Client,
	recorder *stats.Recorder,
	outcomes map[string]*invariant.KeyOutcome,
	duration time.Duration,
	workers int,
	next func(i int) job,
) {
	phaseCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()

	jobs := make(chan job)
	results := make(chan outcome, workers)

	var feeder sync.WaitGroup
	feeder.Add(1)
	go func() {
		defer feeder.Done()
		defer close(jobs)
		for i := 0; ; i++ {
			select {
			case <-phaseCtx.Done():
				return
			case jobs <- next(i):
			}
		}
	}()

	var pool sync.WaitGroup
	for range workers {
		pool.Add(1)
		go func() {
			defer pool.Done()
			for item := range jobs {
				payment, result, err := api.Pay(phaseCtx, item.loanID, item.key, item.amountCents)
				out := outcome{
					key:         item.key,
					amountCents: item.amountCents,
					status:      result.Status,
					latency:     result.Latency,
				}
				if err == nil {
					out.paymentID = payment.ID
				}
				select {
				case results <- out:
				case <-phaseCtx.Done():
					return
				}
			}
		}()
	}

	var collector sync.WaitGroup
	collector.Add(1)
	go func() {
		defer collector.Done()
		for out := range results {
			// A request cancelled by the phase deadline says nothing about the
			// service, so it is not recorded as a transport failure.
			if out.status == 0 && phaseCtx.Err() != nil {
				continue
			}
			recorder.Add(stats.Outcome{Status: out.status, Latency: out.latency})

			entry, ok := outcomes[out.key]
			if !ok {
				entry = &invariant.KeyOutcome{AmountCents: out.amountCents, PaymentIDs: map[string]struct{}{}}
				outcomes[out.key] = entry
			}
			entry.Attempts++
			if out.status >= 200 && out.status < 300 {
				entry.Accepted = true
				if out.paymentID != "" {
					entry.PaymentIDs[out.paymentID] = struct{}{}
				}
			}
		}
	}()

	pool.Wait()
	feeder.Wait()
	close(results)
	collector.Wait()
}
