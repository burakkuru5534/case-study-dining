# Architecture

Restaurant Order Ingestion and Loyalty Platform: receives completed-order webhooks
from POS providers, stores orders, awards loyalty points (1 point per 10 TRY of
eligible spend) and exposes order history and balances.

This document explains how the system works, which guarantees it gives and why,
the decisions behind it, and what is deliberately left out.

---

## 1. Overview

```mermaid
flowchart LR
    POS["POS provider"] -- "signed webhook" --> API
    Client["Client app"] -- "bearer token" --> API

    subgraph Service
        API["api<br/>/webhooks, /v1, /health, /ready"]
        Worker["worker<br/>(N instances)"]
    end

    subgraph DB["PostgreSQL 16"]
        Inbox[("webhook_events<br/>(inbox)")]
        Data[("orders<br/>loyalty_accounts<br/>loyalty_transactions")]
    end

    API -- "INSERT pending<br/>ON CONFLICT DO NOTHING" --> Inbox
    Worker -- "FOR UPDATE SKIP LOCKED" --> Inbox
    Worker -- "one transaction" --> Data
    API -- "read" --> Data
    Migrate["migrate<br/>(one-shot)"] -. "schema" .-> DB
```

The two halves are decoupled by a durable inbox table:

1. **Intake** (`POST /webhooks/{provider}`) verifies the signature, parses and validates
   the payload, stores the raw event as `pending` and answers `202 Accepted`. It never
   touches orders or points.
2. **Processing** (worker) claims pending events one at a time and turns each into an
   order and, when earned, ledger and balance updates, all in **one transaction**.

The read API (`GET /v1/...`) serves order history and balances.

| Guarantee | Mechanism |
|---|---|
| An accepted event is never lost | Committed to `webhook_events` before `202` |
| An event is processed at most once | Row lock + `status = 'processed'` in the same transaction |
| An order is stored once, points awarded once | Unique constraints, not application checks |
| Order, ledger and balance are always consistent | Single transaction, append-only ledger |
| Safe with any number of API and worker instances | `SKIP LOCKED`, row locks, advisory locks |
| Money is exact | Integer minor units end to end |

---

## 2. Components and code structure

### Processes

All service binaries ship in one distroless, non-root image; Compose selects the
entrypoint.

| Process | Role | Instances |
|---|---|---|
| `api` | Webhook intake, read API, health checks | any |
| `worker` | Processes the inbox | any (`--scale worker=N`) |
| `migrate` | Applies migrations, exits; `api` and `worker` start only after it exits 0 | one-shot |
| `seed`, `sendwebhook` | Development tools (not in the image) | — |

### Modular monolith

```
cmd/            api, worker, migrate, seed, sendwebhook
internal/
  ingestion/    intake handler, inbox, processor, worker loop, adapter contract
    reference/  adapter + signature for the internal reference format
  order/        domain model (pure)          order/pgstore    orders table
  loyalty/      earn rule (pure)             loyalty/pgstore  customers, accounts, ledger
  customerapi/  read API (HTTP)
  platform/     config, postgres pool, migrate runner, httpauth, testdb
migrations/     SQL, embedded into the binaries
```

Dependency rules:

- `order` and `loyalty` are **pure**: standard library only, no HTTP, no SQL, no
  provider code. `loyalty` does not import `order`. They are unit-tested without a database.
- SQL lives in the owning module's `pgstore` package. Write functions take the
  caller's `pgx.Tx`, so the processor composes order and loyalty writes in one transaction.
- Provider specifics live behind the `ingestion.Adapter` interface. Adding a POS provider
  means adding an adapter and a signature verifier; the core flow does not change.

---

## 3. Request lifecycle

### Intake

```mermaid
sequenceDiagram
    participant POS
    participant H as Intake handler
    participant V as Verifier (HMAC)
    participant A as Adapter
    participant DB as webhook_events

    POS->>H: POST /webhooks/reference (≤ 1 MiB)
    H->>V: verify(headers, raw body)
    alt bad signature / stale timestamp
        H-->>POS: 401 (no details)
    end
    H->>A: Parse(raw body)
    alt unsupported event type
        H-->>POS: 200 ignored (not stored)
    else invalid payload
        H-->>POS: 422 + field names
    end
    H->>DB: INSERT … ON CONFLICT DO NOTHING
    alt database error
        H-->>POS: 503 (provider retries)
    end
    H-->>POS: 202 {duplicate: true|false}
```

