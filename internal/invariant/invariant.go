// Package invariant holds the correctness checks the load tester runs after a
// run has finished.
//
// Each check is a pure function over data already read back from the service,
// so a check can be unit tested without a server and the same code path runs
// against a real service and against a deliberately broken fake.
package invariant

import (
	"fmt"
	"sort"

	"github.com/vamsi513/ledger-loadtest/internal/client"
)

// Result is the outcome of one check.
type Result struct {
	Name   string
	Pass   bool
	Detail string
}

// Results is a list of check outcomes.
type Results []Result

// AllPass reports whether every check passed. An empty list passes.
func (r Results) AllPass() bool {
	for _, item := range r {
		if !item.Pass {
			return false
		}
	}
	return true
}

// Failed returns only the checks that failed.
func (r Results) Failed() Results {
	var out Results
	for _, item := range r {
		if !item.Pass {
			out = append(out, item)
		}
	}
	return out
}

func pass(name, format string, args ...any) Result {
	return Result{Name: name, Pass: true, Detail: fmt.Sprintf(format, args...)}
}

func fail(name, format string, args ...any) Result {
	return Result{Name: name, Pass: false, Detail: fmt.Sprintf(format, args...)}
}

// InstallmentsSumToTotal checks that the schedule adds up to the total
// payable. The service splits the total with a remainder rule, so the sum must
// be exact rather than approximate.
func InstallmentsSumToTotal(loan client.Loan) Result {
	const name = "installments sum to total"
	if len(loan.Installments) == 0 {
		return fail(name, "loan %s returned no installments", loan.ID)
	}
	var sum int64
	for _, item := range loan.Installments {
		sum += item.AmountCents
	}
	if sum != loan.TotalCents {
		return fail(name, "installments sum to %d cents, total_cents is %d, difference %d",
			sum, loan.TotalCents, sum-loan.TotalCents)
	}
	if len(loan.Installments) != loan.InstallmentCount {
		return fail(name, "loan says %d installments, schedule has %d",
			loan.InstallmentCount, len(loan.Installments))
	}
	return pass(name, "%d installments sum to %d cents", len(loan.Installments), sum)
}

// NoNegativeBalance checks that nothing went below zero. A negative
// outstanding balance would mean the service let a payment past the end of the
// loan, and a negative paid amount would mean an entry was applied backwards.
func NoNegativeBalance(loan client.Loan) Result {
	const name = "no negative balance"
	if loan.OutstandingCents < 0 {
		return fail(name, "outstanding_cents is %d", loan.OutstandingCents)
	}
	if loan.PaidCents < 0 {
		return fail(name, "paid_cents is %d", loan.PaidCents)
	}
	for _, item := range loan.Installments {
		if item.OutstandingCents < 0 {
			return fail(name, "installment %d has outstanding_cents %d", item.Sequence, item.OutstandingCents)
		}
		if item.PaidCents > item.AmountCents {
			return fail(name, "installment %d paid %d cents of %d",
				item.Sequence, item.PaidCents, item.AmountCents)
		}
	}
	return pass(name, "outstanding %d cents, paid %d cents", loan.OutstandingCents, loan.PaidCents)
}

// NoOverpayment checks that the loan was never paid beyond its total, and that
// the reported paid and outstanding amounts are consistent with each other.
func NoOverpayment(loan client.Loan) Result {
	const name = "no overpayment"
	if loan.PaidCents > loan.TotalCents {
		return fail(name, "paid %d cents against a total of %d, overpaid by %d",
			loan.PaidCents, loan.TotalCents, loan.PaidCents-loan.TotalCents)
	}
	if loan.PaidCents+loan.OutstandingCents != loan.TotalCents {
		return fail(name, "paid %d plus outstanding %d is %d, total_cents is %d",
			loan.PaidCents, loan.OutstandingCents,
			loan.PaidCents+loan.OutstandingCents, loan.TotalCents)
	}
	return pass(name, "paid %d of %d cents", loan.PaidCents, loan.TotalCents)
}

// LedgerBalances checks the double entry invariant: every transaction nets to
// zero, and so does the ledger as a whole.
func LedgerBalances(ledger client.Ledger) Result {
	const name = "ledger balances"
	if len(ledger.Transactions) == 0 {
		return fail(name, "ledger for loan %s has no transactions", ledger.LoanID)
	}
	for _, transaction := range ledger.Transactions {
		var debits, credits int64
		for _, entry := range transaction.Entries {
			switch entry.Direction {
			case "debit":
				debits += entry.AmountCents
			case "credit":
				credits += entry.AmountCents
			default:
				return fail(name, "transaction %s has entry with direction %q",
					transaction.TransactionID, entry.Direction)
			}
		}
		if debits != credits {
			return fail(name, "transaction %s has %d cents of debits and %d of credits",
				transaction.TransactionID, debits, credits)
		}
		if transaction.NetCents != 0 {
			return fail(name, "transaction %s reports net_cents %d",
				transaction.TransactionID, transaction.NetCents)
		}
	}
	if ledger.NetCents != 0 {
		return fail(name, "ledger reports net_cents %d across %d transactions",
			ledger.NetCents, len(ledger.Transactions))
	}
	if ledger.DebitCents != ledger.CreditCents {
		return fail(name, "ledger has %d cents of debits and %d of credits",
			ledger.DebitCents, ledger.CreditCents)
	}
	return pass(name, "%d transactions, %d cents on each side",
		len(ledger.Transactions), ledger.DebitCents)
}

// KeyOutcome is what the load tester observed for one idempotency key: the
// amount it asked for, and the distinct payment identities the service handed
// back across every attempt that used the key.
type KeyOutcome struct {
	AmountCents int64
	Accepted    bool
	Attempts    int
	PaymentIDs  map[string]struct{}
}

// IdempotencyAppliedAtMostOnce checks that no key produced more than one
// distinct payment. Retrying a key must return the original payment, so a
// second identity for one key means the write ran twice.
func IdempotencyAppliedAtMostOnce(outcomes map[string]*KeyOutcome) Result {
	const name = "each idempotency key applied at most once"
	var offenders []string
	retried := 0
	for key, outcome := range outcomes {
		if outcome.Attempts > 1 {
			retried++
		}
		if len(outcome.PaymentIDs) > 1 {
			offenders = append(offenders, fmt.Sprintf("%s produced %d payments", key, len(outcome.PaymentIDs)))
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		return fail(name, "%d key(s) applied more than once: %v", len(offenders), offenders)
	}
	return pass(name, "%d key(s), %d of them retried, none applied twice", len(outcomes), retried)
}

// PaidMatchesAcceptedKeys checks the loan's paid amount against the sum of the
// accepted keys the tester sent.
//
// This is the check that actually catches a double application. A service that
// applies a retried payment twice still reports one payment per key, but the
// money moves twice, so the paid total exceeds what the tester asked for.
func PaidMatchesAcceptedKeys(loan client.Loan, outcomes map[string]*KeyOutcome) Result {
	const name = "paid total matches accepted requests"
	var expected int64
	accepted := 0
	for _, outcome := range outcomes {
		if outcome.Accepted {
			expected += outcome.AmountCents
			accepted++
		}
	}
	if loan.PaidCents != expected {
		return fail(name, "service reports %d cents paid, %d accepted key(s) asked for %d, difference %d",
			loan.PaidCents, accepted, expected, loan.PaidCents-expected)
	}
	return pass(name, "%d accepted key(s) account for exactly %d cents", accepted, expected)
}
