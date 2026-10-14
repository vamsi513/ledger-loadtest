package loadtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// mode selects how the fake service misbehaves.
type mode int

const (
	// modeCorrect behaves the way the real service is specified to behave.
	modeCorrect mode = iota
	// modeDoubleApply applies a retried idempotency key a second time while
	// still returning the original payment body, which is the subtle bug: the
	// caller sees one payment per key but the money moves twice.
	modeDoubleApply
	// modeAllowOverpay accepts payments past the outstanding balance.
	modeAllowOverpay
)

type fakeInstallment struct {
	sequence int
	amount   int64
	paid     int64
}

type fakePayment struct {
	id     string
	amount int64
}

type fakeLoan struct {
	id           string
	principal    int64
	count        int
	installments []*fakeInstallment
	payments     []fakePayment
	transactions []map[string]any
}

func (l *fakeLoan) total() int64 {
	var sum int64
	for _, item := range l.installments {
		sum += item.amount
	}
	return sum
}

func (l *fakeLoan) paid() int64 {
	var sum int64
	for _, item := range l.installments {
		sum += item.paid
	}
	return sum
}

func (l *fakeLoan) outstanding() int64 { return l.total() - l.paid() }

// apply fills the oldest unpaid installment first. It allows the balance to go
// negative so the overpay mode can be expressed by simply skipping the guard.
func (l *fakeLoan) apply(amount int64) {
	left := amount
	for _, item := range l.installments {
		if left <= 0 {
			break
		}
		room := item.amount - item.paid
		if room <= 0 {
			continue
		}
		take := min(room, left)
		item.paid += take
		left -= take
	}
	if left > 0 && len(l.installments) > 0 {
		// Nothing left to fill, so the excess lands on the last installment.
		// Only reachable in the overpay mode.
		l.installments[len(l.installments)-1].paid += left
	}
}

type fakeAPI struct {
	mode mode

	mu      sync.Mutex
	loans   map[string]*fakeLoan
	keys    map[string]json.RawMessage
	keySeen map[string]int
	nextID  int
}

func newFakeAPI(m mode) *fakeAPI {
	return &fakeAPI{
		mode:    m,
		loans:   map[string]*fakeLoan{},
		keys:    map[string]json.RawMessage{},
		keySeen: map[string]int{},
	}
}

func (f *fakeAPI) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"code": code, "message": message, "details": map[string]any{}},
	})
}

func (f *fakeAPI) installmentBodies(l *fakeLoan) []map[string]any {
	out := make([]map[string]any, 0, len(l.installments))
	for _, item := range l.installments {
		out = append(out, map[string]any{
			"id":                fmt.Sprintf("%s-i%d", l.id, item.sequence),
			"sequence":          item.sequence,
			"due_date":          "2026-01-29",
			"amount_cents":      item.amount,
			"paid_cents":        item.paid,
			"outstanding_cents": item.amount - item.paid,
		})
	}
	return out
}

func (f *fakeAPI) loanBody(l *fakeLoan) map[string]any {
	return map[string]any{
		"id":                l.id,
		"status":            map[bool]string{true: "closed", false: "active"}[l.outstanding() <= 0],
		"principal_cents":   l.principal,
		"apr_bps":           0,
		"installment_count": l.count,
		"cadence":           "biweekly",
		"start_date":        "2026-01-15",
		"term_days":         56,
		"interest_cents":    int64(0),
		"total_cents":       l.total(),
		"paid_cents":        l.paid(),
		"outstanding_cents": l.outstanding(),
		"created_at":        "2026-01-15T00:00:00",
		"installments":      f.installmentBodies(l),
	}
}

func (f *fakeAPI) ledgerBody(l *fakeLoan) map[string]any {
	var debit, credit, net int64
	for _, t := range l.transactions {
		debit += t["debit_cents"].(int64)
		credit += t["credit_cents"].(int64)
		net += t["net_cents"].(int64)
	}
	return map[string]any{
		"loan_id":      l.id,
		"debit_cents":  debit,
		"credit_cents": credit,
		"net_cents":    net,
		"transactions": l.transactions,
	}
}

