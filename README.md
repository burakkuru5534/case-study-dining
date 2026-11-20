
# Case Study Dining

Restaurant Order Ingestion and Loyalty Platform.

Signed POS webhooks are verified and stored in an inbox (`202 Accepted`); a
worker turns each event into an order and, when earned, loyalty points, in a
single transaction. Loyalty rule: 1 point per 10 TRY of eligible spend.

Design, guarantees, decisions and known issues: [`docs/architecture.md`](docs/architecture.md).

## Tech Stack

- Go
- PostgreSQL 16
- Docker Compose

## Quick start: webhook → points (Docker)

Order matters: provision the restaurant (seed) **before** the worker runs,
otherwise events for an unknown restaurant fail permanently (see
[Re-queue failed events](#re-queue-failed-events)).

PowerShell:

```powershell
docker compose up -d --build migrate          # postgres + migrations
$env:DATABASE_URL = 'postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
go run ./cmd/seed                              # default program + sample restaurants
docker compose up -d --build                   # api + worker

$env:WEBHOOK_SECRET_REFERENCE = 'local-dev-reference-webhook-secret-0123456789'
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json

$h = @{ Authorization = 'Bearer local-dev-read-token-0123456789abcdef' }
$c = 'http://localhost:8080/v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c'
Invoke-RestMethod -Headers $h "$c/loyalty" | ConvertTo-Json -Depth 5
Invoke-RestMethod -Headers $h "$c/orders"  | ConvertTo-Json -Depth 5
```

Bash:

```bash
docker compose up -d --build migrate
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable' go run ./cmd/seed
docker compose up -d --build

WEBHOOK_SECRET_REFERENCE='local-dev-reference-webhook-secret-0123456789' \
  go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json

TOKEN='local-dev-read-token-0123456789abcdef'
C='http://localhost:8080/v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c'
curl -s -H "Authorization: Bearer $TOKEN" "$C/loyalty"
curl -s -H "Authorization: Bearer $TOKEN" "$C/orders"
```

Expected: balance `22` (225.00 TRY eligible → 22 points) and one order with
`"points_awarded": 22`. Sending the same file again changes nothing.

- API: `127.0.0.1:8080`
- PostgreSQL: `127.0.0.1:55432`

Startup order: `postgres` (healthy) → `migrate` (one-shot, must exit 0) → `api`, `worker`.
Use `--build` after code or migration changes, otherwise the old image is reused.

Stop with `docker compose stop` (graceful) or `docker compose down` (`-v` also deletes the database volume).

## Run locally without Docker for the services

```bash
docker compose up -d postgres
export DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
export WEBHOOK_SECRET_REFERENCE='local-dev-reference-webhook-secret-0123456789'
export API_READ_TOKENS='local-dev-read-token-0123456789abcdef'
go run ./cmd/migrate
go run ./cmd/seed
go run ./cmd/api      # terminal 1
go run ./cmd/worker   # terminal 2
```

PowerShell: set the same variables with `$env:NAME = '...'`.

The app reads real environment variables; it does not load `.env` files.
The `api` container and `go run ./cmd/api` both use host port 8080; run one at a time.

| Variable | Used by | Required | Description |
|---|---|---|---|
| `DATABASE_URL` | all | yes | PostgreSQL connection URL |
| `WEBHOOK_SECRET_REFERENCE` | api | yes | HMAC secret(s) for the reference provider, ≥ 32 bytes each, comma-separated for rotation |
| `API_READ_TOKENS` | api | yes | Bearer token(s) for `/v1`, ≥ 32 bytes each, comma-separated for rotation |
| `APP_ENV` | api, seed | no | `local` \| `test` \| `staging` \| `production`, default `local`; seed refuses `production` |
| `HTTP_PORT` | api | no | default `8080` |
| `WORKER_CONCURRENCY` | worker | no | parallel processing loops, 1–64, default `2` |
| `WORKER_POLL_INTERVAL` | worker | no | wait when the queue is empty, 10ms–1m, default `1s` |
| `WORKER_MAX_ATTEMPTS` | worker | no | attempts before a transient failure becomes permanent, 1–100, default `10` |

## Endpoints

- `GET /health` — liveness; does not check dependencies.
- `GET /ready` — readiness; returns `503` when PostgreSQL is unreachable.
- `POST /webhooks/{provider}` — signed webhook intake; see
  [`docs/reference-webhook-format.md`](docs/reference-webhook-format.md).
- `GET /v1/providers/{provider}/tenants/{tenant}/customers/{customer}/orders` — order history.
- `GET /v1/providers/{provider}/tenants/{tenant}/customers/{customer}/loyalty` — loyalty balances.

## Read API

Requires `Authorization: Bearer <token>` (one of `API_READ_TOKENS`); otherwise `401`.
Customers are addressed by their POS identity (provider, tenant, external customer id),
not by internal ids. All identities linked to the same customer return the same data.

**Order history**, newest first (`completed_at`, then insertion order), keyset-paginated:

```
GET /v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c/orders?limit=20
```

```json
{
  "orders": [{
    "restaurant_id": "store-0042",
    "order_id": "ORD-2026-000123",
    "eligible_amount_minor": 22500,
    "currency": "TRY",
    "completed_at": "2026-11-05T09:30:00Z",
    "loyalty_status": "awarded",
    "points_awarded": 22
  }],
  "next_cursor": null
}
```

- `limit`: 1–100, default 20. Pass `next_cursor` back as `cursor` for the next page;
  `null` means last page. Treat cursors as opaque (they encode a position, including an
  internal row id; do not parse or store them). Orders arriving while paging never cause
  skipped or repeated rows.
- Amounts are integer minor units plus an ISO 4217 code (no floating point).
- Guest orders are not attributed to any customer and never appear.

**Loyalty balances**, one per program the customer has earned in:

```
GET /v1/providers/reference/tenants/chain-istanbul/customers/cust-7f3a9c/loyalty
```

```json
{ "balances": [{ "program_id": 1, "program_name": "Default TRY (1 point per 10 TRY)",
                 "balance": 22, "updated_at": "2026-11-20T10:30:00Z" }] }
```

| Status | Meaning |
|---|---|
| `200` | OK (known customer without orders or points: empty list) |
| `400` | invalid `limit`, `cursor` or path identifier |
| `401` | missing or invalid bearer token |
| `404` | no customer with this identity |
| `503` | temporarily unavailable; details are only logged |

The static token is a placeholder: any valid token can read any tenant. Production
would use per-client authentication (OAuth / mTLS) with tenant-scoped authorization.

## Event processing

Each `pending` event is claimed with `FOR UPDATE SKIP LOCKED` and processed in one
transaction: restaurant and program lookup, customer identity, order, balance and
ledger, then `status = 'processed'`. Any number of worker instances can run.

| Outcome | Event status |
|---|---|
| Processed (including guest, below minimum, unsupported currency) | `processed` |
| Same order already received under another event | `processed`, no second order or award |
| Permanent failure (unknown restaurant, invalid payload, unknown provider) | `failed`, `last_error_kind = 'permanent'` |
| Transient failure (database error, lock timeout) | stays `pending`, retried with backoff 5s × 2ⁿ (max 1h, ±20%) |
| Transient failure on the last attempt | `failed`, `last_error_kind = 'transient'` |

On shutdown (`SIGTERM`) the worker stops claiming new events and finishes the
ones in flight (at most 30s each).

### Re-queue failed events

After fixing the cause (for example provisioning the restaurant):

```sql
UPDATE webhook_events
SET status = 'pending', attempts = 0, next_attempt_at = now(),
    last_error = NULL, last_error_kind = NULL
WHERE status = 'failed';
```

The stored raw payload is parsed again, so adapter fixes apply to replays.

## Send a test webhook

`cmd/sendwebhook` signs a JSON file and posts it (uses `WEBHOOK_SECRET_REFERENCE`):

```bash
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json
# same event again → 202 with "duplicate": true
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json
# new event id and order id
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json -event-id evt-2 -order-id ORD-2
# same order under a new event id → accepted, but no second award
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json -event-id evt-3
# stale signature → 401
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json -timestamp-offset -10m
```

## Migrations

- SQL files in `migrations/`, named `<version>_<name>.sql` (e.g. `0002_create_orders.sql`), embedded into the binary.
- Versions must be contiguous from 1; gaps and duplicates fail at `go test` time.
- Forward-only: never edit an applied migration; fix mistakes with a new one.
- Each migration runs in its own transaction, so statements that cannot run in a transaction (e.g. `CREATE INDEX CONCURRENTLY`) are not supported.
- Concurrent runs are serialized with a PostgreSQL advisory lock.

## Test

```bash
go test ./...
```

Include the PostgreSQL integration tests (requires `docker compose up -d postgres`):

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable' go test ./...
```

Integration tests create and drop their own schemas; they do not touch data in the database.
