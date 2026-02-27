# ── API ──────────────────────────────────────────────────────────────────────

FROM golang:1.26-alpine AS build-api
WORKDIR /usr/src/api
COPY api/go.mod api/go.sum ./
RUN go mod download
COPY api/ .
RUN CGO_ENABLED=0 go build -o /usr/local/bin/api .

FROM scratch AS api
COPY --from=build-api /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build-api /usr/local/bin/api /usr/local/bin/api
CMD ["/usr/local/bin/api"]

# ── Optimizer ─────────────────────────────────────────────────────────────────

FROM golang:1.26-alpine AS build-optimizer
WORKDIR /usr/src/optimizer
COPY optimizer/go.mod optimizer/go.sum ./
RUN go mod download
COPY optimizer/ .
RUN CGO_ENABLED=0 go build -o /usr/local/bin/optimizer .

FROM scratch AS optimizer
COPY --from=build-optimizer /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build-optimizer /usr/local/bin/optimizer /optimizer
CMD ["/optimizer"]
