package loadtest

import (
	"context"
	"strings"
	"testing"
	"time"
)

func testConfig(baseURL string) Config {
	return Config{
		BaseURL:          baseURL,
		Workers:          8,
		Duration:         400 * time.Millisecond,
		RequestTimeout:   5 * time.Second,
		PrincipalCents:   120000,
		APRBps:           0,
		InstallmentCount: 4,
		Cadence:          "biweekly",
		StartDate:        "2026-01-15",
		RetryKeys:        4,
		RunID:            "test",
	}
}

func named(t *testing.T, report *Report, substring string) (bool, string) {
	t.Helper()
	for _, result := range report.Invariants {
		if strings.Contains(result.Name, substring) {
			return result.Pass, result.Detail
		}
	}
	t.Fatalf("no invariant whose name contains %q", substring)
	return false, ""
}

func TestRunAgainstCorrectAPIPasses(t *testing.T) {
	server := newFakeAPI(modeCorrect).server()
	defer server.Close()

	report, err := Run(context.Background(), testConfig(server.URL))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	if !report.Pass() {
		for _, result := range report.Invariants.Failed() {
			t.Errorf("unexpected FAIL %s: %s", result.Name, result.Detail)
		}
		t.Fatal("a correct service should pass every invariant")
	}

	if report.Summary.Total == 0 {
		t.Fatal("no requests were recorded")
	}
	if report.Summary.Successes() == 0 {
		t.Fatal("no request succeeded")
	}
	if report.RetryKeyCount != 4 {
		t.Fatalf("retry phase used %d distinct keys, want 4", report.RetryKeyCount)
	}
	if report.OverpayKeyCount < 5 {
		t.Fatalf("overpay phase used %d distinct keys, want more than the four that fit", report.OverpayKeyCount)
	}
}

func TestRunDetectsDoubleAppliedRetry(t *testing.T) {
	server := newFakeAPI(modeDoubleApply).server()
	defer server.Close()

	report, err := Run(context.Background(), testConfig(server.URL))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	if report.Pass() {
		t.Fatal("a service that double applies a retried payment must not pass")
	}

	pass, detail := named(t, report, "retry loan: paid total matches accepted requests")
	if pass {
		t.Fatal("the paid total check should have caught the double application")
	}
	if !strings.Contains(detail, "difference") {
		t.Fatalf("failure detail does not quantify the gap: %s", detail)
	}
	t.Logf("caught as expected: %s", detail)
}

func TestRunDetectsOverpayment(t *testing.T) {
	server := newFakeAPI(modeAllowOverpay).server()
	defer server.Close()

	report, err := Run(context.Background(), testConfig(server.URL))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	if report.Pass() {
		t.Fatal("a service that allows overpayment must not pass")
	}

	if pass, detail := named(t, report, "overpay loan: no overpayment"); pass {
		t.Fatal("the overpayment check should have failed")
	} else {
		t.Logf("caught as expected: %s", detail)
	}
}

func TestRunFailsWhenServiceIsUnreachable(t *testing.T) {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.RequestTimeout = 500 * time.Millisecond

	if _, err := Run(context.Background(), cfg); err == nil {
		t.Fatal("expected an error when the service is unreachable")
	}
}

func TestRunHonoursCancellation(t *testing.T) {
	server := newFakeAPI(modeCorrect).server()
	defer server.Close()

	cfg := testConfig(server.URL)
	cfg.Duration = 30 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	started := time.Now()
	report, err := Run(ctx, cfg)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if elapsed > 10*time.Second {
		t.Fatalf("cancellation took %s to take effect", elapsed)
	}
	if report.Summary.Total == 0 {
		t.Fatal("expected some requests before cancellation")
	}
}

func TestConfigDefaults(t *testing.T) {
	var cfg Config
	cfg.Defaults()

	if cfg.Workers != 16 || cfg.Duration != 10*time.Second || cfg.RetryKeys != 16 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if cfg.Cadence != "biweekly" || cfg.StartDate == "" || cfg.RunID == "" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}

	set := Config{Workers: 3, Duration: time.Second, RetryKeys: 2, Cadence: "weekly", RunID: "fixed"}
	set.Defaults()
	if set.Workers != 3 || set.Duration != time.Second || set.RetryKeys != 2 ||
		set.Cadence != "weekly" || set.RunID != "fixed" {
		t.Fatalf("Defaults overwrote values that were already set: %+v", set)
	}
}

func TestReportWriteIncludesVerdictAndPercentiles(t *testing.T) {
	server := newFakeAPI(modeCorrect).server()
	defer server.Close()

	report, err := Run(context.Background(), testConfig(server.URL))
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}

	var out strings.Builder
	if err := report.Write(&out); err != nil {
		t.Fatalf("Write returned an error: %v", err)
	}
	text := out.String()

	for _, want := range []string{"requests sent", "throughput", "p50", "p95", "p99", "invariants", "RESULT PASS"} {
		if !strings.Contains(text, want) {
			t.Errorf("report does not mention %q", want)
		}
	}
}
