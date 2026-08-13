# Unused by local development, which runs the binaries directly — kept for the
# deferred single-VM deployment (see PROJECT_PLAN.md → Deferred).
#
# CGO_ENABLED=0 is load-bearing beyond static linking: it is why the queue uses
# modernc.org/sqlite rather than mattn/go-sqlite3. A cgo SQLite binding will not
# link here.
#
# The API and the workers share one SQLite queue file, so deploying them as
# separate containers requires a shared volume on the same host. SQLite's
# locking is not safe over a network filesystem — if they ever need to run on
# different machines, the queue has to change, not the mount.
FROM golang:1.24-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /api ./cmd/api

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /api /api
EXPOSE 8080
CMD ["/api"]
