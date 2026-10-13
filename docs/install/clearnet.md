# Public domain (HTTPS)

Run the market on your domain. The app listens on `127.0.0.1:8080`. Your HTTPS reverse proxy forwards to it.

**Time:** about 15 minutes, plus node sync. **Result:** a market at `https://YOUR-DOMAIN` with an administrator.

## Before you begin

- [Requirements](requirements.md) met.
- DNS for your domain points at the host. Ports 80 and 443 are open.
- An HTTPS reverse proxy on the host, such as [Caddy](https://caddyserver.com/) or nginx, with a certificate.
- A node choice. The installer cannot add nodes later:

| Choice | Result |
| --- | --- |
| `--nodes local` | Pruned Bitcoin signet and Monero stagenet nodes. 25–30 GB disk, hours to sync. |
| `--nodes custom` | Choose network, image and pruning per coin. |
| `--nodes none` | No nodes. No payments with a bundled node. |

## Step 1: Get the release

Use the latest tag from [Releases](https://github.com/yegamble/opsecmkt/releases), not `main`.

```sh
git clone https://github.com/yegamble/opsecmkt.git
cd opsecmkt
git checkout v0.1.0-alpha.2
```

## Step 2: Run the installer

```sh
./scripts/install.sh init --clearnet --behind-proxy yes --nodes local
```

Another port: prefix `APP_PORT=PORT`. External database: add `--external-db URL`
([details](external-database.md)).

Output:

```text
Application healthy at http://127.0.0.1:8080. Secure cookies require HTTPS, so finish through your reverse proxy.
…
Ready. Open https://YOUR-DOMAIN/setup to name your marketplace and create its administrator.
Setup token: 3f1c…
```

## Step 3: Configure the reverse proxy

The proxy **must forward the original `Host` header**, or every form fails with 403 "Origin does not match Host".

Caddy (forwards `Host` by default):

```caddyfile
market.example.com {
    reverse_proxy 127.0.0.1:8080
}
```

nginx:

```nginx
server {
    listen 443 ssl;
    server_name market.example.com;
    ssl_certificate     /etc/letsencrypt/live/market.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/market.example.com/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
    }
}
```

- Non-default HTTPS port: use `proxy_set_header Host $http_host;`.
- Apache: `ProxyPreserveHost On`.
- Proxy in a container: see [Clearnet and onion deployment](../operator-guide.md#clearnet-and-onion-deployment).

Reload the proxy.

## Step 4: Open the setup page

Open `https://YOUR-DOMAIN/setup`, then follow [First steps](first-steps.md).

> **Note:** use HTTPS, not `http://127.0.0.1:8080`. The session cookie is Secure, so sign-in fails over HTTP.

## Step 5: Verify

```sh
./scripts/install.sh status
```

Every service is healthy and setup is complete.

## Optional

| Add | Command |
| --- | --- |
| Photo uploads ([scanner](../operator-guide.md#photo-uploads-and-the-malware-scanner)) | `./scripts/install.sh add-scanner` |

## What now?

- [First steps](first-steps.md)
- [Enabling payments](enable-payments.md)
- [Production checklist](production-checklist.md)
