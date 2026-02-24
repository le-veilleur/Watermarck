FROM golang:alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go build -o optimizer main.go

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y fonts-liberation && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY --from=builder /app/optimizer .
ENV FONT_PATH=/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf
EXPOSE 3001
CMD ["./optimizer"]