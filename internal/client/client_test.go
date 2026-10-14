package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestClient(handler http.Handler) (*Client, func()) {
	server := httptest.NewServer(handler)
	return New(server.URL, 2*time.Second, 4), server.Close
}

func TestPaySendsIdempotencyKeyAndBody(t *testing.T) {
	var gotKey, gotContentType, gotMethod, gotPath string
	var gotBody []byte

	c, done := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Idempotency-Key")
		gotContentType = r.Header.Get("Content-Type")
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = make([]byte, r.ContentLength)
		_, _ = r.Body.Read(gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"pay-1","amount_cents":500}`))
	}))
	defer done()

	payment, result, err := c.Pay(context.Background(), "loan-7", "key-abc", 500)
	if err != nil {
		t.Fatalf("Pay returned an error: %v", err)
	}
	if gotKey != "key-abc" {
		t.Errorf("Idempotency-Key = %q, want %q", gotKey, "key-abc")
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q", gotContentType)
	}
	if gotMethod != http.MethodPost || gotPath != "/loans/loan-7/payments" {
		t.Errorf("request was %s %s", gotMethod, gotPath)
	}
	if string(gotBody) != `{"amount_cents":500}` {
		t.Errorf("body = %s", gotBody)
	}
	if payment.ID != "pay-1" || payment.AmountCents != 500 {
		t.Errorf("decoded payment = %+v", payment)
	}
	if result.Status != http.StatusCreated {
		t.Errorf("Status = %d, want 201", result.Status)
	}
	if result.Latency <= 0 {
		t.Error("Latency was not measured")
	}
}

func TestErrorEnvelopeIsParsed(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		body     string
		wantCode string
	}{
		{"overpayment", http.StatusUnprocessableEntity,
			`{"error":{"code":"overpayment","message":"too much","details":{}}}`, "overpayment"},
		{"loan closed", http.StatusConflict,
			`{"error":{"code":"loan_closed","message":"closed","details":{}}}`, "loan_closed"},
		{"body is not the expected shape", http.StatusInternalServerError, `not json at all`, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c, done := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer done()

			_, result, err := c.Pay(context.Background(), "loan-1", "key-1", 100)
			if err == nil {
				t.Fatal("expected an error for a non 2xx response")
			}
			if result.Status != tc.status {
				t.Errorf("Status = %d, want %d", result.Status, tc.status)
			}

			var apiErr *APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("error is not an *APIError: %v", err)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if apiErr.Status != tc.status {
				t.Errorf("APIError.Status = %d, want %d", apiErr.Status, tc.status)
			}
			if apiErr.Error() == "" {
				t.Error("Error() is empty")
			}
		})
	}
}

func TestGetLoanDecodesSchedule(t *testing.T) {
	c, done := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"id":"loan-1","status":"active","total_cents":1000,"paid_cents":334,
			"outstanding_cents":666,"installment_count":3,
			"installments":[
				{"sequence":1,"amount_cents":334,"paid_cents":334,"outstanding_cents":0},
				{"sequence":2,"amount_cents":333,"paid_cents":0,"outstanding_cents":333},
				{"sequence":3,"amount_cents":333,"paid_cents":0,"outstanding_cents":333}
			]}`))
	}))
	defer done()

	loan, _, err := c.GetLoan(context.Background(), "loan-1")
	if err != nil {
		t.Fatalf("GetLoan returned an error: %v", err)
	}
	if len(loan.Installments) != 3 {
		t.Fatalf("decoded %d installments, want 3", len(loan.Installments))
	}
	if loan.TotalCents != 1000 || loan.PaidCents != 334 || loan.OutstandingCents != 666 {
		t.Errorf("decoded loan = %+v", loan)
	}
}

func TestGetLedgerDecodesTransactions(t *testing.T) {
	c, done := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"loan_id":"loan-1","debit_cents":500,"credit_cents":500,"net_cents":0,
			"transactions":[{"transaction_id":"t1","debit_cents":500,"credit_cents":500,"net_cents":0,
				"entries":[
					{"account":"cash","direction":"debit","amount_cents":500},
					{"account":"loan_receivable","direction":"credit","amount_cents":500}]}]}`))
	}))
	defer done()

	ledger, _, err := c.GetLedger(context.Background(), "loan-1")
	if err != nil {
		t.Fatalf("GetLedger returned an error: %v", err)
	}
	if len(ledger.Transactions) != 1 || len(ledger.Transactions[0].Entries) != 2 {
		t.Fatalf("decoded ledger = %+v", ledger)
	}
	if ledger.NetCents != 0 {
		t.Errorf("NetCents = %d, want 0", ledger.NetCents)
	}
}

func TestHealthFailsWhenServiceIsDown(t *testing.T) {
	c := New("http://127.0.0.1:1", 300*time.Millisecond, 2)
	if err := c.Health(context.Background()); err == nil {
		t.Fatal("expected an error against a closed port")
	}
}

func TestContextCancellationIsReported(t *testing.T) {
	c, done := newTestClient(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(2 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	_, result, err := c.GetLoan(ctx, "loan-1")
	if err == nil {
		t.Fatal("expected an error when the context expires")
	}
	if result.Status != 0 {
		t.Errorf("Status = %d, want 0 for a request that never got a response", result.Status)
	}
}
