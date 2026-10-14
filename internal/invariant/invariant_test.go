package invariant

import (
	"strings"
	"testing"

	"github.com/vamsi513/ledger-loadtest/internal/client"
)

func loan(total, paid, outstanding int64, amounts ...int64) client.Loan {
	l := client.Loan{
		ID:               "loan-1",
		TotalCents:       total,
		PaidCents:        paid,
		OutstandingCents: outstanding,
		InstallmentCount: len(amounts),
	}
	for i, amount := range amounts {
		l.Installments = append(l.Installments, client.Installment{
			Sequence:         i + 1,
			AmountCents:      amount,
			OutstandingCents: amount,
		})
	}
	return l
}

func TestInstallmentsSumToTotal(t *testing.T) {
	tests := []struct {
		name string
		loan client.Loan
		want bool
	}{
		{"exact split", loan(1000, 0, 1000, 334, 333, 333), true},
		{"single installment", loan(500, 0, 500, 500), true},
		{"sum is one cent short", loan(1000, 0, 1000, 333, 333, 333), false},
		{"sum is one cent over", loan(1000, 0, 1000, 334, 334, 333), false},
		{"no installments at all", loan(1000, 0, 1000), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := InstallmentsSumToTotal(tc.loan); got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
		})
	}
}

func TestInstallmentsSumToTotalRejectsWrongCount(t *testing.T) {
	l := loan(1000, 0, 1000, 334, 333, 333)
	l.InstallmentCount = 4
	got := InstallmentsSumToTotal(l)
	if got.Pass {
		t.Fatal("expected FAIL when the schedule length disagrees with installment_count")
	}
}

func TestNoNegativeBalance(t *testing.T) {
	negativeInstallment := loan(1000, 0, 1000, 334, 333, 333)
	negativeInstallment.Installments[1].OutstandingCents = -5

	paidTooMuch := loan(1000, 0, 1000, 334, 333, 333)
	paidTooMuch.Installments[0].PaidCents = 400

	tests := []struct {
		name string
		loan client.Loan
		want bool
	}{
		{"untouched loan", loan(1000, 0, 1000, 334, 333, 333), true},
		{"fully paid loan", loan(1000, 1000, 0, 334, 333, 333), true},
		{"negative outstanding on the loan", loan(1000, 1100, -100, 334, 333, 333), false},
		{"negative paid on the loan", loan(1000, -1, 1001, 334, 333, 333), false},
		{"negative outstanding on an installment", negativeInstallment, false},
		{"installment paid beyond its amount", paidTooMuch, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NoNegativeBalance(tc.loan); got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
		})
	}
}

func TestNoOverpayment(t *testing.T) {
	tests := []struct {
		name string
		loan client.Loan
		want bool
	}{
		{"nothing paid", loan(1000, 0, 1000, 1000), true},
		{"half paid", loan(1000, 400, 600, 1000), true},
		{"exactly paid off", loan(1000, 1000, 0, 1000), true},
		{"paid one cent too much", loan(1000, 1001, 0, 1000), false},
		{"paid and outstanding do not add up", loan(1000, 400, 500, 1000), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := NoOverpayment(tc.loan); got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
		})
	}
}

func ledger(transactions ...client.LedgerTransaction) client.Ledger {
	l := client.Ledger{LoanID: "loan-1", Transactions: transactions}
	for _, t := range transactions {
		l.DebitCents += t.DebitCents
		l.CreditCents += t.CreditCents
		l.NetCents += t.NetCents
	}
	return l
}

func balanced(amount int64) client.LedgerTransaction {
	return client.LedgerTransaction{
		TransactionID: "t1",
		DebitCents:    amount,
		CreditCents:   amount,
		NetCents:      0,
		Entries: []client.LedgerEntry{
			{Account: "cash", Direction: "debit", AmountCents: amount},
			{Account: "loan_receivable", Direction: "credit", AmountCents: amount},
		},
	}
}

func TestLedgerBalances(t *testing.T) {
	lopsided := balanced(500)
	lopsided.Entries[1].AmountCents = 400
	lopsided.CreditCents = 400
	lopsided.NetCents = 100

	badDirection := balanced(500)
	badDirection.Entries[0].Direction = "sideways"

	lyingNet := balanced(500)
	lyingNet.NetCents = 7

	tests := []struct {
		name   string
		ledger client.Ledger
		want   bool
	}{
		{"one balanced transaction", ledger(balanced(500)), true},
		{"several balanced transactions", ledger(balanced(500), balanced(300)), true},
		{"no transactions", ledger(), false},
		{"entries do not net to zero", ledger(lopsided), false},
		{"unknown direction", ledger(badDirection), false},
		{"reported net disagrees with entries", ledger(lyingNet), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := LedgerBalances(tc.ledger); got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
		})
	}
}

