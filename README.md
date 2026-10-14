# ledger-loadtest

A concurrent load and correctness tester for the
[installment ledger](https://github.com/vamsi513/installment-ledger) API.

Most load tools answer "how fast is it". This one answers "is it still correct
while it is busy". It drives real concurrent traffic at a running service and
then reads the state back and checks the things the service promises: that a
retried payment is applied once, that concurrent payments cannot overpay a
loan, that a schedule adds up, and that the double entry ledger balances. It
reports latency percentiles alongside a PASS or FAIL for every invariant, and
exits non zero if any of them broke.

It treats the service as a black box. It only speaks HTTP to it.

## How it works

The run has two phases, because the two properties under test need opposite
traffic shapes.

**Retry phase.** A small set of idempotency keys is sent over and over by every
worker, so the same key is in flight several times at once. The amounts are
sized so that every key could be accepted without closing the loan, which lets
the phase keep replaying the same keys for its whole duration. A correct
service applies each key exactly once no matter how many times it arrives.

**Overpay phase.** A stream of distinct keys, each asking for a quarter of the
loan, is fired at a single loan by every worker at once. Four of them fit. The
rest must be refused. This is where a service that reads a stale balance before
deciding would let the loan go past its total.

Afterwards the tester reads the loan and its ledger back and runs the checks.

## Running it

You need Go 1.26 and a running installment ledger service.

Start the service from its own clone, following its README:

```bash
git clone https://github.com/vamsi513/installment-ledger
cd installment-ledger
docker compose up -d
python3 -m venv .venv
.venv/bin/pip install -r requirements.txt
.venv/bin/alembic upgrade head
.venv/bin/uvicorn app.main:app --host 127.0.0.1 --port 8000
```

Then, from this repository:

```bash
go run ./cmd/ledger-loadtest -base-url http://127.0.0.1:8000 -workers 32 -duration 30s
```

### Flags

| Flag | Default | What it does |
| --- | --- | --- |
| `-base-url` | `http://127.0.0.1:8000` | Base URL of the service |
| `-workers` | `16` | Concurrent workers |
| `-duration` | `10s` | Total load time, split evenly between the two phases |
| `-timeout` | `10s` | Per request timeout |
| `-principal-cents` | `120000` | Loan principal, in cents |
| `-apr-bps` | `1200` | Annual rate in basis points, so 1200 is 12 percent |
| `-installments` | `4` | Number of installments |
| `-cadence` | `biweekly` | Installment cadence |
| `-start-date` | `2026-01-15` | Loan start date |
| `-retry-keys` | `16` | Distinct keys reused during the retry phase |
| `-run-id` | timestamp | Prefix for every idempotency key, so runs cannot collide |

### Exit codes

| Code | Meaning |
| --- | --- |
| `0` | Every invariant held |
| `1` | The run completed and at least one invariant broke |
| `2` | The run could not be carried out, for example the service was unreachable |

## What the invariants mean

Each check runs against both loans, so there are twelve results in a report.

**installments sum to total.** The amounts in the schedule add up to
`total_cents` exactly, and there are as many of them as `installment_count`
says. The service splits a total by giving the leftover cents to the earliest
installments, so this has to be exact rather than close.

**no negative balance.** Neither the loan nor any installment has a negative
outstanding or paid amount, and no installment is paid beyond its own amount.

**no overpayment.** `paid_cents` never exceeds `total_cents`, and
`paid_cents + outstanding_cents` equals `total_cents`. The second half catches a
service whose reported numbers disagree with each other even when neither is
out of range on its own.

**ledger balances.** Every ledger transaction has equal debits and credits and
reports `net_cents` of zero, and so does the ledger as a whole. This is the
double entry invariant.

**each idempotency key applied at most once.** No key produced more than one
distinct payment identity. Retrying a key must return the original payment.

**paid total matches accepted requests.** The loan's `paid_cents` equals the
sum of the amounts for the keys that were accepted. This is the check that
actually catches a double application, and it is the reason the tester tracks
keys rather than just counting responses: a service can apply a replayed
payment twice and still return one payment body per key, so the response alone
looks correct and only the money moved twice. The project's test suite includes
a fake service that does exactly this, and that fake passes every other check
and is caught only by this one.

## Example output from a real run

This is a real run against the service at commit `b81f5e4`, not an illustration.

```
target            http://127.0.0.1:8000
workers           32
duration          30s requested, 30.001s elapsed
loan              principal 120000 cents, apr 1200 bps, 4 installments, biweekly

requests sent     28338
throughput        944.6 req/s
succeeded (2xx)   11897
transport errors  0

by status code
  201             11897
  422             16441

latency
  min             3.6 ms
  p50             28.9 ms
  p95             62.4 ms
  p99             87.4 ms
  max             207.0 ms

retry phase       loan 6c79c7eb-fa0b-4dcc-b421-e70b272b7b57, 16 distinct keys, paid 61104 of 122209 cents
overpay phase     loan 03f02646-6c10-4888-8d12-c701b8b38e52, 16445 distinct keys, paid 122208 of 122209 cents, status open

invariants
  PASS retry loan: installments sum to total                4 installments sum to 122209 cents
  PASS retry loan: no negative balance                      outstanding 61105 cents, paid 61104 cents
  PASS retry loan: no overpayment                           paid 61104 of 122209 cents
  PASS retry loan: ledger balances                          17 transactions, 183313 cents on each side
  PASS retry loan: each idempotency key applied at most once 16 key(s), 16 of them retried, none applied twice
  PASS retry loan: paid total matches accepted requests     16 accepted key(s) account for exactly 61104 cents
  PASS overpay loan: installments sum to total              4 installments sum to 122209 cents
  PASS overpay loan: no negative balance                    outstanding 1 cents, paid 122208 cents
  PASS overpay loan: no overpayment                         paid 122208 of 122209 cents
  PASS overpay loan: ledger balances                        5 transactions, 244417 cents on each side
  PASS overpay loan: each idempotency key applied at most once 16445 key(s), 0 of them retried, none applied twice
  PASS overpay loan: paid total matches accepted requests   4 accepted key(s) account for exactly 122208 cents

RESULT PASS
```

The 16441 responses with status 422 are not failures. They are the overpay
phase being refused, which is the behaviour under test. In the retry phase all
16 keys were retried and none was applied twice, and in the overpay phase
exactly four payments were accepted out of 16445 attempts.

### What was measured

Those numbers come from one run on one machine, with the load generator and the
service and the database all competing for the same cores:

* Apple `Mac17,2`, 10 cores, 16 GB RAM, macOS 27.0.1
* Go 1.26.9, load generator built for the host and run natively
* the service at commit `b81f5e4`, Python 3.13.15, uvicorn 0.54.0, single worker
* Postgres 18.6 in Docker 29.7.2 on the same machine

**The latency figures describe that setup and nothing else.** They are a floor
measured over loopback against a single uvicorn worker, so they say more about
the test rig than about how the service would behave deployed. Treat throughput
the same way. The invariant results are the part of this report that generalises.

## Tests

```bash
go test -race ./...
```

The suite covers the percentile maths and every invariant checker with table
driven cases, and drives the whole tool end to end against an `httptest` server
in three modes: one that behaves correctly, one that applies a replayed
idempotency key a second time while returning the original response, and one
that allows payments past the outstanding balance. The last two exist to prove
the checks can actually fail, because a correctness checker that has never been
seen to report FAIL has not been tested.

## Limitations

* The service has no authentication, so neither does this tool. It sends no
  credentials and has nowhere to put them.
* Latency is measured client side and includes the client's own time in the Go
  HTTP stack. There is no server side timing and no tracing.
* The two phases run in sequence, not at the same time, so this does not test
  contention between the two traffic shapes at once.
* Only the payment endpoint is driven under load. Loan creation happens twice
  per run, at the start, so the tester says nothing about `POST /loans` under
  concurrency.
* There is no warmup and no coordinated omission correction, so the first
  requests of a run include connection setup and the percentiles include it.
* The tester only knows what the API tells it. It never reads the database, so
  it cannot catch a bug that the API's own responses hide consistently.
* Nothing is cleaned up. Every run leaves two loans behind in the database.
* `-apr-bps` and the principal are not swept. One loan shape is tested per run.

## Licence

MIT. See [LICENSE](LICENSE).