- The signature is verified on the **raw bytes before parsing**: an unauthenticated
  request never reaches the parser.
- The full payload is validated at intake, so the inbox only holds events that can be
  processed. The **raw body** is stored; the worker parses it again, so adapter fixes
  apply when events are replayed.
- A duplicate delivery is a no-op that still returns `202`.

### Processing

```mermaid
sequenceDiagram
    participant W as Worker loop
    participant P as Processor
    participant DB as PostgreSQL

    W->>P: ProcessNext (30s budget)
    P->>DB: BEGIN, SET LOCAL lock_timeout = 5s
    P->>DB: SELECT … WHERE status='pending' AND due<br/>FOR UPDATE SKIP LOCKED LIMIT 1
    P->>DB: SAVEPOINT
    P->>DB: restaurant + program … FOR SHARE
    P->>DB: resolve / create customer identity
    Note over P: loyalty.Evaluate (pure)
    P->>DB: INSERT order ON CONFLICT DO NOTHING
    alt awarded and order is new
        P->>DB: upsert balance (+points), append ledger row
    end
    alt success
        P->>DB: RELEASE, status = processed, COMMIT
    else failure
        P->>DB: ROLLBACK TO SAVEPOINT, record failure, COMMIT
    end
```

The event row stays locked from claim to commit. A failure rolls back the business
writes to the savepoint while the lock is still held, so the failure is recorded
without another worker grabbing the event. If the worker dies, the whole transaction
rolls back and the event is simply `pending` again; there is no "processing" state to recover.

### Read API

`GET /v1/providers/{provider}/tenants/{tenant}/customers/{customer}/orders|loyalty`,
behind a bearer token. Customers are addressed by their POS identity. Order history uses
**keyset pagination** on `(completed_at DESC, id DESC)`, which matches an index exactly,
does not slow down on deep pages and never skips or repeats rows when new orders arrive.

---

## 4. Data model

```mermaid
erDiagram
    loyalty_programs ||--o{ restaurants : "earn rule"
    restaurants ||--o{ orders : ""
    customers ||--o{ customer_identities : "POS identities"
    customers ||--o{ orders : "NULL = guest"
    webhook_events ||--o{ orders : "source_event_id"
    customers ||--o{ loyalty_accounts : ""
    loyalty_programs ||--o{ loyalty_accounts : ""
    loyalty_accounts ||--o{ loyalty_transactions : ""
    orders ||--o| loyalty_transactions : "earn"
```

| Constraint | What it guarantees |
|---|---|
| `webhook_events UNIQUE (provider, tenant_ref, external_event_id)` | Each delivery is stored once |
| `orders UNIQUE (restaurant_id, external_order_id)` | The same order under a new event id is not stored twice |
| `loyalty_transactions` unique `(order_id) WHERE type = 'earn'` | Points can never be awarded twice for an order |
| Ledger FK `(order_id, program, customer)` → `orders` | A ledger row matches its order's program and customer |
| Trigger on `orders` (`FOR SHARE` on the restaurant) | The order's program is the restaurant's program at insert time |
| `loyalty_accounts.balance >= 0` | No negative balance |
| Ledger triggers on `UPDATE`, `DELETE`, `TRUNCATE` | The ledger is append-only |
| `customer_id IS NULL` ⇔ `loyalty_status = 'no_customer'` | Guest orders are consistent |
| Event status `CHECK`s | `processed` has `processed_at`; a permanent error is always `failed` |

Notes:

- External identifiers are scoped by `(provider, tenant_ref)`: two tenants of one
  provider may reuse ids. Providers without tenants use a constant `"default"`.
- **Customers and identities are separate**: several POS identities can be linked to
  one customer later without a schema change.
- **Loyalty programs** hold the earn rule. "Shared across restaurants" vs "per
  restaurant" is configuration (which restaurants map to which program), not code.
- Orders store a **snapshot** of their program, and ledger rows store the rule used
  (`earn_unit_minor`, `points_per_unit`). Changing a program never rewrites history.
- Statuses are `text` + `CHECK`, not enum types: adding a value is a simple migration.

---

## 5. Guarantees in detail

### Idempotency: three layers

1. **Same delivery** → event unique constraint; intake answers `202 duplicate: true`.
2. **Same order, different event id** → order unique constraint; the event is
   processed, no second order or award.
