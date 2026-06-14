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

FROM debian:bookworm

RUN apt-get update && apt-get install -y --no-install-recommends bash curl git gzip tar tzdata unzip ca-certificates && rm -rf /var/lib/apt/lists/*

RUN mkdir /app

COPY --from=builder ./app/CLIProxyAPIPlus /app/CLIProxyAPIPlus
COPY --from=builder ./app/CLIProxyAPIPlus /app/cli-proxy-api
COPY --from=builder /usr/local/go /usr/local/go
COPY --from=builder ./app /app

COPY config.example.yaml /app/config.example.yaml

WORKDIR /app

EXPOSE 8317

ENV TZ=Asia/Shanghai
ENV PATH="/usr/local/go/bin:${PATH}"

RUN cp /usr/share/zoneinfo/${TZ} /etc/localtime && echo "${TZ}" > /etc/timezone

CMD ["./CLIProxyAPIPlus"]
