FROM golang:1.26-bookworm AS builder

WORKDIR /app

RUN apt-get update && apt-get install -y --no-install-recommends build-essential git && rm -rf /var/lib/apt/lists/*

COPY go.mod go.sum ./

RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -ldflags="-s -w -X 'main.Version=${VERSION}-plus' -X 'main.Commit=${COMMIT}' -X 'main.BuildDate=${BUILD_DATE}'" -o ./CLIProxyAPIPlus ./cmd/server/

FROM node:22-bookworm-slim AS cursor-bridge-builder

WORKDIR /bridge

COPY sidecars/cursor-bridge/package.json sidecars/cursor-bridge/package-lock.json ./
COPY sidecars/cursor-bridge/scripts ./scripts

# npm ci runs the fail-closed structural @cursor/sdk patcher and emits the
# descriptor the bridge validates before readiness. No dependency install or
# vendor mutation occurs at container startup.
RUN npm ci

COPY sidecars/cursor-bridge/*.mjs ./

RUN npm test && npm run selftest

FROM node:22-bookworm-slim

ARG COPILOT_ELECTRON_VERSION=40.4.0

# Railway currently uses the existing Electron-backed Copilot transport. Bake
# both Electron and its runtime libraries into the same immutable image as the
# Cursor bridge so process startup never downloads or mutates dependencies.
RUN apt-get update && apt-get install -y --no-install-recommends \
    bash ca-certificates curl git gzip tar tzdata unzip \
    libasound2 libatk-bridge2.0-0 libatk1.0-0 libcups2 libdrm2 libgbm1 \
    libgtk-3-0 libnss3 libpangocairo-1.0-0 libx11-xcb1 libxcomposite1 \
    libxdamage1 libxext6 libxfixes3 libxi6 libxkbcommon0 libxrandr2 \
    libxrender1 libxshmfence1 libxss1 libxtst6 \
    && npm install --global --ignore-scripts=false "electron@${COPILOT_ELECTRON_VERSION}" \
    && npm cache clean --force \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir /app

COPY --from=builder ./app/CLIProxyAPIPlus /app/CLIProxyAPIPlus
COPY --from=builder ./app/CLIProxyAPIPlus /app/cli-proxy-api
COPY --from=builder ./app /app
COPY --from=cursor-bridge-builder /bridge /app/sidecars/cursor-bridge

COPY config.example.yaml /app/config.example.yaml

WORKDIR /app

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV NODE_ENV=production
ENV BAKED_SERVER_REQUIRED=1

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["bash", "scripts/railway_start.sh"]
