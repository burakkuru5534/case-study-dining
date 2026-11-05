
# Case Study Dining

Restaurant Order Ingestion and Loyalty Platform.

## Tech Stack

- Go
- PostgreSQL 16
- Docker Compose

## Run

Start PostgreSQL (exposed on 127.0.0.1:55432):

```bash
docker compose up -d
```

Run the API. The app reads real environment variables; it does not load `.env` files.

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

## Endpoints

- `GET /health` — liveness; does not check dependencies.
- `GET /ready` — readiness; returns `503` when PostgreSQL is unreachable.

## Test

```bash
go test ./...
```

Include the PostgreSQL integration test (requires `docker compose up -d`):

```bash
TEST_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:55432/dining?sslmode=disable' go test ./...
```