3. **Last line** → one-earn-per-order unique index on the ledger.

All three are enforced by PostgreSQL, so they hold across instances, retries and
races without application-level locking.

### Transactional consistency

Order insert, balance update, ledger append and event status change commit together.
The balance is a read-optimized projection; the **ledger is the source of truth**, and
`balance = sum(ledger points)` holds after every commit (verified by concurrency tests).

### Concurrency and multiple instances

| Concern | Mechanism |
|---|---|
| Two workers, one event | `FOR UPDATE SKIP LOCKED`: the second skips it |
| Two events for one new customer | Identity unique index; the loser rolls back and retries (no orphan customer rows) |
| Concurrent awards to one account | Upsert takes the account row lock: no lost update |
| Program change during processing | Restaurant row read `FOR SHARE`: the change waits for the commit |
| Concurrent migrations | Session advisory lock on a dedicated connection |
| Concurrent seed runs | Transaction advisory lock |

Locks are always taken in the same order (event → restaurant → identity → order → account),
so processing transactions cannot deadlock with each other.

### Crash safety and shutdown

- A crash at any point rolls back the processing transaction; the event is retried.
- `SIGTERM`: the API drains requests (10s); the worker stops claiming and finishes
  in-flight events (at most 30s each). Compose grace periods (15s / 45s) exceed these.

### Money

Amounts are `bigint` minor units from JSON to database. The adapter accepts only JSON
integers (`1.5`, `"100"` and `1e3` are rejected). Points are
`floor(eligible / earn_unit) × points_per_unit` with an explicit overflow check.
The eligible amount is subtotal minus discount, excluding tax and tip.

### Security

- **Webhooks:** HMAC-SHA256 over `timestamp.raw_body`, constant-time comparison, ±5 min
  replay window (replays inside it are absorbed by deduplication), secret rotation,
  1 MiB body limit.
- **Read API:** static bearer tokens with rotation, constant-time comparison.
- **No sensitive data in logs or responses:** errors name fields, never values;
  database errors are logged, not returned. Tests assert this for customer ids,
  amounts, secrets, signatures and tokens.
- Database and API ports bound to loopback in Compose; non-root distroless image.

---

## 6. Failure handling

| Failure | Example | Result |
|---|---|---|
| Permanent | Unknown restaurant, payload invalid for the current adapter, unknown provider, invalid rule | `failed` / `permanent`, never retried automatically |
| Transient | Database error, lock timeout, identity race | stays `pending`, `attempts + 1`, retry after backoff |
| Transient, last attempt | — | `failed` / `transient` |

- Unrecognized errors are treated as transient: the safe default, bounded by `WORKER_MAX_ATTEMPTS`.
- Backoff: 5s × 2ⁿ, capped at 1h, ±20% jitter. With 10 attempts the waits add up to about 43 minutes.
- **Database outage:** the worker logs and backs off (doubling up to 30s), does not crash,
  and resumes when the database returns. Intake answers `503`, so providers retry.
- **Re-queue** after fixing the cause (e.g. provisioning a restaurant): set the event back
  to `pending` (SQL in the README). The raw payload is parsed again.

---

## 7. Decisions and trade-offs

| Decision | Alternative | Why |
|---|---|---|
| Inbox + async worker | Process synchronously in the webhook request | Durable acceptance, retries and status tracking; intake stays fast and simple. Cost: eventual consistency (sub-second) and a second process. |
| Row lock held in the processing transaction | `processing` status + lease/visibility timeout | No stuck-state recovery; a crash simply rolls back. Cost: one connection per in-flight event. |
| Ledger + stored balance | `SUM()` over the ledger | O(1) reads and a row to lock for future redemptions; the ledger stays authoritative. |
| Constraints enforce idempotency | Application checks | Correct under any concurrency and across instances. |
| Keyset pagination | `OFFSET` | Stable while data changes, constant cost per page. |
| Own migration runner (~190 lines) | goose / golang-migrate | No dependency, fully explainable; advisory lock and one transaction per file. Cost: no checksums. |
| Separate `migrate` step | Migrate on API start | The API does not need DDL rights; a broken migration does not crash-loop the API. |
| Polling (1s) | `LISTEN/NOTIFY` | Simpler and robust; latency is acceptable for loyalty points. |
| Tenant-scoped external ids | Provider-wide ids | No assumption that ids are unique across a provider's customers. |
| Reference adapter with our own format | Invent a POS payload | No real contract available; the adapter boundary keeps the core independent. |
| Standard library HTTP, pgx only | Frameworks / ORM | Small surface, explicit SQL, easy to reason about in review. |

