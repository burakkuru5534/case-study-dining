
# Case Study Dining

Restaurant Order Ingestion and Loyalty Platform.

## Tech Stack

- Go
- PostgreSQL 16
- Docker Compose

## Run everything in Docker

```bash
docker compose up -d --build
curl http://localhost:8080/ready
```

- API: `127.0.0.1:8080`
- PostgreSQL: `127.0.0.1:55432`

Startup order: `postgres` (healthy) → `migrate` (one-shot, must exit 0) → `api`.
Use `--build` after code or migration changes, otherwise the old image is reused.

Stop with `docker compose stop` (graceful) or `docker compose down` (`-v` also deletes the database volume).

## Run the API locally

Start only PostgreSQL:

```bash
docker compose up -d postgres
```

The app reads real environment variables; it does not load `.env` files.

Bash:

```bash
export DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
export WEBHOOK_SECRET_REFERENCE='local-dev-reference-webhook-secret-0123456789'
go run ./cmd/migrate
go run ./cmd/api
```

PowerShell:

```powershell
$env:DATABASE_URL = 'postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
$env:WEBHOOK_SECRET_REFERENCE = 'local-dev-reference-webhook-secret-0123456789'
go run ./cmd/migrate
go run ./cmd/api
```

| Variable | Required | Description |
|---|---|---|
| `DATABASE_URL` | yes | PostgreSQL connection URL |
| `WEBHOOK_SECRET_REFERENCE` | yes (API) | HMAC secret(s) for the reference provider, ≥ 32 bytes each, comma-separated for rotation |
| `APP_ENV` | no | `local` \| `test` \| `staging` \| `production`, default `local` |
| `HTTP_PORT` | no | default `8080` |

`cmd/migrate` needs only `DATABASE_URL`.

The `api` container and `go run` both use host port 8080; run one at a time.

## Endpoints

- `GET /health` — liveness; does not check dependencies.
- `GET /ready` — readiness; returns `503` when PostgreSQL is unreachable.
- `POST /webhooks/{provider}` — signed webhook intake; see
  [`docs/reference-webhook-format.md`](docs/reference-webhook-format.md).

Accepted events are stored as `pending` in `webhook_events` and processed asynchronously.

## Send a test webhook

`cmd/sendwebhook` signs a JSON file and posts it (uses `WEBHOOK_SECRET_REFERENCE`):

```bash
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json
# same event again → 202 with "duplicate": true
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json
# new event id and order id
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json -event-id evt-2 -order-id ORD-2
# stale signature → 401
go run ./cmd/sendwebhook -file docs/reference-webhook/order_completed.json -timestamp-offset -10m
```

Against the Docker stack, set `WEBHOOK_SECRET_REFERENCE` to the value in `docker-compose.yml` first.

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
