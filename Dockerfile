FROM golang:1.27.2-bookworm@sha256:5cf287a799e6b94384bad13d16b14904c531f51ba65792237e122ce42b392f61 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /server ./cmd/server
# The escrow signer (A-454) ships in the same image and runs as its own Compose service (compose.nodes.yaml).
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /escrow-signer ./cmd/escrow-signer
FROM debian:bookworm-slim@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
# The escrow signer's directories, copied into its named volumes when they are first created: the seed's (only its
# user, 65533) and the socket's (its group, 65532, is the app's, so the app can connect to the 0660 socket).
RUN install -d -o 65533 -g 65533 -m 0700 /var/lib/escrow-signer && install -d -o 65533 -g 65532 -m 0750 /run/escrow-signer
WORKDIR /app
COPY --from=build /server /app/server
COPY --from=build /escrow-signer /app/escrow-signer
COPY web /app/web
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/server"]
