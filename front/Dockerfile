# ── Stage 1 : Dépendances ────────────────────────────────────
FROM node:20-alpine AS deps

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --ignore-scripts

# ── Stage 2 : Build ──────────────────────────────────────────
FROM node:20-alpine AS builder

WORKDIR /app

COPY --from=deps /app/node_modules ./node_modules
COPY . .

ARG VITE_API_URL
ENV VITE_API_URL=${VITE_API_URL}

RUN npm run build && npm install -g serve

# ── Stage 3 : Distroless Node ────────────────────────────────
FROM gcr.io/distroless/nodejs20-debian12:nonroot

WORKDIR /app

COPY --from=builder /usr/local/lib/node_modules/serve /app/serve
COPY --from=builder /app/dist ./dist

EXPOSE 3000

USER nonroot:nonroot

CMD ["/nodejs/bin/node", "/app/serve/build/main.js", "-s", "dist", "-l", "3000"]