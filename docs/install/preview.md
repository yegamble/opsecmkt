# Read-only preview

Browse the interface with sample data. No Docker, database or wallet. Every form submission is refused.

**Needs:** [Go](https://go.dev/dl/) 1.26.9+.

## Run it

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
go run ./cmd/server -preview
```

Open <http://127.0.0.1:8080>. Stop with <kbd>Ctrl</kbd>+<kbd>C</kbd>.

The first run downloads the Go toolchain pinned in `go.mod`.

## Options

- `PREVIEW_ADDR=127.0.0.1:18080`<br>
  Another port. Must be a loopback IP and port (`127.0.0.1:…` or `[::1]:…`), not `localhost`.
- `GOTOOLCHAIN=local`<br>
  Use your installed Go. Refuse the toolchain download.

```sh
PREVIEW_ADDR=127.0.0.1:18080 go run ./cmd/server -preview
```

## What now?

- [Local marketplace](local.md): create accounts, listings and orders.
