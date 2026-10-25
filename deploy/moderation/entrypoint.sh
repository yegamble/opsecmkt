#!/bin/sh
# The reviewed image must provide /bin/sh, env, sha256sum and /app/llama-server.
# Native Unix socket support is required: --host paths ending in .sock.
set -eu
umask 007

case "${MODERATION_WEIGHTS_SHA256:-}" in
  ''|*[!0-9a-f]*) echo 'moderation runtime: invalid weights pin' >&2; exit 1 ;;
esac
[ "${#MODERATION_WEIGHTS_SHA256}" -eq 64 ] || {
  echo 'moderation runtime: invalid weights pin' >&2
  exit 1
}
actual=$(sha256sum /models/model.gguf)
actual=${actual%% *}
[ "$actual" = "$MODERATION_WEIGHTS_SHA256" ] || {
  echo 'moderation runtime: weights pin mismatch' >&2
  exit 1
}

# Never remove another service's socket. A stale socket requires an operator
# to verify the old process has stopped before removing that dedicated file.
[ ! -e /run/moderation/inference.sock ] && [ ! -L /run/moderation/inference.sock ] || {
  echo 'moderation runtime: socket path is occupied' >&2
  exit 1
}

exec env -i \
  PATH=/usr/local/bin:/usr/bin:/bin \
  HOME=/tmp TMPDIR=/tmp \
  HF_HUB_OFFLINE=1 TRANSFORMERS_OFFLINE=1 HF_HUB_DISABLE_TELEMETRY=1 DO_NOT_TRACK=1 \
  /app/llama-server \
  --model /models/model.gguf \
  --host /run/moderation/inference.sock \
  --port 8080 \
  --ctx-size 4096 --parallel 1 --threads 2 \
  --no-webui --log-disable