func TestLedgerBalancesCatchesTotalsThatDoNotAgree(t *testing.T) {
	l := ledger(balanced(500))
	l.CreditCents = 499
	if LedgerBalances(l).Pass {
		t.Fatal("expected FAIL when the ledger totals disagree")
	}
}

func key(amount int64, accepted bool, attempts int, paymentIDs ...string) *KeyOutcome {
	out := &KeyOutcome{AmountCents: amount, Accepted: accepted, Attempts: attempts, PaymentIDs: map[string]struct{}{}}
	for _, id := range paymentIDs {
		out.PaymentIDs[id] = struct{}{}
	}
	return out
}

func TestIdempotencyAppliedAtMostOnce(t *testing.T) {
	tests := []struct {
		name     string
		outcomes map[string]*KeyOutcome
		want     bool
	}{
		{"no keys", map[string]*KeyOutcome{}, true},
		{"one key one payment", map[string]*KeyOutcome{"a": key(100, true, 1, "p1")}, true},
		{
			"one key retried many times, still one payment",
			map[string]*KeyOutcome{"a": key(100, true, 9, "p1")},
			true,
		},
		{
			"key rejected every time, no payment",
			map[string]*KeyOutcome{"a": key(100, false, 3)},
			true,
		},
		{
			"one key produced two payments",
			map[string]*KeyOutcome{"a": key(100, true, 2, "p1", "p2")},
			false,
		},
		{
			"one bad key among good ones",
			map[string]*KeyOutcome{
				"a": key(100, true, 1, "p1"),
				"b": key(100, true, 4, "p2", "p3"),
				"c": key(100, true, 1, "p4"),
			},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := IdempotencyAppliedAtMostOnce(tc.outcomes)
			if got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
			if !tc.want && !strings.Contains(got.Detail, "applied more than once") {
				t.Fatalf("failure detail does not name the problem: %s", got.Detail)
			}
		})
	}
}

func TestPaidMatchesAcceptedKeys(t *testing.T) {
	tests := []struct {
		name     string
		loan     client.Loan
		outcomes map[string]*KeyOutcome
		want     bool
	}{
		{
			"two accepted keys account for the paid total",
			loan(1000, 300, 700, 1000),
			map[string]*KeyOutcome{"a": key(100, true, 1, "p1"), "b": key(200, true, 1, "p2")},
			true,
		},
		{
			"rejected keys are not counted",
			loan(1000, 100, 900, 1000),
			map[string]*KeyOutcome{"a": key(100, true, 1, "p1"), "b": key(900, false, 3)},
			true,
		},
		{
			"nothing accepted and nothing paid",
			loan(1000, 0, 1000, 1000),
			map[string]*KeyOutcome{"a": key(100, false, 2)},
			true,
		},
		{
			"service applied a retried payment twice",
			loan(1000, 200, 800, 1000),
			map[string]*KeyOutcome{"a": key(100, true, 2, "p1")},
			false,
		},
		{
			"service lost a payment",
			loan(1000, 100, 900, 1000),
			map[string]*KeyOutcome{"a": key(100, true, 1, "p1"), "b": key(100, true, 1, "p2")},
			false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PaidMatchesAcceptedKeys(tc.loan, tc.outcomes); got.Pass != tc.want {
				t.Fatalf("Pass = %v, want %v (detail: %s)", got.Pass, tc.want, got.Detail)
			}
		})
	}
}

func TestResultsAllPassAndFailed(t *testing.T) {
	none := Results{}
	if !none.AllPass() {
		t.Fatal("an empty result set should pass")
	}
	if len(none.Failed()) != 0 {
		t.Fatal("an empty result set has no failures")
	}

	mixed := Results{
		{Name: "a", Pass: true},
		{Name: "b", Pass: false},
		{Name: "c", Pass: true},
		{Name: "d", Pass: false},
	}
	if mixed.AllPass() {
		t.Fatal("AllPass should be false when any check failed")
	}
	if got := len(mixed.Failed()); got != 2 {
		t.Fatalf("Failed() returned %d, want 2", got)
	}
}