---

## 8. Testing strategy

About 390 tests (unit and PostgreSQL integration), all run with `-race`.

| Layer | Examples |
|---|---|
| Pure unit | Earn rule boundaries (999 / 1000 / 1001 kuruş), payload validation (44 malformed-payload cases), signature, backoff, config |
| HTTP | Intake status codes, signature checked before parsing, log content, routing and auth |
| Integration (real PostgreSQL) | Every schema constraint, processor outcomes, migration runner, read API pagination |
| Concurrency | 6 processors × 60 events with duplicates and identity races; racing earns; concurrent migrations and seeds; lock behavior of `0003` |
| End to end | Real binaries: webhook → worker → read API, database outage, graceful shutdown |

Integration tests create an isolated schema per test, so they never touch real data and
can run in parallel.

**Tests are checked against deliberately broken code.** For each key guarantee, the
protecting code was removed in a throwaway copy and the test had to fail:

| Guarantee | Broken by | Caught by |
|---|---|---|
| One award per order | Dropping the earn unique index | Racing earns test |
| One order per restaurant order id | Dropping the order unique constraint | Concurrent workers test |
| Serialized migrations | Removing the advisory lock | Concurrent migration test |
| Signature before parsing | Swapping the order of the two steps | Intake test (`422` instead of `401`) |
| No orphan customers | Reusing the winner's identity on a race | Concurrent workers test |
| Stable pagination | Removing the `id` tiebreak / using `OFFSET` | Pagination tests |
| Program snapshot lock (`0003`) | Removing `FOR SHARE` | Lock tests |

Run: `go test ./...`; with `TEST_DATABASE_URL` set, integration tests run too (see README).

---

## 9. Known issues and limitations

### Known issues (found in review, intentionally not yet fixed)

| # | Severity | Issue | Fix when needed |
|---|---|---|---|
| B1 | **Medium** | If processing an event exceeds the worker's 30s budget, the attempt is not counted: the event is retried indefinitely and never reaches `failed`. Needs a pathologically slow database; **fix before production traffic**. | Treat `DeadlineExceeded` as a transient failure and record it in a fresh transaction (guarded by `status` and `attempts`); keep `Canceled` untouched. |
| B2 | Low | The pagination cursor is opaque but not secret: it encodes the internal order id. The package comment in `internal/customerapi/handler.go` still says internal ids are never exposed. | Treat as an implementation detail and correct the comment; sign or encrypt the cursor if internal ids must stay hidden. |
| B3 | Low | A re-delivery with the same event id but different content is acknowledged as a duplicate without any trace. | Compare with the stored payload and log a warning. |
| B4 | Info | A processed event keeps `last_error` from earlier transient failures. | Intended as history; documented here. |
| B5 | Low | `migrations/schema_test.go` has its own schema harness instead of `testdb`. | Switch to `testdb.New`. |
| B6 | Low | A far-future `completed_at` is accepted and sorts first in history. | Decide on an accepted clock-skew window with the real POS contract. |
| B7 | Low | The same value may be configured as webhook secret and read token. | Reject overlap in config. |

### Deliberate limitations

- **Authorization:** any valid read token can read any tenant. Production needs per-client
  authentication (OAuth / mTLS) with tenant-scoped authorization, and per-tenant webhook secrets.
- **Rate limiting:** none; unauthenticated requests can still make the server read up to 1 MiB.
- **Data retention:** raw payloads (which may contain personal data) and processed events
  are kept indefinitely.
- **Refunds and cancellations:** out of scope. The ledger's `type` column and the partial
  unique index leave room for `reversal` rows.
- **Observability:** structured logs only; no metrics, tracing or queue-depth alerting.
- **Readiness during shutdown:** `/ready` does not turn `503` before draining (matters behind
  a load balancer).
- **Migrations:** no checksums for edited files; statements that cannot run in a transaction
  are not supported.
- **Raw payload storage:** stored as `jsonb`, so the original bytes (and the signature)
  cannot be re-verified later.

### Next steps, in order

