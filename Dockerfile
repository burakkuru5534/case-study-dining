# syntax=docker/dockerfile:1

# ---- build stage ----
# Tag must satisfy the "go" directive in go.mod, otherwise the build tries to
# download a newer toolchain or fails.
FROM golang:1.27-alpine AS build
WORKDIR /src

# Dependencies first: this layer is only rebuilt when go.mod/go.sum change.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Static binaries: no libc dependency, run on distroless/static.
# One image ships both commands; compose selects one via entrypoint.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate

# ---- runtime stage ----
# Distroless static: CA certificates and a nonroot user, no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
COPY --from=build /out/migrate /migrate
USER nonroot:nonroot
EXPOSE 8080
# Exec form: the Go binary is PID 1 and receives SIGTERM directly,
# which is what triggers graceful shutdown.
ENTRYPOINT ["/api"]
