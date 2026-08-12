# Build context is the transition-operator repo ROOT (not this directory) -
# the module's go.mod has `replace github.com/vitu1234/transition-operator =>
# ../..` so it can import reconcilers/ and api/v1 directly; Docker COPY can
# only reach files inside the build context, so the context has to include
# the parent repo. Build with:
#   docker build -f mcp/failover-agent-mcp/Dockerfile -t failover-agent-mcp:demo .
# (run from the transition-operator repo root)

FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
COPY api ./api
COPY reconcilers ./reconcilers
COPY mcp/failover-agent-mcp ./mcp/failover-agent-mcp

WORKDIR /app/mcp/failover-agent-mcp
RUN go mod download

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/server ./cmd/server

# Final stage
FROM alpine

WORKDIR /app

COPY --from=builder /app/server /app/server

EXPOSE 8080

ENTRYPOINT ["/app/server"]
