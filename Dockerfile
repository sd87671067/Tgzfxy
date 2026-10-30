FROM golang:1.24.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN GOMAXPROCS=1 GOMEMLIMIT=700MiB go build -p 1 -gcflags=github.com/gotd/td/tg=-l -ldflags="-s -w" -o /sbbot ./cmd/sbbot
FROM debian:13-slim
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates util-linux && rm -rf /var/lib/apt/lists/*
COPY --from=build /sbbot /usr/local/bin/sbbot
ENV GOMAXPROCS=2 GOMEMLIMIT=64MiB
ENTRYPOINT ["/usr/local/bin/sbbot"]