1. Fix B1.
2. Metrics: queue depth, oldest pending age, failures by kind, processing latency.
3. Tenant-scoped authorization and per-tenant secrets.
4. Retention job for processed events.
5. First real POS adapter once its contract is known.

---

## 10. Five-minute demo

The numbers below assume an empty database. To reset local data first
(**this deletes all local data**): `docker compose down -v`.

Processing is asynchronous, so each check waits briefly for the worker.

### PowerShell (Windows PowerShell 5.1 and PowerShell 7)

```powershell
docker compose up -d --build migrate
$env:DATABASE_URL = 'postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
go run ./cmd/seed
docker compose up -d --build

$env:WEBHOOK_SECRET_REFERENCE = 'local-dev-reference-webhook-secret-0123456789'
$Auth = @{ Authorization = 'Bearer local-dev-read-token-0123456789abcdef' }
$C = 'http://localhost:8080/v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c'
$F = 'docs/reference-webhook/order_completed.json'

# Prints the JSON body, or "HTTP <code>" for 4xx/5xx (Invoke-RestMethod throws on those).
# Note: in Windows PowerShell 5.1, "curl" is an alias of Invoke-WebRequest, not curl.
function Show-Response($Uri, $Headers = @{}) {
    try { Invoke-RestMethod -Uri $Uri -Headers $Headers | ConvertTo-Json -Depth 5 }
    catch { 'HTTP {0}' -f [int]$_.Exception.Response.StatusCode }
}

# 1. Accept an order -> 22 points
go run ./cmd/sendwebhook -file $F
Start-Sleep -Seconds 2
Show-Response "$C/loyalty" $Auth

# 2. Idempotency: same delivery, then same order under a new event id -> still 22
go run ./cmd/sendwebhook -file $F
go run ./cmd/sendwebhook -file $F -event-id evt-redelivered
Start-Sleep -Seconds 2
Show-Response "$C/loyalty" $Auth

# 3. A new order -> 44, two orders in the history
go run ./cmd/sendwebhook -file $F -event-id evt-2 -order-id ORD-2
Start-Sleep -Seconds 2
Show-Response "$C/loyalty" $Auth
Show-Response "$C/orders" $Auth

# 4. Security: stale signature -> 401 (go run also prints "exit status 2"), no token -> 401
go run ./cmd/sendwebhook -file $F -timestamp-offset '-10m'
Show-Response "$C/loyalty"

# 5. Resilience: stop the database, the worker backs off without crashing;
#    start it again (--wait blocks until healthy) and processing resumes -> 66
docker compose stop postgres
Start-Sleep -Seconds 3
docker compose logs --tail 5 worker
docker compose up -d --wait postgres
go run ./cmd/sendwebhook -file $F -event-id evt-after-outage -order-id ORD-3
Start-Sleep -Seconds 15
Show-Response "$C/loyalty" $Auth
```

### Bash

```bash
docker compose up -d --build migrate
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable' go run ./cmd/seed
docker compose up -d --build

export WEBHOOK_SECRET_REFERENCE='local-dev-reference-webhook-secret-0123456789'
TOKEN='local-dev-read-token-0123456789abcdef'
C='http://localhost:8080/v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c'
F=docs/reference-webhook/order_completed.json

# 1. Accept an order -> 22 points
go run ./cmd/sendwebhook -file $F; sleep 2
curl -s -H "Authorization: Bearer $TOKEN" "$C/loyalty"

# 2. Idempotency -> still 22
go run ./cmd/sendwebhook -file $F
go run ./cmd/sendwebhook -file $F -event-id evt-redelivered; sleep 2
curl -s -H "Authorization: Bearer $TOKEN" "$C/loyalty"

# 3. A new order -> 44
go run ./cmd/sendwebhook -file $F -event-id evt-2 -order-id ORD-2; sleep 2
curl -s -H "Authorization: Bearer $TOKEN" "$C/orders"

# 4. Security: stale signature -> 401, no token -> 401
go run ./cmd/sendwebhook -file $F -timestamp-offset -10m
curl -s -w ' HTTP %{http_code}\n' "$C/loyalty"

# 5. Resilience -> 66
docker compose stop postgres; sleep 3
docker compose logs --tail 5 worker
docker compose up -d --wait postgres
go run ./cmd/sendwebhook -file $F -event-id evt-after-outage -order-id ORD-3; sleep 15
curl -s -H "Authorization: Bearer $TOKEN" "$C/loyalty"
```
