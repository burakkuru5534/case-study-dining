
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
go run ./cmd/api
```

PowerShell:

```powershell
$env:DATABASE_URL = 'postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable'
go run ./cmd/api
```

Optional: `APP_ENV` (`local` | `test` | `staging` | `production`, default `local`) and `HTTP_PORT` (default `8080`).

The `api` container and `go run` both use host port 8080; run one at a time.

## Endpoints

- `GET /health` — liveness; does not check dependencies.
- `GET /ready` — readiness; returns `503` when PostgreSQL is unreachable.

## Test

```bash
go test ./...
```

Include the PostgreSQL integration test (requires `docker compose up -d postgres`):

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable' go test ./...
```
