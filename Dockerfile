FROM golang:1.27.3-bookworm@sha256:5c3e9b43cfd3bb95c7937462e29d87ada57b481a96fa23ad0eb61778d7a00110 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /server ./cmd/server
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# Apply stable-distribution security fixes even between pinned base-image rebuilds. CI passes the build day as
# SECURITY_REFRESH so a cached build layer is never older than a day; an unset value changes nothing.
ARG SECURITY_REFRESH=
RUN apt-get update && apt-get upgrade -y --no-install-recommends && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
# Only app writes the private administrator socket; tor-admin receives read/traverse access via group 65532.
RUN install -d -o 65532 -g 65532 -m 0750 /run/market-admin
RUN install -d -o 65532 -g 65532 -m 0700 /var/lib/opsecmkt-retention
WORKDIR /app
COPY --from=build /server /app/server
COPY web /app/web
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/server"]