func transaction(id string, entries ...map[string]any) map[string]any {
	var debit, credit int64
	for _, e := range entries {
		switch e["direction"] {
		case "debit":
			debit += e["amount_cents"].(int64)
		case "credit":
			credit += e["amount_cents"].(int64)
		}
	}
	return map[string]any{
		"transaction_id": id,
		"created_at":     "2026-01-15T00:00:00",
		"debit_cents":    debit,
		"credit_cents":   credit,
		"net_cents":      debit - credit,
		"entries":        entries,
	}
}

func entry(account, direction string, amount int64) map[string]any {
	return map[string]any{"account": account, "direction": direction, "amount_cents": amount}
}

func (f *fakeAPI) server() *httptest.Server {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})

	mux.HandleFunc("POST /loans", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key is required")
			return
		}
		var req struct {
			PrincipalCents   int64 `json:"principal_cents"`
			InstallmentCount int   `json:"installment_count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "bad body")
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()
		if stored, ok := f.keys[key]; ok {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(stored)
			return
		}

		loan := &fakeLoan{id: f.id("loan"), principal: req.PrincipalCents, count: req.InstallmentCount}
		base := req.PrincipalCents / int64(req.InstallmentCount)
		remainder := req.PrincipalCents % int64(req.InstallmentCount)
		for i := range req.InstallmentCount {
			amount := base
			if int64(i) < remainder {
				amount++
			}
			loan.installments = append(loan.installments, &fakeInstallment{sequence: i + 1, amount: amount})
		}
		loan.transactions = append(loan.transactions, transaction(f.id("txn"),
			entry("loan_receivable", "debit", loan.total()),
			entry("funding_payable", "credit", loan.total()),
		))
		f.loans[loan.id] = loan

		body, _ := json.Marshal(f.loanBody(loan))
		f.keys[key] = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})

	mux.HandleFunc("GET /loans/{id}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		loan, ok := f.loans[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "loan_not_found", "no such loan")
			return
		}
		writeJSON(w, http.StatusOK, f.loanBody(loan))
	})

	mux.HandleFunc("GET /loans/{id}/ledger", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		loan, ok := f.loans[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "loan_not_found", "no such loan")
			return
		}
		writeJSON(w, http.StatusOK, f.ledgerBody(loan))
	})

	mux.HandleFunc("POST /loans/{id}/payments", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			writeError(w, http.StatusBadRequest, "missing_idempotency_key", "Idempotency-Key is required")
			return
		}
		var req struct {
			AmountCents int64 `json:"amount_cents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusUnprocessableEntity, "validation_error", "bad body")
			return
		}

		f.mu.Lock()
		defer f.mu.Unlock()

		loan, ok := f.loans[r.PathValue("id")]
		if !ok {
			writeError(w, http.StatusNotFound, "loan_not_found", "no such loan")
			return
		}

		stored, replay := f.keys[key]
		f.keySeen[key]++

		if replay {
			// The bug: apply the amount again on a replay, but still return the
			// original response so the caller cannot tell from the body.
			if f.mode == modeDoubleApply {
				loan.apply(req.AmountCents)
				loan.transactions = append(loan.transactions, transaction(f.id("txn"),
					entry("cash", "debit", req.AmountCents),
					entry("loan_receivable", "credit", req.AmountCents),
				))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(stored)
			return
		}

		if f.mode != modeAllowOverpay {
			if loan.outstanding() <= 0 {
				writeError(w, http.StatusConflict, "loan_closed", "loan is closed")
				return
			}
			if req.AmountCents > loan.outstanding() {
				writeError(w, http.StatusUnprocessableEntity, "overpayment",
					"payment exceeds the outstanding balance")
				return
			}
		}

		loan.apply(req.AmountCents)
		payment := fakePayment{id: f.id("pay"), amount: req.AmountCents}
		loan.payments = append(loan.payments, payment)
		txnID := f.id("txn")
		loan.transactions = append(loan.transactions, transaction(txnID,
			entry("cash", "debit", req.AmountCents),
			entry("loan_receivable", "credit", req.AmountCents),
		))

		body, _ := json.Marshal(map[string]any{
			"id":             payment.id,
			"loan_id":        loan.id,
			"amount_cents":   payment.amount,
			"transaction_id": txnID,
			"created_at":     "2026-01-15T00:00:00",
			"allocations":    []map[string]any{},
			"loan":           f.loanBody(loan),
		})
		f.keys[key] = body
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(body)
	})

	return httptest.NewServer(mux)
}
