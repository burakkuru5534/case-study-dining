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
# Static binary: no libc dependency, runs on distroless/static.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

# ---- runtime stage ----
# Distroless static: CA certificates and a nonroot user, no shell or package manager.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
USER nonroot:nonroot
EXPOSE 8080
# Exec form: the Go binary is PID 1 and receives SIGTERM directly,
# which is what triggers graceful shutdown.
ENTRYPOINT ["/api"]
