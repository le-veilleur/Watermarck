# ── Stage 1 : Build ───────────────────────────────────────────────────────────
# golang:bookworm = golang + debian bookworm + gcc/build-essential
# Nécessaire pour CGO (chai2010/webp embarque libwebp en C, compilé statiquement dans le binaire)
FROM golang:bookworm AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=1 (défaut sur Linux avec gcc) pour compiler le codec WebP en C embarqué
RUN CGO_ENABLED=1 go build -o optimizer main.go

# ── Stage 2 : Runtime ─────────────────────────────────────────────────────────
# debian:bookworm-slim pour rester compatible glibc avec le binaire compilé ci-dessus
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y fonts-liberation && rm -rf /var/lib/apt/lists/*

WORKDIR /app
COPY --from=builder /app/optimizer .

ENV FONT_PATH=/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf
EXPOSE 3001
CMD ["./optimizer"]
