FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /server ./cmd/server
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
# Only app writes the private administrator socket; tor-admin receives read/traverse access via group 65532.
RUN install -d -o 65532 -g 65532 -m 0750 /run/market-admin
RUN install -d -o 65532 -g 65532 -m 0700 /var/lib/opsecmkt-retention
WORKDIR /app
COPY --from=build /server /app/server
COPY web /app/web
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/server"]
