// Package client is a thin HTTP client for the installment ledger API.
//
// It mirrors only what the load tester needs: create a loan, pay a loan, and
// read the loan and its ledger back. Every amount on the wire is an integer
// number of cents, so every amount here is an int64 and never a float.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Installment is one row of a loan schedule.
type Installment struct {
	ID               string `json:"id"`
	Sequence         int    `json:"sequence"`
	DueDate          string `json:"due_date"`
	AmountCents      int64  `json:"amount_cents"`
	PaidCents        int64  `json:"paid_cents"`
	OutstandingCents int64  `json:"outstanding_cents"`
}

// Loan is the loan body returned by the API.
type Loan struct {
	ID               string        `json:"id"`
	Status           string        `json:"status"`
	PrincipalCents   int64         `json:"principal_cents"`
	APRBps           int           `json:"apr_bps"`
	InstallmentCount int           `json:"installment_count"`
	Cadence          string        `json:"cadence"`
	StartDate        string        `json:"start_date"`
	TermDays         int           `json:"term_days"`
	InterestCents    int64         `json:"interest_cents"`
	TotalCents       int64         `json:"total_cents"`
	PaidCents        int64         `json:"paid_cents"`
	OutstandingCents int64         `json:"outstanding_cents"`
	Installments     []Installment `json:"installments"`
}

// Payment is the payment body returned by the API.
type Payment struct {
	ID            string `json:"id"`
	LoanID        string `json:"loan_id"`
	AmountCents   int64  `json:"amount_cents"`
	TransactionID string `json:"transaction_id"`
	Loan          Loan   `json:"loan"`
}

// LedgerEntry is one side of a ledger transaction.
type LedgerEntry struct {
	Account     string `json:"account"`
	Direction   string `json:"direction"`
	AmountCents int64  `json:"amount_cents"`
}

// LedgerTransaction is a group of entries that must net to zero.
type LedgerTransaction struct {
	TransactionID string        `json:"transaction_id"`
	DebitCents    int64         `json:"debit_cents"`
	CreditCents   int64         `json:"credit_cents"`
	NetCents      int64         `json:"net_cents"`
	Entries       []LedgerEntry `json:"entries"`
}

// Ledger is the whole ledger for one loan.
type Ledger struct {
	LoanID       string              `json:"loan_id"`
	DebitCents   int64               `json:"debit_cents"`
	CreditCents  int64               `json:"credit_cents"`
	NetCents     int64               `json:"net_cents"`
	Transactions []LedgerTransaction `json:"transactions"`
}

// CreateLoanRequest is the POST /loans body.
type CreateLoanRequest struct {
	PrincipalCents   int64  `json:"principal_cents"`
	APRBps           int    `json:"apr_bps"`
	InstallmentCount int    `json:"installment_count"`
	Cadence          string `json:"cadence"`
	StartDate        string `json:"start_date"`
}

// APIError is the error shape the service returns for every failure.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("api error: status %d code %q: %s", e.Status, e.Code, e.Msg)
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client talks to one installment ledger service.
type Client struct {
	baseURL string
	http    *http.Client
}

// New returns a Client for baseURL with the given per request timeout.
//
// Connection reuse matters here: the load tester opens many concurrent
// requests, and the default transport would cap idle connections per host at
// two and then churn sockets, which would show up as latency that belongs to
// the client rather than the service.
func New(baseURL string, timeout time.Duration, maxConns int) *Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = maxConns
	transport.MaxIdleConnsPerHost = maxConns
	transport.MaxConnsPerHost = maxConns
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Timeout: timeout, Transport: transport},
	}
}

// Result carries the status code and latency of one call alongside its body,
// so the caller can record a rejected request without treating it as a bug.
type Result struct {
	Status  int
	Latency time.Duration
}

func (c *Client) do(ctx context.Context, method, path, idempotencyKey string, body, out any) (Result, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return Result{}, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return Result{}, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	if err != nil {
		return Result{Latency: time.Since(start)}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	payload, err := io.ReadAll(resp.Body)
	latency := time.Since(start)
	if err != nil {
		return Result{Status: resp.StatusCode, Latency: latency}, fmt.Errorf("read body: %w", err)
	}

	result := Result{Status: resp.StatusCode, Latency: latency}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope errorEnvelope
		_ = json.Unmarshal(payload, &envelope)
		return result, &APIError{Status: resp.StatusCode, Code: envelope.Error.Code, Msg: envelope.Error.Message}
	}
	if out != nil {
		if err := json.Unmarshal(payload, out); err != nil {
			return result, fmt.Errorf("decode response: %w", err)
		}
	}
	return result, nil
}

// Health reports whether the service answers its health endpoint.
func (c *Client) Health(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodGet, "/health", "", nil, nil)
	return err
}

// CreateLoan creates a loan and returns it.
func (c *Client) CreateLoan(ctx context.Context, key string, req CreateLoanRequest) (Loan, Result, error) {
	var loan Loan
	result, err := c.do(ctx, http.MethodPost, "/loans", key, req, &loan)
	return loan, result, err
}

// GetLoan reads a loan, its schedule and its outstanding balance.
func (c *Client) GetLoan(ctx context.Context, id string) (Loan, Result, error) {
	var loan Loan
	result, err := c.do(ctx, http.MethodGet, "/loans/"+id, "", nil, &loan)
	return loan, result, err
}

// Pay applies a payment to a loan.
func (c *Client) Pay(ctx context.Context, loanID, key string, amountCents int64) (Payment, Result, error) {
	var payment Payment
	body := map[string]int64{"amount_cents": amountCents}
	result, err := c.do(ctx, http.MethodPost, "/loans/"+loanID+"/payments", key, body, &payment)
	return payment, result, err
}

// GetLedger reads the ledger entries for a loan, grouped by transaction.
func (c *Client) GetLedger(ctx context.Context, loanID string) (Ledger, Result, error) {
	var ledger Ledger
	result, err := c.do(ctx, http.MethodGet, "/loans/"+loanID+"/ledger", "", nil, &ledger)
	return ledger, result, err
}
