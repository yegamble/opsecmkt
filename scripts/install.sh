#!/usr/bin/env bash
# OPSEC Market installer and operator helper. It works in the checkout that contains it.
#
#   ./scripts/install.sh [init] [options]   first install: write .env, start, print the setup link (default)
#   ./scripts/install.sh --local            quick loopback install with no questions (init --local)
#   ./scripts/install.sh check              validate .env and the Compose configuration; starts nothing
#   ./scripts/install.sh status             mode, service health, setup state and onion addresses
#   ./scripts/install.sh setup-token [--new]   show (or reissue) the first-run setup token while setup is pending
#   ./scripts/install.sh add-mirror         add the second onion address (Tor installs)
#   ./scripts/install.sh init --tor --onion-key-dir DIR, add-mirror --mirror-onion-key-dir DIR
#                                           use an existing onion identity (a vanity address) instead of a new one
#   ./scripts/install.sh add-scanner        add the ClamAV malware scanner that photo uploads need
#   ./scripts/install.sh refresh-scanner    have Docker fetch the scanner's newer signatures and restart it
#   ./scripts/install.sh upgrade [--no-backup]  after pulling new code: back up, update .env, rebuild, restart
#
# Secrets live only in .env (mode 0600). The installer prints one of them, SETUP_TOKEN, and only in init's final
# block and from setup-token. After init it changes .env only through env_set/env_unset (a temporary file moved
# into place), and it never fetches anything itself: Docker builds and pulls the images.
set -euo pipefail
# Never trace: SHELLOPTS=xtrace in the caller's environment would print every secret the installer handles.
set +x
cd "$(dirname "${BASH_SOURCE[0]}")/.."
umask 077

# Pinned node images: reviewed releases for the escrow-support nodes (A-100), each written with its tag's multi-arch
# index digest beside the tag (A-523), as `docker buildx imagetools inspect <tag>` printed it (its Digest: line,
# 2026-10-13). Change the tags together with compose.nodes.yaml (tags only) and .env.example (tag and digest), and
# resolve the new digests the same way (docs/operator-guide.md#node-image-versions).
bitcoin_image_default='bitcoin/bitcoin:31.1'
monero_image_default='ghcr.io/sethforprivacy/simple-monerod:v0.18.5.1'
monero_wallet_image_default='ghcr.io/sethforprivacy/simple-monero-wallet-rpc:v0.18.5.1'
bitcoin_image_default+='@sha256:da25cedc66b1daefff9f412ee196c901a899c3fa68a33b20849c3e08b5c40d63'
monero_image_default+='@sha256:0329465ffc599048efd57712d91d3b4b7d66380bd7205d5d0992cd9faf668e38'
monero_wallet_image_default+='@sha256:09ff81f470d6d37811e3fde808c5cd1158a8cd2c42abbfb935b33f26f91e879d'
# The malware scanner's image: a ClamAV release with signatures (never _base or latest). Change it together with
# compose.scanner.yaml and .env.example (docs/operator-guide.md#photo-uploads-and-the-malware-scanner).
clamav_image_default='clamav/clamav:1.5.4-debian'
# The only scanner address the app is given: compose.scanner.yaml's clamd, on the internal scanner network.
clamd_address='clamd:3310'
# Monero escrow's seed key: the installer copies the operator's public key to escrow_seed_key_file, and
# compose.escrow-xmr.yaml mounts it read-only at xmr_seed_key_path inside the app (ESCROW_XMR_SEED_KEY).
escrow_seed_key_file=escrow-xmr-seed-key.asc
xmr_seed_key_path=/etc/opsecmkt/escrow-xmr-seed-key.asc
# Local nodes are pruned unless the operator says no. Keep these equal to compose.nodes.yaml's defaults.
bitcoin_prune_default=2000
monero_prune_default='--prune-blockchain --sync-pruned-blocks'
# A-523: init writes mainnet (main, mainnet) for the coins it runs local nodes for unless a test network is chosen. On
# mainnet bitcoind keeps 15000 MiB of blocks (the owner's choice: about two months, an estimate) and an escrow counts
# as funded after 6 confirmations. Compose's blank fallbacks stay test networks, so existing installs never move.
bitcoin_mainnet_prune_mb=15000
bitcoin_mainnet_confirmations=6
onion_hostname=/var/lib/tor/marketplace/hostname
onion_pattern='^[a-z2-7]{56}\.onion$'
project_pattern='^[a-z0-9][a-z0-9_-]*$'
env_key_pattern='^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)[[:space:]]*='
# Every documented setting and optional Compose override. Commands unset them and Compose's environment-file
# controls so validation and startup both read this checkout's .env rather than the operator's shell.
# MONERO_NETWORK_FLAG is documented without an assignment: a copied blank line would select mainnet.
env_keys='DATABASE_URL POSTGRES_PASSWORD APP_SECRET_KEY SETUP_TOKEN APP_MODE COOKIE_SECURE APP_PORT ADMIN_SOCKET TOR_ADMIN_AUTH_DIR AUDIT_EXPORT_RECIPIENT
DATABASE_CONNECT_TIMEOUT MIGRATION_LOCK_TIMEOUT MIGRATION_TIMEOUT COMPOSE_PROJECT_NAME COMPOSE_FILE COMPOSE_PROFILES
BITCOIN_RPC_URL BITCOIN_WALLET BITCOIN_CHAIN PAYMENT_CONFIRMATIONS_BTC MONERO_RPC_URL MONERO_WALLET_RPC_URL
MONERO_NETWORK MONERO_NETWORK_FLAG PAYMENT_CONFIRMATIONS_XMR PAYMENT_POLL_INTERVAL PAYMENT_EXPIRY BITCOIN_MAX_TIP_AGE MONERO_MAX_TIP_AGE AUDIT_SIGNING_KEY BITCOIN_IMAGE
MONERO_IMAGE MONERO_WALLET_IMAGE BITCOIN_PRUNE_MB MONERO_PRUNE_FLAGS BITCOIN_RPC_PASSWORD BITCOIN_RPC_AUTH MONERO_WALLET_RPC_PASSWORD
CLAMD_ADDRESS CLAMAV_IMAGE CLAMAV_IMAGE_DEFAULT SCANNER_MAX_SIGNATURE_AGE_DAYS IMAGE_STORAGE_LIMIT_MB ESCROW_BTC ESCROW_BTC_RPC_URL
ESCROW_BTC_ARBITER_WALLET ESCROW_BTC_WATCH_WALLET ESCROW_BTC_FEE_BPS ESCROW_BTC_FEE_ADDRESS ESCROW_BTC_FEE_RATE
ESCROW_XMR ESCROW_XMR_WALLET_RPC_URL ESCROW_XMR_DAEMON_URL ESCROW_XMR_FEE_BPS ESCROW_XMR_FEE_ADDRESS ESCROW_XMR_SLOTS
ESCROW_XMR_MAX_FEE ESCROW_XMR_SEED_KEY ESCROW_XMR_TOR_RELAY MONERO_WALLET_SECRET ESCROW_XMR_WALLET_RPC_PASSWORD ESCROW_SIGNER_SOCKET
ESCROW_SIGNER_RPC_URL LOG_DRIVER TOR_VERSION'
# What `upgrade` writes for a key missing from .env: the value Compose already uses when the key is absent, so
# adding it changes nothing but makes it visible. APP_MODE, APP_PORT, COMPOSE_PROJECT_NAME, COMPOSE_PROFILES and
# COMPOSE_FILE are derived from the install instead; secrets are never invented (APP_SECRET_KEY is copied from
# SETUP_TOKEN). tests/test_installer.py checks that this covers every non-secret key in .env.example. It never holds
# MONERO_NETWORK_FLAG: absent runs stagenet, and a blank line would move a test-network install to mainnet (A-523).
upgrade_defaults="COOKIE_SECURE=true
ADMIN_SOCKET=
TOR_ADMIN_AUTH_DIR=
AUDIT_EXPORT_RECIPIENT=
DATABASE_CONNECT_TIMEOUT=
MIGRATION_LOCK_TIMEOUT=
MIGRATION_TIMEOUT=
BITCOIN_RPC_URL=
BITCOIN_WALLET=
BITCOIN_CHAIN=
PAYMENT_CONFIRMATIONS_BTC=3
MONERO_RPC_URL=
MONERO_WALLET_RPC_URL=
MONERO_NETWORK=
PAYMENT_CONFIRMATIONS_XMR=10
PAYMENT_POLL_INTERVAL=30s
PAYMENT_EXPIRY=24h
BITCOIN_MAX_TIP_AGE=2h
MONERO_MAX_TIP_AGE=1h
BITCOIN_IMAGE=$bitcoin_image_default
MONERO_IMAGE=$monero_image_default
MONERO_WALLET_IMAGE=$monero_wallet_image_default
BITCOIN_PRUNE_MB=$bitcoin_prune_default
MONERO_PRUNE_FLAGS=$monero_prune_default
CLAMD_ADDRESS=
CLAMAV_IMAGE=$clamav_image_default
SCANNER_MAX_SIGNATURE_AGE_DAYS=
IMAGE_STORAGE_LIMIT_MB=
ESCROW_BTC=
ESCROW_BTC_RPC_URL=
ESCROW_BTC_ARBITER_WALLET=
ESCROW_BTC_WATCH_WALLET=
ESCROW_BTC_FEE_BPS=
ESCROW_BTC_FEE_ADDRESS=
ESCROW_BTC_FEE_RATE=
ESCROW_SIGNER_SOCKET=
ESCROW_SIGNER_RPC_URL=
ESCROW_XMR=
ESCROW_XMR_WALLET_RPC_URL=
ESCROW_XMR_DAEMON_URL=
ESCROW_XMR_FEE_BPS=
ESCROW_XMR_FEE_ADDRESS=
ESCROW_XMR_SLOTS=
ESCROW_XMR_MAX_FEE=
ESCROW_XMR_SEED_KEY=
ESCROW_XMR_TOR_RELAY="
# Values from the caller's environment that init and upgrade honour, read before isolate_env clears them.
caller_app_port=${APP_PORT-}
caller_project=${COMPOSE_PROJECT_NAME-}
caller_wallet_image=${MONERO_WALLET_IMAGE-}
temporaries=''
# A private temporary directory holding the copy of an imported onion key that is checked and streamed (A-526).
onion_stage=''
onion_files=()
env_locked=false
# The file env_get reads and env_edit rewrites: .env, or a staged copy that a command moves into place whole
# (add-scanner changes three keys together).
env_file=.env
# Bash runs this on exit, including after SIGINT, SIGTERM and SIGHUP.
cleanup() {
  if [[ -n $temporaries ]]; then rm -f $temporaries; fi
  if [[ -n $onion_stage ]]; then rm -rf -- "$onion_stage"; fi
  if [[ $env_locked == true ]]; then rmdir .env.lock 2>/dev/null || true; fi
}
trap cleanup EXIT

usage() {
  usage_text >&2
  exit 2
}

usage_text() {
  cat <<'USAGE'
Usage: scripts/install.sh [init] [options]   first install (the default)
       scripts/install.sh --local [--admin HANDLE [--site-name NAME]]
       scripts/install.sh check | status | add-mirror | add-scanner | refresh-scanner
       scripts/install.sh add-mirror [--mirror-onion-key-dir DIR [--mirror-onion-address ADDR.onion]]
       scripts/install.sh setup-token [--new]
       scripts/install.sh upgrade [--no-backup]
init options (each answers one question; a question without its option is asked):
  --tor | --clearnet               exposure
  --local                          loopback HTTP, internal database, no nodes, no questions
  --external-db URL                external PostgreSQL (with any option given: an internal database otherwise)
  --behind-proxy yes|no            clearnet: behind an HTTPS reverse proxy (secure cookies), or local HTTP only
  --nodes local|none|custom        pruned local Bitcoin and Monero escrow-support nodes, none, or per coin
  --bitcoin local|disabled         --bitcoin-image IMAGE  --bitcoin-network main|signet|testnet4|regtest
  --bitcoin-prune yes|no
  --monero local|disabled           --monero-image IMAGE   --monero-network mainnet|stagenet
  --monero-prune yes|no
  --monero-escrow yes|no           Monero escrow on the local Monero node (custodial; default no)
  --monero-escrow-seed-key FILE    with it: the armored OpenPGP public key its wallet seeds are sealed to
  --admin HANDLE [--site-name NAME]  create the first administrator here (password asked twice, never shown)
                                   instead of printing a setup token for the browser
  --uploads yes|no                 photo uploads with the ClamAV malware scanner (needs 3 GiB of memory; default no)
  --onion-key-dir DIR              Tor: use this onion identity (e.g. a vanity address) instead of a new one: DIR holds
                                   hs_ed25519_secret_key and optionally hs_ed25519_public_key, outside this checkout
  --onion-address ADDR.onion       with it: the address the key must have (otherwise it is shown for you to confirm)
add-mirror's two options do the same for the mirror, before it first starts.
Local nodes run MAINNET (main, mainnet: real funds) unless a network option names a test network; the other coin then
defaults to its own (signet, stagenet): a market is all mainnet or all test networks. No mainnet run of this software
is recorded (docs/operator-guide.md#mainnet).
Values given as options (URLs with passwords) are visible to other local users while the installer runs.
USAGE
}

die() {
  printf '%s\n' "$@" >&2
  exit 1
}

# ---- .env access -------------------------------------------------------------------------------------------

# env_get KEY [FILE]: print the value of the last line assigning KEY in FILE (default $env_file, normally .env;
# quotes removed, as Compose reads the installer's single quotes, double quotes, `export` and unquoted values);
# status 1 when no line assigns it.
env_get() {
  local key=$1 file=${2:-$env_file} line value='' found=false
  [[ -f $file ]] || return 1
  while IFS= read -r line || [[ -n $line ]]; do
    line=${line%$'\r'}
    if [[ $line =~ $env_key_pattern && ${BASH_REMATCH[2]} == "$key" ]]; then
      value=${line#*=}
      found=true
    fi
  done < "$file"
  [[ $found == true ]] || return 1
  value=${value#"${value%%[![:space:]]*}"}
  case $value in
    \'*) value=${value#\'}; value=${value%%\'*} ;;
    \"*) value=${value#\"}; value=${value%%\"*} ;;
    *) value=${value%%[[:space:]]#*}; value=${value%"${value##*[![:space:]]}"} ;;
  esac
  printf '%s' "$value"
}

# env_value KEY: the value, or nothing when the key is absent.
env_value() { env_get "$1" || true; }

# env_edit KEY VALUE set|unset: rewrite .env (or $env_file) line by line into a temporary file beside it, then
# move that into place with mode 0600. Only lines assigning KEY change: set replaces the first with KEY='VALUE'
# (appending one when none exists) and drops later ones; unset drops them all. Every other line, including
# comments, blank lines, CRLF endings and a missing final newline, is copied byte for byte. Every step is checked here, not by
# errexit, which is off when a caller tests the result (`retire_setup_token ... || true`, `if ...`): a failed
# write (a full disk) exits before .env is replaced, so .env is either the old file or the complete new one.
env_edit() {
  local key=$1 value=$2 action=$3 line temporary replaced=false ended open=false
  [[ $value != *"'"* && $value != *$'\n'* && $value != *$'\r'* ]] || die 'Values cannot contain single quotes or newlines.'
  temporary=$(mktemp .env.edit.XXXXXX) || die 'Could not write .env; it was not changed.'
  temporaries="$temporaries $temporary"
  # One redirection for the whole file (appending writes can escape a file size limit on some systems).
  {
    while :; do
      ended=true
      IFS= read -r line || { [[ -n $line ]] || break; ended=false; }
      if [[ $line =~ $env_key_pattern && ${BASH_REMATCH[2]} == "$key" ]]; then
        if [[ $action == set && $replaced == false ]]; then
          printf "%s='%s'\n" "$key" "$value" || env_edit_failed "$temporary"
        fi
        replaced=true
        open=false
      elif [[ $ended == true ]]; then
        printf '%s\n' "$line" || env_edit_failed "$temporary"
        open=false
      else
        printf '%s' "$line" || env_edit_failed "$temporary"
        open=true
      fi
      [[ $ended == true ]] || break
    done < "$env_file" || env_edit_failed "$temporary"
    if [[ $action == set && $replaced == false ]]; then
      if [[ $open == true ]]; then printf '\n' || env_edit_failed "$temporary"; fi
      printf "%s='%s'\n" "$key" "$value" || env_edit_failed "$temporary"
    fi
  } > "$temporary" || env_edit_failed "$temporary"
  chmod 600 "$temporary" && mv -f "$temporary" "$env_file" || env_edit_failed "$temporary"
}
env_edit_failed() {
  rm -f "$1"
  die 'Could not write .env; it was not changed.'
}
env_set() { env_edit "$1" "$2" set; }
env_unset() { env_edit "$1" '' unset; }

# lock_env: take .env.lock (mkdir is atomic everywhere) so that two installer commands cannot both read .env,
# change it and write it back, the second losing the first one's change. cleanup removes it on exit. Status 1,
# with nothing taken, when another command holds it.
lock_env() {
  [[ $env_locked == false ]] || return 0
  mkdir .env.lock 2>/dev/null || return 1
  env_locked=true
}
lock_env_or_die() {
  lock_env || die 'Another ./scripts/install.sh command is changing .env in this checkout (.env.lock exists), so nothing was done.' \
    'Wait for it to finish and run this again. If none is running (it was killed), remove the stale lock with: rmdir .env.lock'
}

isolate_env() {
  local key
  for key in $env_keys COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE COMPOSE_PATH_SEPARATOR; do unset "$key"; done
}

has_item() { [[ ,$1, == *,"$2",* ]]; }  # has_item LIST ITEM: comma-separated list membership

# compose_has FILE: COMPOSE_FILE lists FILE (by base name; split on ':' and ';' as the app does).
compose_has() {
  local file files
  files=$(env_value COMPOSE_FILE)
  local IFS=':;'
  for file in $files; do
    file=${file//\\//}
    [[ ${file##*/} != "$1" ]] || return 0
  done
  return 1
}

# compose_after FILE EARLIER: COMPOSE_FILE lists EARLIER before FILE's first appearance (base names, as compose_has).
compose_after() {
  local file files earlier=false
  files=$(env_value COMPOSE_FILE)
  local IFS=':;'
  for file in $files; do
    file=${file//\\//}
    file=${file##*/}
    [[ $file != "$1" ]] || { $earlier; return; }
    [[ $file != "$2" ]] || earlier=true
  done
  return 1
}

# url_host / url_password URL: the host and the userinfo password of a URL (nothing when absent).
url_authority() {
  local rest=${1#*://}
  rest=${rest%%/*}
  printf '%s' "${rest%%\?*}"
}
url_host() {
  local authority
  authority=$(url_authority "$1")
  authority=${authority##*@}
  printf '%s' "${authority%%:*}"
}
url_password() {
  local authority
  authority=$(url_authority "$1")
  [[ $authority == *@* ]] || return 0
  authority=${authority%@*}
  [[ $authority == *:* ]] || return 0
  printf '%s' "${authority#*:}"
}

# url_decode TEXT: TEXT with %XX escapes decoded, as the app's URL parser reads userinfo (+ stays +).
url_decode() {
  local LC_ALL=C text=$1 decoded='' byte
  while [[ -n $text ]]; do
    if [[ $text =~ ^%([0-9A-Fa-f]{2}) ]]; then
      printf -v byte "\\x${BASH_REMATCH[1]}"
      decoded=$decoded$byte
      text=${text:3}
    else
      decoded=$decoded${text:0:1}
      text=${text:1}
    fi
  done
  printf '%s' "$decoded"
}

# secret N prints N random bytes as hex and refuses to continue if generation failed.
secret() {
  local value
  value=$(openssl rand -hex "$1") || value=''
  [[ $value =~ ^[0-9a-f]+$ && ${#value} -eq $(($1 * 2)) ]] || { echo 'Could not generate a random secret with openssl; nothing was started.' >&2; exit 1; }
  printf '%s' "$value"
}

# secret_problem VALUE: why VALUE is not an acceptable secret under the app's rules (checkSecret), or nothing.
secret_problem() {
  local value=$1 length=${#1} lower distinct period unit repeated
  if ((length < 32)); then echo 'must contain at least 32 random characters'; return; fi
  lower=$(printf '%s' "$value" | tr '[:upper:]' '[:lower:]')
  case $lower in *replace*|*changeme*|*change_me*|*placeholder*) echo 'is still a placeholder value'; return ;; esac
  distinct=$(printf '%s' "$value" | fold -w1 | sort -u | wc -l)
  if ((distinct < 8)); then echo 'is not random: it uses fewer than 8 distinct characters'; return; fi
  for ((period = 1; period <= length / 2; period++)); do
    unit=${value:0:period}
    repeated=$unit
    while ((${#repeated} < length)); do repeated=$repeated$unit; done
    if [[ ${repeated:0:length} == "$value" ]]; then echo 'is not random: it repeats a short pattern'; return; fi
  done
}

# database_placeholder URL: URL holds an example word the app refuses (dbPlaceholder: REPLACE in capitals, or
# replace_me, replace-me, replace_with, changeme, change_me or change-me in any case).
database_placeholder() {
  local lower
  [[ $1 != *REPLACE* ]] || return 0
  lower=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')
  case $lower in *replace_me*|*replace-me*|*replace_with*|*changeme*|*change_me*|*change-me*) return 0 ;; esac
  return 1
}

# image_pinned VALUE: the image names a release tag or a digest, not :latest or no tag.
image_pinned() {
  local reference=${1%%@*} tag digest
  if [[ $1 == *@* ]]; then
    digest=${1#*@}
    [[ -n $reference && $digest != *@* && $digest =~ ^sha256:[0-9a-f]{64}$ ]]
    return
  fi
  tag=${reference##*/}
  [[ $tag == *:* ]] || return 1
  tag=${tag##*:}
  [[ -n $tag && $tag != latest ]]
}
image_repository() {
  local reference=${1%%@*} last
  last=${reference##*/}
  if [[ $last == *:* ]]; then reference=${reference%:*}; fi
  printf '%s' "$reference"
}
# clamav_image_ok VALUE: one ClamAV release with signatures: a MAJOR.MINOR.PATCH tag, optionally -debian (upstream
# rebuilds it only with newer signatures), or an @sha256 digest (one build). Every other tag moves to other
# releases (latest, stable, unstable, 1.5 and their -debian forms), and a _base tag has no signatures (with no
# volume clamd would have none).
clamav_image_ok() {
  local reference=${1%%@*} tag=''
  if [[ ${reference##*/} == *:* ]]; then tag=${reference##*:}; fi
  if [[ $1 == *@* ]]; then
    image_pinned "$1" && [[ $tag != *_base* ]]
  else
    [[ $tag =~ ^[0-9]+\.[0-9]+\.[0-9]+(-debian)?$ ]]
  fi
}

# ---- Docker helpers ---------------------------------------------------------------------------------------

require_docker() {
  command -v docker >/dev/null || die 'Install Docker with Compose v2 first.'
  docker compose version >/dev/null || die 'Docker Compose v2 is required.'
}

require_env() {
  [[ -f .env ]] || die 'No .env in this checkout: run ./scripts/install.sh init first.'
}

project_name() {
  local name
  name=$(env_value COMPOSE_PROJECT_NAME)
  printf '%s' "${name:-opsecmkt}"
}

# all_containers: "project|working directory" for every Compose container on this host.
all_containers() {
  docker ps -a --filter label=com.docker.compose.project \
    --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}'
}

# same_dir DIR: DIR is this checkout. The label holds the path Compose was started from, which may go through
# a symlink.
same_dir() {
  [[ -n $1 ]] || return 1
  [[ $1 != "$PWD" ]] || return 0
  [[ -d $1 && $(cd "$1" 2>/dev/null && pwd -P) == "$(pwd -P)" ]]
}

# up_wait [OPTION...]: docker compose up -d OPTION... --wait, except that with the malware scanner on it waits
# for every service but clamd and then starts clamd without waiting. Nothing depends on the scanner (ruling C1),
# so a clamd that never becomes healthy (out of memory, a bad upstream build) must not stop init, upgrade,
# add-mirror or setup-token part-way; uploads are refused until it answers, and status shows it.
up_wait() {
  local listing line services=()
  if ! has_item "$(env_value COMPOSE_PROFILES)" scanner; then
    docker compose up -d "$@" --wait
    return
  fi
  listing=$(docker compose config --services) || return 1
  while IFS= read -r line; do
    if [[ -n $line && $line != clamd ]]; then services+=("$line"); fi
  done <<< "$listing"
  ((${#services[@]})) || return 1
  docker compose up -d "$@" --wait "${services[@]}" || return 1
  docker compose up -d clamd ||
    echo 'warning: the malware scanner (clamd) did not start, so photo uploads are refused. Inspect docker compose logs clamd.' >&2
}

# status_line_ok LINE: LINE is a `-status` line: installed=true|false schema=NNN keyroot=app_secret_key|setup_token
# (an app from before keyroot existed prints no keyroot).
status_line_ok() {
  [[ $1 =~ ^installed=(true|false)\ schema=[0-9]+(\ keyroot=(app_secret_key|setup_token))?$ ]]
}

# app_status: the app's `-status` line, from the running app (so keyroot is the key root it runs with) or, when
# it is not running, a one-off container with this .env (the database must be up). Status 1 when unreadable.
app_status() {
  local out
  out=$(docker compose exec -T app /app/server -status 2>/dev/null) || out=$(docker compose run --rm -T --no-deps app -status) || return 1
  status_line_ok "$out" || return 1
  printf '%s' "$out"
}

# onion_address SERVICE: the onion address Tor wrote for SERVICE (tor or tor-mirror).
onion_address() {
  local address
  address=$(docker compose exec -T "$1" cat "$onion_hostname" 2>/dev/null) || return 1
  address=${address%%[[:space:]]*}
  [[ $address =~ $onion_pattern ]] || return 1
  printf '%s' "$address"
}

# wait_onion SERVICE: wait up to 90 seconds for SERVICE's onion address and print it.
wait_onion() {
  local attempt address deadline=$((SECONDS + 90))
  for ((attempt = 0; attempt < 90 && SECONDS < deadline; attempt++)); do
    if address=$(onion_address "$1"); then
      printf '%s' "$address"
      return 0
    fi
    sleep 1
  done
  return 1
}

# ---- onion identity import (A-526) -------------------------------------------------------------------------
#
# init --onion-key-dir and add-mirror --mirror-onion-key-dir give a new onion service an existing identity, such as a
# vanity address, instead of letting Tor create one. Tor alone is no validator: it silently creates a new identity for
# a truncated key and overwrites a forged hostname. So the files are checked byte for byte, Tor derives the address in a
# throwaway container with no network, the operator confirms it (or names it with --onion-address), the key goes into
# an empty volume before that Tor first starts, and the address Tor then serves is compared once more. Key bytes only
# ever move between files and stdin: never into a variable, an argument, the environment, .env or the output.
# scripts/test-tor-identity-import.sh runs these functions against the real Tor image.

# The dry run, as Tor's user in the Tor image: read the key files from stdin (the secret key, then the public key if
# any), start Tor offline on them in a tmpfs, wait up to 10 seconds for the hostname it derives, stop it and print the
# hostname. Status 3: Tor refused the public key as not matching the secret key. The torrc is an empty file (tor
# 0.4.9.14 refuses -f /dev/null: "Unable to open configuration file"). Never --verify-config: it accepts a mismatched pair. (Single quotes: the
# container expands the script.)
# shellcheck disable=SC2016
onion_dry_run_script='umask 077
cat > /dry/keys
size=$(wc -c < /dry/keys)
case $size in 96|160) ;; *) echo "The key stream is $size bytes, not 96 or 160." >&2; exit 1 ;; esac
mkdir /dry/data /dry/data/hs
head -c 96 /dry/keys > /dry/secret
cp /dry/secret /dry/data/hs/hs_ed25519_secret_key
if [ "$size" = 160 ]; then tail -c 64 /dry/keys > /dry/data/hs/hs_ed25519_public_key; fi
: > /dry/torrc
tor -f /dry/torrc --DisableNetwork 1 --SocksPort 0 --DataDirectory /dry/data --HiddenServiceDir /dry/data/hs \
  --HiddenServicePort "80 127.0.0.1:8080" > /dry/log 2>&1 &
pid=$!
i=0
while [ ! -s /dry/data/hs/hostname ] && kill -0 "$pid" 2>/dev/null && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
kill "$pid" 2>/dev/null || true
wait "$pid" 2>/dev/null || true
if [ ! -s /dry/data/hs/hostname ]; then
  if grep -q "does not match" /dry/log; then exit 3; fi
  grep "\[warn\]\|\[err\]" /dry/log >&2 || tail -n 5 /dry/log >&2
  exit 1
fi
cmp -s /dry/secret /dry/data/hs/hs_ed25519_secret_key || { echo "Tor replaced the key with a new one." >&2; exit 1; }
head -n 1 /dry/data/hs/hostname'

# The import, as root (it writes whatever the volume's owner) with only the capabilities that needs: refuse a volume
# Tor has used, write the key files from stdin into marketplace/, and give them to Tor's user (0700 directories,
# 0600 files: Tor refuses keys it does not own). Tor writes hostname itself when it starts. $0 is the volume's name.
# shellcheck disable=SC2016
onion_import_script='umask 077
if [ -n "$(ls -A /var/lib/tor)" ]; then echo "Refusing: volume $0 is not empty (Tor has run on it), so nothing was written." >&2; exit 3; fi
cat > /in/keys
size=$(wc -c < /in/keys)
case $size in 96|160) ;; *) echo "The key stream is $size bytes, not 96 or 160." >&2; exit 1 ;; esac
mkdir /var/lib/tor/marketplace
head -c 96 /in/keys > /var/lib/tor/marketplace/hs_ed25519_secret_key
if [ "$size" = 160 ]; then tail -c 64 /in/keys > /var/lib/tor/marketplace/hs_ed25519_public_key; fi
chown -R debian-tor:debian-tor /var/lib/tor
chmod 700 /var/lib/tor /var/lib/tor/marketplace
chmod 600 /var/lib/tor/marketplace/hs_ed25519_*'

# onion_address_option OPTION VALUE: VALUE as a v3 onion address in lower case, else a usage error.
onion_address_option() {
  local address
  address=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')
  [[ $address =~ $onion_pattern ]] || { echo "$1 takes a v3 onion address: 56 characters (a-z, 2-7) and .onion." >&2; exit 2; }
  printf '%s' "$address"
}

# onion_key_check DIR: copy DIR's hs_ed25519_secret_key, and hs_ed25519_public_key when there is one, into the private
# directory onion_stage and check the copies: regular files (not symlinks) in a directory outside this checkout, the
# secret key exactly 96 bytes and the public key 64, each starting with Tor's 32-byte header. A hostname file is
# ignored (Tor derives the address from the key). Sets onion_files to the copies, the secret key first. Exits on a
# problem; nothing is written anywhere else.
onion_key_check() {
  local dir=$1 physical here name file copy size expected
  [[ -d $dir ]] || die "Refusing: $dir is not a directory. Nothing was written."
  physical=$(cd -- "$dir" && pwd -P) || die "Refusing: cannot open $dir. Nothing was written."
  here=$(pwd -P)
  [[ $physical != "$here" && $physical != "$here"/* ]] ||
    die "Refusing: $dir is inside this checkout, where git or a Docker build could pick up the private key. Move it outside first. Nothing was written."
  onion_stage=$(mktemp -d "${TMPDIR:-/tmp}/opsecmkt-onion.XXXXXX") || die 'Could not create a temporary directory. Nothing was written.'
  onion_files=()
  for name in secret public; do
    file=$dir/hs_ed25519_${name}_key
    if [[ $name == public && ! -e $file && ! -L $file ]]; then continue; fi
    [[ -f $file && ! -L $file ]] || die "Refusing: $file must be a regular file, not a symlink (or missing). Nothing was written."
    copy=$onion_stage/hs_ed25519_${name}_key
    cat -- "$file" > "$copy" || die "Refusing: cannot read $file. Nothing was written."
    expected=96
    if [[ $name == public ]]; then expected=64; fi
    size=$(wc -c < "$copy") || die "Refusing: cannot read $file. Nothing was written."
    size=$((size))
    ((size == expected)) ||
      die "Refusing: $file is $size bytes; a Tor v3 onion $name key is exactly $expected bytes. Nothing was written."
    # The header is the only part compared, file to file (a NUL cannot be held in a bash variable).
    printf "== ed25519v1-%s: type0 ==\\0\\0\\0" "$name" > "$onion_stage/header"
    head -c 32 "$copy" | cmp -s - "$onion_stage/header" ||
      die "Refusing: $file does not start with Tor's $name key header (== ed25519v1-$name: type0 == and three NUL bytes). Nothing was written."
    onion_files+=("$copy")
  done
  rm -f -- "$onion_stage/header"
}

# onion_dry_run IMAGE: the address Tor derives from the checked key, started offline in a throwaway container of the
# project's Tor image: no network, no capabilities, a tmpfs, the key files streamed on stdin. Exits on a refusal.
onion_dry_run() {
  local address status=0
  address=$(cat -- "${onion_files[@]}" | docker run --rm -i --pull never --network none --cap-drop ALL \
    --security-opt no-new-privileges:true --tmpfs /dry --entrypoint sh "$1" -euc "$onion_dry_run_script") || status=$?
  case $status in
    0) ;;
    3) die 'Refusing: hs_ed25519_public_key does not match hs_ed25519_secret_key (Tor refused the pair). Use the two files of one identity, or only the secret key. Nothing was written.' ;;
    *) die 'Refusing: Tor did not accept the key (the message above says why). Nothing was written.' ;;
  esac
  [[ $address =~ $onion_pattern ]] || die 'Refusing: Tor wrote no onion address for the key. Nothing was written.'
  printf '%s' "$address"
}

# onion_confirm DERIVED EXPECTED: DERIVED must equal EXPECTED (--onion-address); without one the operator confirms it
# in a terminal.
onion_confirm() {
  local confirmed=''  # not "answer": ask keeps its own local of that name
  if [[ -n $2 ]]; then
    [[ $1 == "$2" ]] || die "Refusing: the key derives $1, not $2. Nothing was written."
    return 0
  fi
  [[ -t 0 ]] || die 'Refusing: with no terminal to confirm the address on, give it as --onion-address. Nothing was written.'
  printf 'The key is for %s.\n' "$1"
  ask confirmed 'Type yes to use this onion address: '
  [[ $confirmed == yes ]] || die 'Not confirmed. Nothing was written.'
}

# onion_volume_create PROJECT NAME: create PROJECT's Compose volume NAME, labelled as Compose labels its own (so
# Compose uses it and `docker compose down -v` removes it).
onion_volume_create() {
  docker volume create --label "com.docker.compose.project=$1" --label "com.docker.compose.volume=$2" "${1}_$2" >/dev/null
}

# onion_import IMAGE VOLUME: stream the checked key files into the empty VOLUME (onion_import_script).
onion_import() {
  cat -- "${onion_files[@]}" | docker run --rm -i --pull never --network none --user 0 --cap-drop ALL --cap-add CHOWN \
    --cap-add DAC_OVERRIDE --cap-add FOWNER --security-opt no-new-privileges:true --tmpfs /in -v "$2:/var/lib/tor" \
    --entrypoint sh "$1" -euc "$onion_import_script" "$2"
}

# onion_import_notes DIR: after an import and the backup command, the warning and what to do with DIR.
onion_import_notes() {
  echo 'Warning: a key generated by anyone else, including a vanity service, is held by them: generate vanity keys yourself (e.g. mkp224o, offline).'
  printf 'Once that encrypted backup is stored, securely delete the plaintext keys in %s.\n' "$1"
}

# retire_setup_token STATUS: once setup is complete SETUP_TOKEN grants nothing (the app refuses it), so remove
# it from .env, but only when no derived key changes: the app reports (keyroot=app_secret_key) that it derives
# its keys from APP_SECRET_KEY, and .env still sets that. Status 0 when it removed the line, 1 when it left
# .env alone. The caller holds the .env lock.
retire_setup_token() {
  [[ $1 == installed=true\ * ]] || return 1
  env_get SETUP_TOKEN >/dev/null || return 1
  [[ -n $(env_value APP_SECRET_KEY) ]] || return 1
  if [[ $1 != *\ keyroot=app_secret_key ]]; then
    echo 'SETUP_TOKEN stays in .env: the running app does not report that its keys derive from APP_SECRET_KEY (it was started before .env had it, or by an older release). Recreate it with docker compose up -d, then run ./scripts/install.sh status to remove the token.'
    return 1
  fi
  env_unset SETUP_TOKEN
  echo 'Removed SETUP_TOKEN from .env: setup is complete, so it grants nothing now.'
}

# Bitcoin Core's rpcauth verifier: a random salt and HMAC of the existing private RPC password. The password
# goes through stdin, never an external program argument; only the salt appears in openssl's argv. Supplying a
# salt verifies an existing .env entry without changing it. Core's share/rpcauth/rpcauth.py uses this format.
bitcoin_rpcauth() {
  local password=$1 salt=${2:-} hash
  if [[ -z $salt ]]; then
    salt=$(openssl rand -hex 16) || return 1
  fi
  [[ $salt =~ ^[0-9a-f]{32}$ ]] || return 1
  hash=$(printf '%s' "$password" | openssl dgst -sha256 -hmac "$salt" | sed 's/^.* //') || return 1
  [[ $hash =~ ^[0-9a-f]{64}$ ]] || return 1
  printf 'marketplace:%s$%s' "$salt" "$hash"
}

# mixed_networks BITCOIN_CHAIN MONERO_NETWORK: one coin is on mainnet and the other names a test network (as the
# app's marketLiveFromEnv reads them; a blank is neither).
mixed_networks() {
  [[ $1 == main && -n $2 && $2 != mainnet || $2 == mainnet && -n $1 && $1 != main ]]
}

# ---- check ------------------------------------------------------------------------------------------------

# Legacy pooled configuration must be removed only after the previous drain-capable build has emptied it. This
# runs before an upgrade takes a backup, copies .env or invokes Compose, and never prints configured values.
legacy_pooled_guard() {
  local key failed=false
  for key in BITCOIN_RPC_URL BITCOIN_WALLET MONERO_RPC_URL MONERO_WALLET_RPC_URL; do
    if [[ -n $(env_value "$key") ]]; then
      printf 'Refusing: %s is a retired pooled-payment setting. Use the previous drain-capable build to drain pooled funds, then remove this setting and retry; nothing was changed.\n' "$key" >&2
      failed=true
    fi
  done
  if has_item "$(env_value COMPOSE_PROFILES)" monero-wallet; then
    echo 'Refusing: COMPOSE_PROFILES includes the retired monero-wallet profile. Use the previous drain-capable build to drain pooled funds, then remove this profile and retry; nothing was changed.' >&2
    failed=true
  fi
  [[ $failed == false ]]
}

# run_check: the app's startup refusals (D8) plus what only the host can see. Messages name keys, never values.
run_check() {
  local problems='' warnings='' key value problem mode secure files profiles database host project listing
  local entry entry_project entry_dir foreign='' moved='' elsewhere='' rpc_password rpc_auth salt expected_auth
  local rpc_auth_pattern='^marketplace:[0-9a-f]{32}\$[0-9a-f]{64}$'
  legacy_pooled_guard || return 1
  for key in APP_SECRET_KEY SETUP_TOKEN; do
    value=$(env_value "$key")
    [[ -n $value ]] || continue
    problem=$(secret_problem "$value")
    [[ -z $problem ]] || problems="$problems  - $key $problem; generate one with \`openssl rand -hex 32\` and see UPGRADING.md before changing it on an existing install"$'\n'
  done
  if [[ -z $(env_value APP_SECRET_KEY) && -z $(env_value SETUP_TOKEN) ]]; then
    problems="$problems  - APP_SECRET_KEY is not set (nor the legacy SETUP_TOKEN): the app has no key root"$'\n'
  fi
  mode=$(env_value APP_MODE)
  mode=${mode:-clearnet}
  case $mode in
    clearnet|tor) ;;
    *) problems="$problems  - APP_MODE must be clearnet or tor"$'\n' ;;
  esac
  secure=$(env_value COOKIE_SECURE)
  case $secure in
    ''|true|false) ;;
    *) problems="$problems  - COOKIE_SECURE must be exactly true or false"$'\n' ;;
  esac
  if [[ $mode == tor && $secure != false ]]; then
    warnings="${warnings}warning: APP_MODE=tor with COOKIE_SECURE=true: only browsers that treat .onion as secure keep the session cookie; the installer writes false for Tor"$'\n'
  fi
  database=$(env_value DATABASE_URL)
  if [[ -z $database ]]; then
    problems="$problems  - DATABASE_URL is not set"$'\n'
  elif database_placeholder "$database"; then
    problems="$problems  - DATABASE_URL still contains a placeholder (as in .env.example)"$'\n'
  fi
  files=$(env_value COMPOSE_FILE)
  if [[ -n $files ]]; then
    if compose_has compose.clearnet.yaml && compose_has compose.tor.yaml; then
      problems="$problems  - COMPOSE_FILE lists both compose.clearnet.yaml and compose.tor.yaml"$'\n'
    elif [[ $mode == clearnet || $mode == tor ]] && ! compose_has "compose.$mode.yaml"; then
      problems="$problems  - APP_MODE=$mode needs compose.$mode.yaml in COMPOSE_FILE"$'\n'
    fi
    # Local nodes over Tor (A-442): it replaces the node commands, so listed before compose.nodes.yaml it would lose.
    if compose_has compose.nodes-tor.yaml; then
      if [[ $mode != tor ]] || ! compose_has compose.tor.yaml; then
        problems="$problems  - compose.nodes-tor.yaml (local nodes over Tor) needs APP_MODE=tor and compose.tor.yaml"$'\n'
      fi
      compose_after compose.nodes-tor.yaml compose.nodes.yaml ||
        problems="$problems  - compose.nodes-tor.yaml must come after compose.nodes.yaml in COMPOSE_FILE"$'\n'
    fi
  fi
  # The opt-in administrator onion must never fall back to a public descriptor when its allowlist is missing.
  # The overlay fixes the app socket path; .env may leave it blank, but may not name a different listener.
  value=$(env_value ADMIN_SOCKET)
  if compose_has compose.admin-tor.yaml; then
    if [[ $mode != tor ]] || ! compose_has compose.tor.yaml; then
      problems="$problems  - compose.admin-tor.yaml requires APP_MODE=tor and compose.tor.yaml"$'\n'
    fi
    compose_after compose.admin-tor.yaml compose.yaml ||
      problems="$problems  - compose.admin-tor.yaml must come after compose.yaml in COMPOSE_FILE"$'\n'
    if [[ -n $value && $value != /run/market-admin/http.sock ]]; then
      problems="$problems  - ADMIN_SOCKET must be blank or /run/market-admin/http.sock with compose.admin-tor.yaml"$'\n'
    fi
    value=$(env_value TOR_ADMIN_AUTH_DIR)
    if [[ $value != /* ]] || ! sh deploy/tor-admin-entrypoint.sh "$value" --check 2>/dev/null; then
      problems="$problems  - TOR_ADMIN_AUTH_DIR must be an absolute readable directory containing at least one valid public .auth key; private administrator Tor startup would be refused"$'\n'
    fi
  elif [[ -n $value || -n $(env_value TOR_ADMIN_AUTH_DIR) ]]; then
    problems="$problems  - ADMIN_SOCKET and TOR_ADMIN_AUTH_DIR require compose.admin-tor.yaml in COMPOSE_FILE"$'\n'
  fi
  profiles=$(env_value COMPOSE_PROFILES)
  host=$(url_host "$database")
  if has_item "$profiles" internal-db; then
    if [[ -n $database && $host != db ]]; then
      problems="$problems  - COMPOSE_PROFILES runs the internal database (internal-db) but DATABASE_URL points elsewhere"$'\n'
    elif [[ -n $database && $(url_decode "$(url_password "$database")") != "$(env_value POSTGRES_PASSWORD)" ]]; then
      problems="$problems  - POSTGRES_PASSWORD differs from the password in DATABASE_URL, so the app cannot sign in to the internal database"$'\n'
    fi
  elif [[ $host == db ]]; then
    problems="$problems  - DATABASE_URL points at the internal database (host db) but COMPOSE_PROFILES lacks internal-db"$'\n'
  fi
  if has_item "$profiles" mirror && [[ $mode != tor ]]; then
    problems="$problems  - COMPOSE_PROFILES includes mirror, which needs APP_MODE=tor"$'\n'
  fi
  if has_item "$profiles" bitcoin; then
    value=$(env_value BITCOIN_CHAIN)
    case $value in
      ''|regtest|testnet4|signet|main) ;;
      *) problems="$problems  - BITCOIN_CHAIN must be regtest, testnet4, signet or main (mainnet, real funds) for the local escrow-support node"$'\n' ;;
    esac
    rpc_password=$(env_value BITCOIN_RPC_PASSWORD)
    rpc_auth=$(env_value BITCOIN_RPC_AUTH)
    if [[ -z $rpc_password ]]; then
      problems="$problems  - the local Bitcoin node (bitcoin profile) needs BITCOIN_RPC_PASSWORD"$'\n'
    fi
    if [[ -z $rpc_auth ]]; then
      problems="$problems  - the local Bitcoin node (bitcoin profile) needs BITCOIN_RPC_AUTH; run upgrade or generate a verifier from the existing password"$'\n'
    elif ! [[ $rpc_auth =~ $rpc_auth_pattern ]]; then
      problems="$problems  - BITCOIN_RPC_AUTH must be a marketplace salted HMAC verifier; do not place the password in it"$'\n'
    elif [[ -n $rpc_password ]]; then
      salt=${rpc_auth#marketplace:}
      salt=${salt%%\$*}
      expected_auth=$(bitcoin_rpcauth "$rpc_password" "$salt") ||
        die 'Could not check the local Bitcoin RPC verifier; nothing was started.'
      if [[ $rpc_auth != "$expected_auth" ]]; then
        problems="$problems  - BITCOIN_RPC_AUTH does not match BITCOIN_RPC_PASSWORD; repair the verifier before starting the local node"$'\n'
      fi
    fi
  fi
  if has_item "$profiles" monero; then
    value=$(env_value MONERO_NETWORK)
    case $value in
      ''|stagenet)
        if [[ $(env_value MONERO_NETWORK_FLAG) != --stagenet ]] && env_get MONERO_NETWORK_FLAG >/dev/null; then
          problems="$problems  - MONERO_NETWORK_FLAG must be --stagenet (or absent) for a stagenet daemon"$'\n'
        fi ;;
      mainnet)
        if ! env_get MONERO_NETWORK_FLAG >/dev/null || [[ -n $(env_value MONERO_NETWORK_FLAG) ]]; then
          problems="$problems  - MONERO_NETWORK=mainnet needs a blank MONERO_NETWORK_FLAG= line so the local daemon runs mainnet (monerod has no --mainnet)"$'\n'
        fi ;;
      *) problems="$problems  - MONERO_NETWORK must be stagenet or mainnet (real funds) for the local escrow-support daemon"$'\n' ;;
    esac
  fi
  # The app's errMixedNetworks (marketLiveFromEnv), whatever nodes run: a market is all mainnet or all test networks,
  # and a blank network takes the other coin's class.
  if mixed_networks "$(env_value BITCOIN_CHAIN)" "$(env_value MONERO_NETWORK)"; then
    problems="$problems  - BITCOIN_CHAIN and MONERO_NETWORK mix mainnet with a test network, so the app refuses to start: a market runs on mainnet or on test networks, never both. BITCOIN_CHAIN=main needs MONERO_NETWORK=mainnet or blank, and MONERO_NETWORK=mainnet needs BITCOIN_CHAIN=main or blank (docs/operator-guide.md#mainnet)"$'\n'
  fi
  if has_item "$profiles" monero-escrow-wallet && [[ -z $(env_value ESCROW_XMR_WALLET_RPC_PASSWORD) ]]; then
    problems="$problems  - the Monero escrow wallet (monero-escrow-wallet profile) needs ESCROW_XMR_WALLET_RPC_PASSWORD"$'\n'
  fi
  # Monero escrow in Tor mode (A-515): the app refuses any ESCROW_XMR_TOR_RELAY but blank or clearnet-accepted, and
  # holds settlement relays (relay_tx goes out from this host's IP address) unless the node Tor overlay is listed.
  if [[ $(env_value ESCROW_XMR) == on ]]; then
    value=$(env_value ESCROW_XMR_TOR_RELAY)
    case $value in
      ''|clearnet-accepted) ;;
      *) problems="$problems  - ESCROW_XMR_TOR_RELAY must be blank or clearnet-accepted"$'\n' ;;
    esac
    if [[ $mode == tor && -z $value ]] && ! compose_has compose.nodes-tor.yaml; then
      warnings="${warnings}warning: APP_MODE=tor with ESCROW_XMR=on: Monero escrow settlements are built but not relayed, because the escrow wallet-rpc would relay them from this host's IP address. Add compose.nodes-tor.yaml to COMPOSE_FILE (Monero over Tor is unverified) or set ESCROW_XMR_TOR_RELAY=clearnet-accepted; see docs/operator-guide.md#monero-escrow"$'\n'
    fi
  fi
  if [[ -n $files ]] && compose_has compose.escrow-xmr.yaml && ! [[ -f $escrow_seed_key_file && ! -L $escrow_seed_key_file ]]; then
    problems="$problems  - COMPOSE_FILE includes compose.escrow-xmr.yaml but $escrow_seed_key_file (the Monero escrow seed key it mounts) is missing"$'\n'
  fi
  for key in BITCOIN_IMAGE MONERO_IMAGE MONERO_WALLET_IMAGE; do
    value=$(env_value "$key")
    if [[ -n $value ]] && ! image_pinned "$value"; then
      problems="$problems  - $key must name a release tag or @sha256 digest, not latest (./scripts/install.sh upgrade pins the default images; see docs/operator-guide.md#node-image-versions)"$'\n'
    fi
  done
  # The malware scanner (A-184): the profile runs clamd, CLAMD_ADDRESS points the app at it, and neither works
  # without the other. The address is always clamd's own; a scanner elsewhere is not supported.
  value=$(env_value CLAMD_ADDRESS)
  if [[ -n $value && $value != "$clamd_address" ]]; then
    problems="$problems  - CLAMD_ADDRESS must be $clamd_address (the scanner in compose.scanner.yaml) or blank"$'\n'
  elif has_item "$profiles" scanner; then
    [[ -n $value ]] || problems="$problems  - the malware scanner (scanner profile) needs CLAMD_ADDRESS=$clamd_address; ./scripts/install.sh add-scanner sets both"$'\n'
  elif [[ -n $value ]]; then
    problems="$problems  - CLAMD_ADDRESS is set but COMPOSE_PROFILES lacks scanner, so uploads would wait for a scanner that never runs; ./scripts/install.sh add-scanner sets both"$'\n'
  fi
  if has_item "$profiles" scanner; then
    compose_has compose.scanner.yaml || problems="$problems  - the scanner profile needs compose.scanner.yaml in COMPOSE_FILE"$'\n'
    [[ -n $(find deploy/clamd.conf -perm -0004 2>/dev/null) ]] ||
      problems="$problems  - deploy/clamd.conf must be readable by every user (chmod 644 deploy/clamd.conf): clamd reads it as its own user"$'\n'
  fi
  value=$(env_value CLAMAV_IMAGE)
  if [[ -n $value ]] && ! clamav_image_ok "$value"; then
    problems="$problems  - CLAMAV_IMAGE must name a ClamAV release with signatures: a MAJOR.MINOR.PATCH tag (optionally -debian) or an @sha256 digest; latest, stable, unstable and 1.5 move to other releases and _base has no signatures (docs/operator-guide.md#photo-uploads-and-the-malware-scanner)"$'\n'
  fi
  value=$(env_value APP_PORT)
  if [[ -n $value ]] && ! { [[ $value =~ ^[0-9]{1,5}$ ]] && ((10#$value > 0 && 10#$value <= 65535)); }; then
    problems="$problems  - APP_PORT must be between 1 and 65535"$'\n'
  fi
  value=$(env_value COMPOSE_PROJECT_NAME)
  if [[ -n $value && ! $value =~ $project_pattern ]]; then
    problems="$problems  - COMPOSE_PROJECT_NAME must use lower-case letters, digits, hyphens and underscores"$'\n'
  fi
  if [[ -n $warnings ]]; then printf '%s' "$warnings" >&2; fi
  if [[ -n $problems ]]; then
    printf 'Configuration check failed (.env):\n%s' "$problems" >&2
    return 1
  fi
  # Compose names containers and volumes after the project, not the checkout (A-135).
  project=$(project_name)
  listing=$(all_containers) || { echo 'Could not list Docker containers (is the Docker daemon running?).' >&2; return 1; }
  while IFS= read -r entry; do
    [[ -n $entry ]] || continue
    entry_project=${entry%%|*}
    entry_dir=${entry#*|}
    if [[ $entry_project == "$project" ]]; then
      if same_dir "$entry_dir"; then
        :
      elif [[ -n $entry_dir && ! -e $entry_dir ]]; then
        moved="$moved  $entry_dir (it no longer exists)"$'\n'
      else
        foreign="$foreign  ${entry_dir:-(no working directory label)}"$'\n'
      fi
    elif same_dir "$entry_dir"; then
      elsewhere="$elsewhere  $entry_project"$'\n'
    fi
  done <<< "$listing"
  if [[ -n $foreign ]]; then
    printf "Configuration check failed: Compose project '%s' has containers created from another directory:\n" "$project" >&2
    printf '%s' "$foreign" | sort -u >&2
    echo 'Commands here would recreate them with this .env. Give this checkout its own COMPOSE_PROJECT_NAME.' >&2
    return 1
  fi
  if [[ -n $moved ]]; then
    printf "Configuration check failed: Compose project '%s' has containers created from a directory that no longer exists:\n" "$project" >&2
    printf '%s' "$moved" | sort -u >&2
    printf '%s\n' 'If this checkout (with its .env) was moved from there, recreate the containers from here so they record this' \
      'directory: docker compose up -d --force-recreate (it keeps the volumes and their data), then run this command again.' \
      'Keep COMPOSE_PROJECT_NAME as it is: another name starts the install on new, empty volumes.' >&2
    return 1
  fi
  if [[ -n $elsewhere ]]; then
    printf "Configuration check failed: containers created from this directory belong to another Compose project than '%s' (COMPOSE_PROJECT_NAME in .env):\n" "$project" >&2
    printf '%s' "$elsewhere" | sort -u >&2
    echo 'Changing the project name starts the install on new, empty volumes. Put the name those containers use back in .env.' >&2
    return 1
  fi
  docker compose config --quiet || { echo 'Configuration check failed: docker compose config rejected the configuration.' >&2; return 1; }
  if has_item "$profiles" scanner; then
    listing=$(docker compose config) || { echo 'Configuration check failed: docker compose config rejected the configuration.' >&2; return 1; }
    problems=$(scanner_layout_problems "$listing")
    if [[ -n $problems ]]; then
      printf 'Configuration check failed (the malware scanner, as the Compose files resolve it):\n%s\n' "$problems" >&2
      return 1
    fi
  fi
}

cmd_check() {
  require_env
  run_check || exit 1
  echo 'Configuration OK.'
}

# ---- init -------------------------------------------------------------------------------------------------

# ask VAR PROMPT [silent]: read one answer into VAR. End of input stops the installer before anything is written.
ask() {
  local answer
  if [[ ${3-} == silent ]]; then
    IFS= read -r -s -p "$2" answer || die '' 'No answer (end of input); nothing was written or started.'
    printf '\n'
  else
    read -r -p "$2" answer || die 'No answer (end of input); nothing was written or started.'
  fi
  printf -v "$1" '%s' "$answer"
}

# one_of OPTION VALUE ALLOWED...: VALUE when it is one of ALLOWED, else a usage error.
one_of() {
  local option=$1 value=$2 allowed
  shift 2
  for allowed in "$@"; do
    [[ $value != "$allowed" ]] || { printf '%s' "$value"; return 0; }
  done
  echo "$option takes one of: $*" >&2
  exit 2
}

# refuse_network_name COIN VALUE PREFIX: a usage error naming the right network when VALUE misnames one (as
# scripts/escrow-node-setup.sh does); PREFIX is the option that took it ('--bitcoin-network '), or nothing for an answer.
refuse_network_name() {
  local message=''
  case $1:$2 in
    BITCOIN:mainnet) message="Bitcoin Core names mainnet \"main\": give ${3}main." ;;
    BITCOIN:test|BITCOIN:testnet|BITCOIN:testnet3) message="testnet3 is not an escrow chain; give ${3}testnet4 or ${3}signet." ;;
    MONERO:main) message="Monero names mainnet \"mainnet\": give ${3}mainnet." ;;
    MONERO:testnet) message="Monero testnet is not an escrow network; give ${3}stagenet." ;;
  esac
  [[ -z $message ]] || { echo "Refusing: $message Nothing was written." >&2; exit 2; }
}

# check_image REFERENCE: a Docker image reference with a repository and optional tag or complete digest.
check_image() {
  local name=$1 digest
  if [[ $1 == *@* ]]; then
    name=${1%@*}
    digest=${1##*@}
    [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die 'Invalid image digest; use a complete @sha256: followed by 64 lowercase hexadecimal characters.'
  fi
  [[ $name =~ ^[[:alnum:]][[:alnum:]./:_-]*$ && $name != */ && $name != *: ]] ||
    die 'Invalid Docker image reference; include a repository name and optional tag or complete SHA-256 digest.'
}

# Write literal dotenv values to the new configuration; refuse quotes/newlines rather than evaluating input.
put() {
  [[ $2 != *"'"* && $2 != *$'\n'* && $2 != *$'\r'* ]] || { echo 'Values cannot contain single quotes or newlines.' >&2; exit 1; }
  printf "%s='%s'\n" "$1" "$2" >> "$config"
}

print_proxy_snippets() {
  cat <<EOF

Point your HTTPS reverse proxy at 127.0.0.1:$1 and forward the original Host header. Caddy:

  market.example.com {
      reverse_proxy 127.0.0.1:$1
  }

nginx (inside your server block for the domain, with its certificate):

  location / {
      proxy_pass http://127.0.0.1:$1;
      proxy_set_header Host \$host;
  }
EOF
}

cmd_init() {
  local bare=true local_mode=false mode='' database='' proxy='' nodes='' admin='' site_name='' site_given=false
  local btc='' xmr='' btc_image='' xmr_image='' btc_network='' xmr_network='' btc_prune='' xmr_prune=''
  local detail=false local_http='' password1='' password2='' uploads=''
  local xmr_escrow='' xmr_seed_key='' onion_dir='' onion_given='' onion_expected=''
  (($# == 0)) || bare=false
  while (($#)); do
    case $1 in
      --local) local_mode=true ;;
      --tor|--clearnet) [[ -z $mode ]] || usage; mode=${1#--} ;;
      --external-db|--behind-proxy|--nodes|--bitcoin|--monero|--bitcoin-image|--monero-image|--bitcoin-network|\
      --monero-network|--bitcoin-prune|--monero-prune|\
      --monero-escrow|--monero-escrow-seed-key|--admin|--site-name|--uploads|--onion-key-dir|--onion-address)
        (($# >= 2)) || usage
        case $1 in
          --external-db) database=$2; [[ -n $database ]] || usage ;;
          --behind-proxy) proxy=$(one_of "$1" "$2" yes no) ;;
          --nodes) nodes=$(one_of "$1" "$2" local none custom) ;;
          --bitcoin) btc=$(one_of "$1" "$2" local disabled) ;;
          --monero) xmr=$(one_of "$1" "$2" local disabled) ;;
          --bitcoin-image) btc_image=$2 ;;
          --monero-image) xmr_image=$2 ;;
          --bitcoin-network) refuse_network_name BITCOIN "$2" "$1 "; btc_network=$(one_of "$1" "$2" main signet testnet4 regtest) ;;
          --monero-network) refuse_network_name MONERO "$2" "$1 "; xmr_network=$(one_of "$1" "$2" mainnet stagenet) ;;
          --bitcoin-prune) btc_prune=$(one_of "$1" "$2" yes no) ;;
          --monero-prune) xmr_prune=$(one_of "$1" "$2" yes no) ;;
          --monero-escrow) xmr_escrow=$(one_of "$1" "$2" yes no) ;;
          --monero-escrow-seed-key) xmr_seed_key=$2; [[ -n $xmr_seed_key ]] || usage ;;
          --admin) admin=$2 ;;
          --site-name) site_name=$2; site_given=true ;;
          --uploads) uploads=$(one_of "$1" "$2" yes no) ;;
          --onion-key-dir) onion_dir=$2; [[ -n $onion_dir ]] || usage ;;
          --onion-address) onion_given=$(onion_address_option "$1" "$2") ;;
        esac
        shift ;;
      *) usage ;;
    esac
    shift
  done
  if [[ -n $xmr_seed_key && $xmr_escrow != yes ]]; then echo '--monero-escrow-seed-key needs --monero-escrow yes.' >&2; exit 2; fi
  if [[ -n $onion_given && -z $onion_dir ]]; then echo '--onion-address needs --onion-key-dir DIR.' >&2; exit 2; fi
  if [[ -n $onion_dir ]]; then
    # An onion identity is for a Tor install, which the option implies.
    if [[ $local_mode == true || $mode == clearnet ]]; then
      echo '--onion-key-dir imports an onion identity, so it needs a Tor install: it cannot be combined with --clearnet or --local.' >&2
      exit 2
    fi
    mode=tor
    if [[ -z $onion_given && ! -t 0 ]]; then
      echo 'Refusing: with no terminal to confirm the imported onion address on, give it as --onion-address ADDR.onion. Nothing was written.' >&2
      exit 2
    fi
  fi
  if [[ $local_mode == true && -n $mode$database$proxy$nodes$btc$xmr$btc_image$xmr_image$btc_network$xmr_network$btc_prune$xmr_prune$uploads$xmr_escrow ]]; then
    echo '--local answers every question itself; combine it only with --admin and --site-name.' >&2
    exit 2
  fi
  if [[ $site_given == true && -z $admin ]]; then echo '--site-name needs --admin HANDLE.' >&2; exit 2; fi
  if [[ -n $admin ]]; then
    [[ $admin =~ ^[A-Za-z0-9_]{3,32}$ ]] || { echo '--admin needs a 3-32 character handle (letters, digits, underscores).' >&2; exit 2; }
    site_name=$(printf '%s' "$site_name" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    [[ $site_name != *$'\n'* ]] && (($(printf '%s' "$site_name" | LC_ALL=C wc -c) <= 80)) ||
      { echo '--site-name must be at most 80 bytes on one line.' >&2; exit 2; }
  fi
  if [[ -n $btc$xmr && -z $nodes ]]; then nodes=custom; fi

  require_docker
  lock_env_or_die
  command -v openssl >/dev/null || die 'OpenSSL is required to generate secrets.'
  [[ ! -e .env ]] || die '.env already exists, so this checkout is installed. To bring it up to date after git pull, run ./scripts/install.sh upgrade; ./scripts/install.sh status shows its state. No secrets were overwritten.'
  # Compose names containers, volumes and networks after the project, not the checkout. A second checkout with
  # the same name would recreate another installation's containers with new secrets, and `docker compose down -v`
  # there would delete its volumes. Refuse unless everything under this name was created from this directory,
  # and never start on existing volumes (I10): they hold an installation whose .env is not this one.
  local project=${caller_project:-opsecmkt} second_copy owners foreign='' owner here volumes
  [[ $project =~ $project_pattern ]] || die 'COMPOSE_PROJECT_NAME must use lower-case letters, digits, hyphens and underscores, starting with a letter or digit.'
  second_copy="To install a second copy on this host, give it its own project name, for example: COMPOSE_PROJECT_NAME=${project}-rehearsal ./scripts/install.sh"
  if [[ $local_mode == true ]]; then second_copy="$second_copy --local"; fi
  # One line per container; the prefix keeps a container without the label from vanishing as an empty line.
  owners=$(docker ps -a --filter "label=com.docker.compose.project=$project" --format 'dir={{.Label "com.docker.compose.project.working_dir"}}') ||
    die 'Could not list Docker containers (is the Docker daemon running?); nothing was written or started.'
  if [[ -n $owners ]]; then
    here=$(pwd -P)
    while IFS= read -r owner; do
      owner=${owner#dir=}
      if [[ -z $owner ]]; then
        foreign="$foreign  (no working directory label)"$'\n'
      elif [[ $owner != "$PWD" && ! ( -d $owner && $(cd "$owner" 2>/dev/null && pwd -P) == "$here" ) ]]; then
        foreign="$foreign  $owner"$'\n'
      fi
    done <<< "$owners"
    if [[ -n $foreign ]]; then
      printf "Refusing to install: Compose project '%s' already has containers created from another directory:\n" "$project" >&2
      printf '%s' "$foreign" | sort -u >&2
      printf '%s\n' 'Installing here would recreate them with new secrets, and docker compose down -v here would delete their volumes. Nothing was written or started.' \
        "To manage that installation, work in its directory. $second_copy" >&2
      exit 1
    fi
  fi
  volumes=$(docker volume ls -q --filter "label=com.docker.compose.project=$project") ||
    die 'Could not list Docker volumes (is the Docker daemon running?); nothing was written or started.'
  if [[ -n $volumes ]]; then
    if [[ -n $owners ]]; then
      printf "Refusing to install: Compose project '%s' already has volumes, created by an earlier install from this directory:\n" "$project" >&2
    else
      printf "Refusing to install: Compose project '%s' has volumes but no containers, so the checkout that created them is unknown:\n" "$project" >&2
    fi
    printf '%s\n' "$volumes" | sed 's/^/  /' >&2
    printf '%s\n' "They may hold another installation's data: installing here would start on them with new secrets, and docker compose down -v here would delete them. Nothing was written or started." \
      "To use that data, start it from the checkout (and .env) that created it. $second_copy" >&2
    exit 1
  fi
  if [[ $uploads == yes ]]; then host_memory_check || exit 1; fi
  if [[ -n $onion_dir ]]; then
    # A-526: the key files first, before any question. A volume under the identity's name that the label check above
    # misses (one created by hand) is refused too: the key goes only into a volume this installer creates.
    onion_key_check "$onion_dir"
    ! docker volume inspect "${project}_tor_identity" >/dev/null 2>&1 ||
      die "Refusing: the volume ${project}_tor_identity already exists, and an onion identity is imported only into a new, empty one. Nothing was written."
  fi

  # Questions. The bare invocation asks today's questions; with options, only what they leave open.
  if [[ $local_mode == true ]]; then
    mode=clearnet
    nodes=none
    # The quick start always creates its own local database and disables external wallet connections.
    local_http=yes
  else
    if [[ -z $mode ]]; then ask mode 'Exposure (clearnet/tor) [clearnet]: '; fi
    mode=${mode:-clearnet}
    [[ $mode == clearnet || $mode == tor ]] || die 'Invalid mode'
    if [[ $bare == true ]]; then ask database 'External PostgreSQL URL (blank creates internal database): ' silent; fi
    if [[ -n $database && $database != postgres://* && $database != postgresql://* ]]; then die 'Use a postgres:// or postgresql:// database URL'; fi
    if [[ -n $database ]] && database_placeholder "$database"; then die 'The database URL still contains a placeholder such as REPLACE_ME; nothing was written.'; fi
    if [[ $mode == clearnet ]]; then
      if [[ $bare == true ]]; then
        ask local_http 'Local HTTP development only? (yes/no) [no]: '
      else
        if [[ -z $proxy ]]; then ask proxy 'Behind an HTTPS reverse proxy? (yes/no) [yes]: '; fi
        case ${proxy:-yes} in yes) local_http=no ;; no) local_http=yes ;; *) die 'Answer yes or no.' ;; esac
      fi
    fi
    if [[ $bare == true ]]; then
      nodes=custom
    elif [[ -z $nodes ]]; then
      printf '%s\n' "Nodes: 'local' (the default) runs pruned Bitcoin Core and Monero nodes in Docker on MAINNET (real funds) unless --bitcoin-network or --monero-network names a test network (the other coin then follows). Pruned, Bitcoin mainnet keeps about 30 GB and Monero mainnet 45 GB or more on disk, and their first sync takes days; on test networks (Bitcoin signet or testnet4, Monero stagenet) about 5-8 GB and 20 GB, synced in hours (all estimates). 'none' leaves payments off. 'custom' asks per coin."
      ask nodes 'Nodes (local/none/custom) [local]: '
      nodes=${nodes:-local}
      [[ $nodes == local || $nodes == none || $nodes == custom ]] || die 'Choose local, none or custom.'
    fi
  fi
  [[ $nodes != custom ]] || detail=true
  if [[ $mode == clearnet ]]; then
    [[ ${caller_app_port:-8080} =~ ^[0-9]{1,5}$ ]] && ((10#${caller_app_port:-8080} > 0 && 10#${caller_app_port:-8080} <= 65535)) ||
      die 'APP_PORT must be between 1 and 65535.'
  fi
  # Per-coin answers. local: pruned node with the default image on mainnet (A-523) unless a network is chosen; none:
  # disabled; custom: every question without an option is asked. The market's class follows an explicitly chosen
  # network (an option, or an answer): a test network for one coin makes the other default to its own (signet,
  # stagenet). A coin disabled by its option chooses nothing.
  local btc_explicit=false xmr_explicit=false
  if [[ -n $btc_network && $btc != disabled ]]; then btc_explicit=true; fi
  if [[ -n $xmr_network && $xmr != disabled ]]; then xmr_explicit=true; fi
  local coin choice image network prune default_image value
  local btc_choice='' xmr_choice='' bitcoin_chain='' monero_network='' btc_prune_mb='' xmr_prune_flags=$monero_prune_default
  for coin in BITCOIN MONERO; do
    if [[ $coin == BITCOIN ]]; then
      choice=$btc image=$btc_image network=$btc_network prune=$btc_prune default_image=$bitcoin_image_default
    else
      choice=$xmr image=$xmr_image network=$xmr_network prune=$xmr_prune default_image=$monero_image_default
    fi
    if [[ -z $choice ]]; then
      case $nodes in
        local) choice=local ;;
        none) choice=disabled ;;
        custom)
          # Sizes are upstream estimates (docs/operator-guide.md#local-node-pruning) and grow with each chain.
          case $coin in
            BITCOIN) printf '%s\n' "BITCOIN: 'local' (the default) runs a pruned Bitcoin Core node for operator-managed escrow setup. On mainnet (the default network) it keeps about 30 GB on disk (${bitcoin_mainnet_prune_mb} MiB of recent blocks plus about 14 GB of chain state) and its first sync downloads the whole chain, which takes days; on signet or testnet4 about 5-8 GB (${bitcoin_prune_default} MiB plus the chain state), synced in hours (all estimates). 'disabled' leaves it out." ;;
            MONERO) printf '%s\n' "MONERO: 'local' (the default) runs a pruned Monero daemon for operator-managed escrow setup. Pruning keeps about a third of the chain: on mainnet (the default network) 45 GB in April 2022 and more today, with a first sync of days; on stagenet keep 20 GB free, synced in hours (all estimates). 'disabled' leaves it out." ;;
          esac
          ask choice "$coin node (local/disabled) [local]: "
          choice=${choice:-local} ;;
      esac
    fi
    case $choice in
      disabled) ;;
      local)
        if [[ -z $image && $detail == true ]]; then ask image "$coin Docker image [$default_image; blank uses this default]: "; fi
        image=${image:-$default_image}
        check_image "$image"
        if [[ $coin == BITCOIN ]]; then
          value=main
          if [[ $xmr_explicit == true && $xmr_network == stagenet ]]; then value=signet; fi
          if [[ -z $network && $detail == true ]]; then ask network "BITCOIN network (main = mainnet, real funds; or signet/testnet4/regtest) [$value]: "; fi
          refuse_network_name BITCOIN "$network" ''
          if [[ -n $network ]]; then btc_explicit=true btc_network=$network; fi
          case ${network:-$value} in
            main|signet|testnet4|regtest) network=${network:-$value} ;;
            *) die 'Choose main, signet, testnet4 or regtest.' ;;
          esac
          if [[ -z $prune && $detail == true ]]; then ask prune 'Prune the BITCOIN node to save disk? (yes/no) [yes]: '; fi
        else
          value=mainnet
          if [[ $btc_explicit == true && $btc_network != main ]]; then value=stagenet; fi
          if [[ -z $network && $detail == true ]]; then ask network "MONERO network (mainnet = real funds; or stagenet) [$value]: "; fi
          refuse_network_name MONERO "$network" ''
          if [[ -n $network ]]; then xmr_explicit=true xmr_network=$network; fi
          case ${network:-$value} in
            mainnet|stagenet) network=${network:-$value} ;;
            *) die 'Choose mainnet or stagenet.' ;;
          esac
          if [[ -z $prune && $detail == true ]]; then ask prune 'Prune the MONERO node to save disk? (yes/no) [yes]: '; fi
        fi
        case ${prune:-yes} in yes|no) prune=${prune:-yes} ;; *) die 'Answer yes or no.' ;; esac
        ;;
      *) die 'Invalid node choice' ;;
    esac
    if [[ $coin == BITCOIN ]]; then
      btc_choice=$choice btc_image=$image
      if [[ $choice == local ]]; then bitcoin_chain=$network btc_prune=$prune; fi
    else
      xmr_choice=$choice xmr_image=$image
      if [[ $choice == local ]]; then monero_network=$network; if [[ $prune == no ]]; then xmr_prune_flags=''; fi; fi
    fi
  done
  # A market is all mainnet or all test networks (the app's errMixedNetworks). Two chosen networks that conflict are
  # refused here, before anything else is asked or written. A defaulted one follows the chosen one: Bitcoin's default,
  # asked first, can only conflict with a Monero network answered after it.
  if mixed_networks "$bitcoin_chain" "$monero_network"; then
    if [[ $btc_explicit == true && $xmr_explicit == true ]]; then
      die "Refusing: --bitcoin-network $bitcoin_chain and --monero-network $monero_network mix mainnet with a test network. A market runs on mainnet or on test networks, never both (the app refuses to start otherwise)." \
        'Give both test networks (--bitcoin-network signet|testnet4|regtest with --monero-network stagenet), both mainnet (main, mainnet), or disable one coin. Nothing was written.'
    elif [[ $btc_explicit == false ]]; then
      if [[ $monero_network == mainnet ]]; then bitcoin_chain=main; else bitcoin_chain=signet; fi
      echo "BITCOIN follows the Monero network's class: $bitcoin_chain (--bitcoin-network chooses another)."
    else
      if [[ $bitcoin_chain == main ]]; then monero_network=mainnet; else monero_network=stagenet; fi
      echo "MONERO follows the Bitcoin network's class: $monero_network (--monero-network chooses another)."
    fi
  fi
  if [[ $btc_choice == local ]]; then
    btc_prune_mb=$bitcoin_prune_default
    if [[ $bitcoin_chain == main ]]; then btc_prune_mb=$bitcoin_mainnet_prune_mb; fi
    if [[ $btc_prune == no ]]; then btc_prune_mb=0; fi
  fi
  # Monero escrow (A-449): only beside the local Monero node, on stagenet or (A-523) mainnet, whose daemon its own
  # wallet-RPC service (monero-escrow-wallet) shares. Off unless chosen; asked where the Monero questions are asked.
  # Its secret and its wallet-RPC password are generated below; the seed key (a public key whose private half the
  # operator keeps offline) is asked for, never defaulted.
  local xmr_funds='these coins'
  if [[ $monero_network == mainnet ]]; then xmr_funds='these coins: real funds on mainnet'; fi
  if [[ -z $xmr_escrow && $detail == true && $xmr_choice == local ]]; then
    printf '%s\n' "MONERO escrow (off by default): each order paid by Monero escrow gets an account of its own in a monthly escrow wallet on $monero_network, run by a separate wallet-RPC service. It is custodial: the market holds every escrow wallet's keys and can move $xmr_funds. It needs an OpenPGP public key whose private half you keep offline; every wallet seed is sealed to it (docs/operator-guide.md#monero-escrow)."
    ask xmr_escrow 'Turn on Monero escrow? (yes/no) [no]: '
  fi
  case ${xmr_escrow:-no} in yes|no) xmr_escrow=${xmr_escrow:-no} ;; *) die 'Answer yes or no.' ;; esac
  if [[ $xmr_escrow == yes ]]; then
    [[ $xmr_choice == local ]] ||
      die 'Monero escrow needs the local Monero node (--monero local). Nothing was written.'
    if [[ -z $xmr_seed_key ]]; then ask xmr_seed_key 'Path of the armored OpenPGP public key every escrow wallet seed is sealed to: '; fi
    [[ -n $xmr_seed_key ]] || die 'Monero escrow needs the OpenPGP public key file its wallet seeds are sealed to. Nothing was written.'
    [[ -f $xmr_seed_key && -r $xmr_seed_key ]] || die 'The Monero escrow seed key file does not exist or cannot be read. Nothing was written.'
    (($(LC_ALL=C wc -c < "$xmr_seed_key") <= 65536)) || die 'The Monero escrow seed key file is larger than 64 KiB. Nothing was written.'
    ! grep -q 'PRIVATE KEY BLOCK' "$xmr_seed_key" ||
      die 'That file holds a private key: give the public key only, and keep the private key offline. Nothing was written.'
    grep -q -- '-----BEGIN PGP PUBLIC KEY BLOCK-----' "$xmr_seed_key" ||
      die 'The Monero escrow seed key file is not an armored OpenPGP public key (-----BEGIN PGP PUBLIC KEY BLOCK-----). Nothing was written.'
  fi
  if [[ -n $admin ]]; then
    # Asked last, before anything is written or built. Kept in an unexported variable: it reaches the server
    # only through a pipe on stdin, never argv, the environment, the output or .env.
    IFS= read -r -s -p "Password for administrator $admin (12-72 bytes, not shown): " password1 || die '' 'No password entered; nothing was written or started.'
    printf '\n'
    IFS= read -r -s -p 'Repeat the password: ' password2 || die '' 'No password entered; nothing was written or started.'
    printf '\n'
    [[ $password1 == "$password2" ]] || die 'The passwords do not match; nothing was written or started.'
    local bytes
    # Count bytes directly, regardless of Bash's multibyte locale handling. The password stays on stdin and is
    # never exposed as a command argument.
    bytes=$(printf '%s' "$password1" | LC_ALL=C wc -c)
    ((bytes >= 12 && bytes <= 72)) || die 'Use a password of 12-72 bytes; nothing was written or started.'
    # QA5W5B-7: -bootstrap-admin refuses a password that is not valid UTF-8 (typed in a terminal set to LATIN1, for one),
    # which the sign-in form could never send; refused here, before .env is written or anything started. The pattern is
    # well-formed UTF-8 byte by byte (RFC 3629: no overlong form, surrogate or code point above U+10FFFF), matched in the
    # C locale. It admits no NUL byte, and none can arrive: bash's read drops it.
    # Match ASCII hex rather than raw high bytes: BSD regex rejects the latter
    # even in the C locale. The same RFC 3629 boundaries remain enforced.
    local password_bytes utf8_text
    password_bytes=$(printf '%s' "$password1" | od -An -v -tx1 | tr -d ' \n')
    utf8_text='^((0[1-9a-f]|[1-7][0-9a-f])|(c[2-9a-f]|d[0-9a-f])[89ab][0-9a-f]|e0[ab][0-9a-f][89ab][0-9a-f]|e[1-9abcef]([89ab][0-9a-f]){2}|ed[89][0-9a-f][89ab][0-9a-f]|f0[9ab][0-9a-f]([89ab][0-9a-f]){2}|f[123]([89ab][0-9a-f]){3}|f48[0-9a-f]([89ab][0-9a-f]){2})*$'
    (LC_ALL=C; [[ $password_bytes =~ $utf8_text ]]) ||
      die 'The password is not valid UTF-8 (is the terminal set to another encoding, such as LATIN1?); nothing was written or started.'
    password2=''
  fi

  # Secrets and the configuration.
  local password app_key setup_token audit_key rpc_password rpc_auth profiles='' external_egress=false secure=true
  local external_database=false
  local compose_files="compose.yaml:compose.${mode}.yaml:compose.nodes.yaml"
  password=$(secret 24) || exit 1
  # Distinct roots: APP_SECRET_KEY derives the CSRF/CAPTCHA and sealing keys; SETUP_TOKEN only opens /setup.
  app_key=$(secret 32) || exit 1
  setup_token=$(secret 32) || exit 1
  # Ed25519 seed for signed audit exports.
  audit_key=$(secret 32) || exit 1
  if [[ $btc_choice == local ]]; then
    rpc_password=$(secret 24) || exit 1
    rpc_auth=$(bitcoin_rpcauth "$rpc_password") || die 'Could not generate the Bitcoin RPC verifier; nothing was written or started.'
  fi
  local escrow_wallet_password='' wallet_secret=''
  if [[ $xmr_escrow == yes ]]; then
    escrow_wallet_password=$(secret 24) || exit 1
    # MONERO_WALLET_SECRET: every escrow wallet's password derives from it; 64 hex characters, unlike SETUP_TOKEN and
    # APP_SECRET_KEY (the app refuses it equal to either).
    wallet_secret=$(secret 32) || exit 1
  fi
  if [[ -z $database ]]; then
    database="postgres://opsecmkt:${password}@db:5432/opsecmkt?sslmode=disable"
    profiles=internal-db
    compose_files="${compose_files}:compose.internal-db.yaml"
  else
    external_egress=true
    external_database=true
  fi
  if [[ $mode == tor || $local_http == yes ]]; then secure=false; fi
  # A-523: before .env is written, a mainnet install says what that means, per coin.
  if [[ $bitcoin_chain == main || $monero_network == mainnet ]]; then
    printf '\n%s\n' "MAINNET: the local nodes run on mainnet, so this market's payments move real funds. No mainnet run of this software is recorded: mainnet is covered by unit, database and simulated-RPC tests only (docs/operator-guide.md#mainnet). For a test market, start again with --bitcoin-network signet|testnet4|regtest and --monero-network stagenet."
    if [[ $bitcoin_chain == main && $btc_prune_mb != 0 ]]; then
      echo "Bitcoin mainnet, pruned: about 30 GB on disk ($btc_prune_mb MiB of recent blocks plus about 14 GB of chain state; a full node is about 856 GB of blocks + 14 GB, Bitcoin Core v31.1's estimates). Its first sync downloads the whole chain and takes days (an estimate)."
      echo 'Prune window: the node keeps about two months of blocks (an estimate). A wallet restore or backup older than that cannot be rescanned on it: restore the whole bitcoin_data volume instead, or run an unpruned node (docs/operator-guide.md#local-node-pruning).'
    elif [[ $bitcoin_chain == main ]]; then
      echo "Bitcoin mainnet, full: about 870 GB on disk (856 GB of blocks + 14 GB of chain state, Bitcoin Core v31.1's estimates). Its first sync takes days (an estimate)."
    fi
    if [[ $monero_network == mainnet ]]; then
      if [[ -n $xmr_prune_flags ]]; then
        echo 'Monero mainnet, pruned: 45 GB in April 2022 (the Monero README), more today (an estimate). Its first sync takes days (an estimate).'
      else
        echo 'Monero mainnet, full: 130 GB in April 2022 (the Monero README), more today (an estimate). Its first sync takes days (an estimate).'
      fi
      if [[ $xmr_escrow == yes ]]; then
        echo "Monero escrow is on, and on mainnet it is custodial with real funds: the market holds every escrow wallet's keys (docs/operator-guide.md#monero-escrow)."
      else
        echo "Monero escrow stays off. On mainnet it is custodial with real funds: the market would hold every escrow wallet's keys (docs/operator-guide.md#monero-escrow)."
      fi
    fi
    if [[ $mode == tor ]]; then
      echo "Tor: the local nodes' peer-to-peer traffic is not routed through Tor unless compose.nodes-tor.yaml is added to COMPOSE_FILE (docs/operator-guide.md#local-nodes-over-tor); without it, their peers see this host's IP address."
    fi
  fi
  # The local nodes' size on disk, per chain (estimates; docs/operator-guide.md#local-node-pruning), checked against
  # Docker's free space. A regtest node holds only what it mines.
  local disk_gb=0 disk_text=''
  if [[ $btc_choice == local && $bitcoin_chain != regtest ]]; then
    case $bitcoin_chain:$btc_prune_mb in
      main:0) disk_gb=870 ;;
      main:*) disk_gb=30 ;;
      signet:0) disk_gb=28 ;;
      testnet4:0) disk_gb=33 ;;
      signet:*) disk_gb=8 ;;
      *) disk_gb=5 ;;
    esac
    disk_text="Bitcoin ${bitcoin_chain/#main/mainnet} $([[ $btc_prune_mb == 0 ]] && echo full || echo pruned) about $disk_gb GB"
  fi
  if [[ $xmr_choice == local ]]; then
    case $monero_network:$xmr_prune_flags in
      mainnet:) disk_gb=$((disk_gb + 130)) disk_text="${disk_text:+$disk_text, }Monero mainnet full 130 GB or more" ;;
      mainnet:*) disk_gb=$((disk_gb + 45)) disk_text="${disk_text:+$disk_text, }Monero mainnet pruned 45 GB or more" ;;
      *:) disk_gb=$((disk_gb + 60)) disk_text="${disk_text:+$disk_text, }Monero stagenet full about 60 GB" ;;
      *) disk_gb=$((disk_gb + 20)) disk_text="${disk_text:+$disk_text, }Monero stagenet pruned about 20 GB" ;;
    esac
  fi
  if ((disk_gb > 0)); then
    echo "Disk for the local nodes (estimates): $disk_text; about $disk_gb GB together."
    node_disk_check "$disk_gb"
  fi
  config=$(mktemp .env.install.XXXXXX)
  temporaries="$temporaries $config"
  put DATABASE_URL "$database"
  put POSTGRES_PASSWORD "$password"
  put APP_SECRET_KEY "$app_key"
  put SETUP_TOKEN "$setup_token"
  put APP_MODE "$mode"
  put COOKIE_SECURE "$secure"
  put APP_PORT "${caller_app_port:-8080}"
  # Remember the image this installer manages as its default. Future upgrades can advance it without rewriting
  # an operator-selected ClamAV image.
  put CLAMAV_IMAGE_DEFAULT "$clamav_image_default"
  # Every later docker compose command in this directory reads the project name from .env. Never change it on an
  # existing install: Compose would start it on new, empty volumes.
  put COMPOSE_PROJECT_NAME "$project"
  # These Compose mappings remain blank so a copied legacy .env reaches the guard instead of being ignored.
  put BITCOIN_RPC_URL ''
  put BITCOIN_WALLET ''
  put MONERO_RPC_URL ''
  put MONERO_WALLET_RPC_URL ''
  put AUDIT_SIGNING_KEY "$audit_key"
  # Network pins are for local escrow-support nodes; enabling the nodes alone never turns payments on.
  case $btc_choice in
    local)
      put BITCOIN_IMAGE "$btc_image"
      # A local node never runs without a generated RPC password (secret exits otherwise).
      put BITCOIN_RPC_PASSWORD "$rpc_password"
      put BITCOIN_RPC_AUTH "$rpc_auth"
      if [[ $btc_prune_mb == 0 ]]; then
        case $bitcoin_chain in
          main) value='about 870 GB on mainnet (856 GB of blocks + 14 GB of chain state)' ;;
          signet) value='about 28 GB on signet' ;;
          testnet4) value='about 33 GB on testnet4' ;;
          *) value='on regtest only the blocks you mine' ;;
        esac
        echo "Full node: BITCOIN keeps every block, $value today (Bitcoin Core v31.1's estimates, growing), with the same first sync. BITCOIN_PRUNE_MB=0 in .env; see docs/operator-guide.md#local-node-pruning to change it later."
      fi
      put BITCOIN_PRUNE_MB "$btc_prune_mb"
      profiles="${profiles:+$profiles,}bitcoin" ;;
  esac
  case $xmr_choice in
    local)
      put MONERO_IMAGE "$xmr_image"
      if [[ -z $xmr_prune_flags ]]; then
        value='on stagenet (upstream publishes no stagenet size)'
        if [[ $monero_network == mainnet ]]; then value='on mainnet (130 GB in April 2022, more today)'; fi
        echo "Full node: MONERO keeps the whole chain, about three times the pruned size $value, with a longer first sync (estimates). MONERO_PRUNE_FLAGS is blank in .env; see docs/operator-guide.md#local-node-pruning to change it later."
      fi
      put MONERO_PRUNE_FLAGS "$xmr_prune_flags"
      put MONERO_WALLET_IMAGE "${caller_wallet_image:-$monero_wallet_image_default}"
      profiles="${profiles:+$profiles,}monero" ;;
  esac
  if [[ $xmr_escrow == yes ]]; then
    put ESCROW_XMR on
    put ESCROW_XMR_WALLET_RPC_PASSWORD "$escrow_wallet_password"
    put ESCROW_XMR_WALLET_RPC_URL "http://escrow:${escrow_wallet_password}@monero-escrow-wallet:18083"
    put ESCROW_XMR_DAEMON_URL 'http://monero:18081'
    put ESCROW_XMR_SEED_KEY "$xmr_seed_key_path"
    put MONERO_WALLET_SECRET "$wallet_secret"
    profiles="${profiles:+$profiles,}monero-escrow-wallet"
    compose_files="${compose_files}:compose.escrow-xmr.yaml"
    value=''
    if [[ $monero_network == mainnet ]]; then value=' It holds real funds.'; fi
    echo "Monero escrow is on ($monero_network, custodial: the market holds every escrow wallet's keys).$value Back up .env (MONERO_WALLET_SECRET) and the monero_escrow_wallet volume; keep the seed key's private half offline (docs/operator-guide.md#monero-escrow)."
  fi
  # Written explicitly (A-523): blank keeps Compose's test-network fallbacks, so a mainnet install never relies on it.
  put BITCOIN_CHAIN "$bitcoin_chain"
  if [[ $bitcoin_chain == main ]]; then put PAYMENT_CONFIRMATIONS_BTC "$bitcoin_mainnet_confirmations"; fi
  put MONERO_NETWORK "$monero_network"
  # The bundled Monero services' network flag: a blank line runs mainnet (monerod has no --mainnet), --stagenet stagenet.
  if [[ $xmr_choice == local ]]; then
    if [[ $monero_network == mainnet ]]; then put MONERO_NETWORK_FLAG ''; else put MONERO_NETWORK_FLAG --stagenet; fi
  fi
  if [[ ,$profiles, == *,bitcoin,* ]] && ! [[ $(env_get BITCOIN_RPC_PASSWORD "$config") =~ ^[0-9a-f]{48}$ ]]; then
    die 'Refusing to enable the local Bitcoin node without a generated RPC password.'
  fi
  if [[ $mode == tor && $external_egress == true ]]; then
    compose_files="${compose_files}:compose.external-egress.yaml"
    echo 'External database selected: app direct network egress is enabled; only inbound exposure is Tor-only.'
  fi
  # Photo uploads: the malware scanner, its profile and the app's address for it (off unless --uploads yes).
  if [[ $uploads == yes ]]; then
    put CLAMAV_IMAGE "$clamav_image_default"
    put CLAMD_ADDRESS "$clamd_address"
    profiles="${profiles:+$profiles,}scanner"
    compose_files="${compose_files}:compose.scanner.yaml"
  fi
  put COMPOSE_FILE "$compose_files"
  put COMPOSE_PROFILES "$profiles"
  if [[ -n $onion_dir ]]; then
    # A-526: Tor derives the key's address offline, in the image Compose builds for the tor service (built here from the
    # configuration about to become .env when it is absent), before .env is written or anything starts.
    if ! docker image inspect "$project-tor" >/dev/null 2>&1; then
      docker compose --env-file "$config" build tor || die 'Could not build the Tor image (the message above says why). Nothing was written or started.'
    fi
    onion_expected=$(onion_dry_run "$project-tor") || exit 1
    onion_confirm "$onion_expected" "$onion_given"
  fi
  if [[ $xmr_escrow == yes ]]; then
    # A public key: readable by the app's user through the read-only mount (compose.escrow-xmr.yaml).
    cp -- "$xmr_seed_key" "$escrow_seed_key_file" && chmod 0644 "$escrow_seed_key_file" ||
      die 'Could not copy the Monero escrow seed key beside compose.escrow-xmr.yaml; nothing was started.'
  fi
  mv "$config" .env
  run_check || die 'Nothing was started; .env was kept for diagnosis. Fix it (or remove it and run the installer again).'
  if [[ $external_database == true ]]; then
    # An external database may already hold a market: starting on it with new secrets would lock its users out
    # of their second factors and orphan its sealed data. Read it first, with a one-off container.
    local probe
    probe=$(docker compose run --rm -T --no-deps app -status) && status_line_ok "$probe" ||
      die 'Could not read the external database with DATABASE_URL (the message above says why). Nothing was started; .env was kept for diagnosis. Fix it (or remove it and run the installer again).'
    if [[ $probe == installed=true\ * ]]; then
      rm -f .env
      die 'Refusing to install: the external database already belongs to a market (its setup is complete).' \
        "A new .env would start it with new secrets. To run that market from this checkout, restore that market's .env here" \
        'instead (docs/operator-guide.md#secrets-env-and-onion-identities); to start a new market, give --external-db an empty database.' \
        'The new .env was removed and nothing was started.'
    fi
  fi
  if [[ -n $onion_dir ]]; then
    # A-526: into the identity's volume, created here, before Tor first starts.
    onion_volume_create "$project" tor_identity || die "Could not create the volume ${project}_tor_identity. Nothing was started."
    onion_import "$project-tor" "${project}_tor_identity" ||
      die "Could not import the onion key into the volume ${project}_tor_identity (the message above says why). Nothing was started." \
        "To try again, remove the volume (docker volume rm ${project}_tor_identity) and .env."
  fi
  # Builds, starts and waits until every service's healthcheck passes: the app answers /healthz (it reaches
  # PostgreSQL and has migrated) and each Tor service has written its onion address.
  up_wait --build ||
    die 'The services did not become healthy. Inspect docker compose ps and docker compose logs app; .env and data were kept.'
  local onion='' base
  if [[ $mode == tor ]]; then
    onion=$(wait_onion tor) || onion=''
    if [[ -n $onion_expected && $onion != "$onion_expected" ]]; then
      docker compose stop tor || true
      die "Refusing: Tor serves ${onion:-no onion address}, not the imported $onion_expected, so the tor service was stopped." \
        'Inspect docker compose logs tor; .env and data were kept.'
    fi
    base="http://${onion:-YOUR-ONION-ADDRESS}"
  elif [[ $secure == false ]]; then
    base="http://127.0.0.1:${caller_app_port:-8080}"
  else
    base='https://YOUR-DOMAIN'
  fi
  # clamd was started without waiting (up_wait): give it the same bounded wait as add-scanner, but a scanner
  # that does not answer leaves the rest of the install to finish.
  local scanner=''
  if [[ $uploads == yes ]]; then
    echo 'Waiting for clamd to load its signatures (a minute or two; at most 10 minutes).'
    if scanner=$(wait_clamd); then
      scanner="signatures $scanner. Refresh them at least monthly with ./scripts/install.sh refresh-scanner."
    else
      scanner='not answering after 10 minutes, so photo uploads are refused until it does. Inspect docker compose logs clamd; ./scripts/install.sh status checks it again.'
    fi
  fi
  if [[ -n $admin ]]; then
    local args=(-bootstrap-admin "$admin")
    if [[ -n $site_name ]]; then args+=(-site-name "$site_name"); fi
    # printf is a shell builtin: the password is written to the pipe without appearing in any argv.
    if ! printf '%s\n' "$password1" | docker compose run --rm -T app "${args[@]}"; then
      password1=''
      die 'Creating the administrator failed (the message above says why); the market is running and not yet set up.' \
        'Finish in the browser with the token from ./scripts/install.sh setup-token, or run' \
        "  docker compose run --rm app -bootstrap-admin $admin"
    fi
    password1=''
    # The setup token opened nothing and now grants nothing; drop it and recreate the app without it.
    env_unset SETUP_TOKEN
    up_wait || die 'The app did not become healthy after setup. Inspect docker compose logs app.'
  fi

  # Final block: the only place init prints a secret (SETUP_TOKEN, when no --admin was given).
  if [[ -n $scanner ]]; then
    printf 'Malware scanner: %s\n' "$scanner"
  fi
  if [[ $mode == clearnet && $secure == true ]]; then
    printf 'Application healthy at http://127.0.0.1:%s. Secure cookies require HTTPS, so finish through your reverse proxy.\n' "${caller_app_port:-8080}"
    print_proxy_snippets "${caller_app_port:-8080}"
  fi
  echo
  if [[ $mode == tor ]]; then
    if [[ -n $onion ]]; then
      printf 'Onion address: %s\n' "$onion"
      echo 'Its descriptor can take 1-3 more minutes to reach the Tor network.'
    else
      echo 'Onion address: not written yet. Run ./scripts/install.sh status in a minute to see it.'
    fi
  fi
  if [[ -n $admin ]]; then
    printf 'Ready. Administrator %s is created; sign in at %s/login.\n' "$admin" "$base"
  else
    printf 'Ready. Open %s/setup%s to name your marketplace and create its administrator.\n' "$base" "$([[ $mode == tor ]] && echo ' in Tor Browser')"
    printf 'Setup token: %s\n' "$setup_token"
    echo 'Paste it on that page. It works once; after setup it grants nothing. While setup is pending,'
    echo './scripts/install.sh setup-token shows it again.'
  fi
  echo
  echo 'Next: back up .env (it holds every secret) and, on Tor, the onion keys: docs/operator-guide.md#secrets-env-and-onion-identities.'
  echo 'Check the installation any time with ./scripts/install.sh status.'
  if [[ -n $onion_expected ]]; then
    echo
    echo 'Back up the imported onion keys now: they are its address. In this directory, with age installed, run'
    echo '  identity=tor_identity AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT'
    echo 'and then the "Back up an onion identity" commands in docs/operator-guide.md#secrets-env-and-onion-identities.'
    onion_import_notes "$onion_dir"
  fi
  # A local Bitcoin node is infrastructure only (D-58): say how escrow is turned on, and change nothing for it.
  if [[ $btc_choice == local ]]; then
    local escrow_chain=${bitcoin_chain:-signet}
    echo
    echo 'Bitcoin escrow stays off until you set it up; this installer edits no file for it. Run these in this directory, once the local Bitcoin'
    echo 'node has synced its chain (a regtest node is ready at once). Lines starting with # are comments, safe to paste.'
    if [[ $escrow_chain == main ]]; then
      echo '# Mainnet, real funds (docs/operator-guide.md#mainnet): a plain fee address is refused on mainnet, so with a market fee above 0 give'
      echo '# ESCROW_BTC_FEE_ADDRESS the fee wallet'"'"'s watch-only receiving descriptor, wpkh([fingerprint/path]xpub/0/*), and never a test wallet'"'"'s.'
      echo '# The fee rate (ESCROW_BTC_FEE_RATE) and confirmation (PAYMENT_CONFIRMATIONS_BTC) defaults were sized on test chains; review them first.'
    fi
    echo '# 1. Wallets and RPC users; prints bitcoin.conf and .env lines once and writes no file. Copy its output.'
    echo "./scripts/escrow-node-setup.sh --chain $escrow_chain --rpc-port 8332 --signer --cli 'docker compose exec -T -u bitcoin bitcoin bitcoin-cli -datadir=/data -rpcport=8332'"
    echo '# 2. Append its rpcauth=, rpcwhitelistdefault= and rpcwhitelist= lines to the node'"'"'s bitcoin.conf (quoted EOF keeps the $ in rpcauth literal):'
    echo "docker compose exec -T -u bitcoin bitcoin sh -c 'cat >> /data/bitcoin.conf' <<'EOF'"
    echo '# paste the printed rpcauth, rpcwhitelistdefault and rpcwhitelist lines here'
    echo 'EOF'
    echo '# 3. Append its ESCROW_* lines to .env, turn escrow on, and add the signer service. If .env already has any of these keys, the last line wins.'
    echo "cat >> .env <<'EOF'"
    echo 'ESCROW_BTC=on'
    echo "COMPOSE_PROFILES=$profiles,escrow-signer"
    echo '# paste the printed ESCROW_* lines here'
    echo 'EOF'
    echo '# 4. Restart the node, then check it from the Compose network (the bundled node publishes no port). It must print PASS.'
    echo 'docker compose restart bitcoin'
    echo "ESCROW_BTC_RPC_URL='<the URL it printed>' ./scripts/escrow-node-setup.sh --chain $escrow_chain --verify --via-compose app --wait 120"
    echo '# 5. Make the escrow signer'"'"'s seed (write down the fingerprint it prints, then back the seed up), and start everything with the new settings.'
    echo 'docker compose run --rm --no-deps escrow-signer init'
    echo 'docker compose up -d'
    echo 'Details: docs/operator-guide.md#escrow-node-set-up-and-the-public-network-smoke'
  fi
}

# ---- status, setup-token, add-mirror ---------------------------------------------------------------------

cmd_status() {
  require_env
  local mode state='' known=true profiles address version
  mode=$(env_value APP_MODE)
  mode=${mode:-clearnet}
  profiles=$(env_value COMPOSE_PROFILES)
  printf 'Mode: %s (Compose project %s)\n' "$mode" "$(project_name)"
  echo 'Services:'
  docker compose ps -a --format '  {{.Service}}: {{.Status}}' || echo '  (could not list them; is the Docker daemon running?)'
  if state=$(app_status); then
    if [[ $state == installed=true* ]]; then
      echo 'Setup: complete'
      if ! env_get SETUP_TOKEN >/dev/null; then
        :
      elif ! lock_env; then
        echo 'SETUP_TOKEN stays in .env for now: another installer command is changing .env (.env.lock exists).'
      elif retire_setup_token "$state"; then
        echo 'The running app drops it the next time it is recreated (docker compose up -d).'
      fi
    else
      echo 'Setup: pending. ./scripts/install.sh setup-token shows the token for the setup page.'
    fi
    printf 'Schema: %s\n' "${state##*schema=}"
  else
    known=false
    echo 'Setup: unknown (the app could not read its database; see docker compose logs app)'
  fi
  if [[ $mode == tor ]]; then
    echo 'Onion addresses:'
    address=$(onion_address tor) || address='(not available: is tor running?)'
    printf '  main:   %s\n' "$address"
    if has_item "$profiles" mirror; then
      address=$(onion_address tor-mirror) || address='(not available: is tor-mirror running?)'
      printf '  mirror: %s\n' "$address"
    fi
  else
    address=$(env_value APP_PORT)
    printf 'App: http://127.0.0.1:%s (host loopback)\n' "${address:-8080}"
  fi
  if ! has_item "$profiles" scanner; then
    echo 'Malware scanner: off (photo uploads are off; ./scripts/install.sh add-scanner turns it on)'
  elif version=$(clamd_version); then
    printf 'Malware scanner: answering; signatures %s\n' "$version"
  else
    echo 'Malware scanner: not answering, so photo uploads are refused (still loading its signatures, or see docker compose logs clamd)'
  fi
  [[ $known == true ]]
}

# setup_page_hint: where the setup page is, for setup-token.
setup_page_hint() {
  local mode onion port
  mode=$(env_value APP_MODE)
  port=$(env_value APP_PORT)
  if [[ $mode == tor ]]; then
    if onion=$(onion_address tor); then printf 'http://%s/setup (Tor Browser)' "$onion"; else printf '/setup on your onion address (./scripts/install.sh status shows it)'; fi
  elif [[ $(env_value COOKIE_SECURE) == false ]]; then
    printf 'http://127.0.0.1:%s/setup' "${port:-8080}"
  else
    printf 'https://YOUR-DOMAIN/setup (through your HTTPS reverse proxy)'
  fi
}

cmd_setup_token() {
  local new=false state token key
  case $# in
    0) ;;
    1) [[ $1 == --new ]] || usage; new=true ;;
    *) usage ;;
  esac
  require_env
  if [[ $new == true ]]; then lock_env_or_die; fi
  state=$(app_status) || die 'Could not read the setup state: start the database and app (docker compose up -d) and try again.'
  [[ $state == installed=false* ]] || die 'Setup is complete, so there is no setup token: it grants nothing now. Sign in on /login.'
  if [[ $new == true ]]; then
    command -v openssl >/dev/null || die 'OpenSSL is required to generate secrets.'
    if [[ -z $(env_value APP_SECRET_KEY) ]]; then
      # SETUP_TOKEN was also the key root (a legacy install): keep the keys by copying it first.
      key=$(env_value SETUP_TOKEN)
      if [[ -z $key ]]; then key=$(secret 32) || exit 1; fi
      env_set APP_SECRET_KEY "$key"
      key=''
      echo 'APP_SECRET_KEY set (to the previous SETUP_TOKEN when there was one, so no key changes).'
    fi
    token=$(secret 32) || exit 1
    env_set SETUP_TOKEN "$token"
    echo 'New SETUP_TOKEN written to .env; restarting the app with it.'
    up_wait || die 'The app did not become healthy. Inspect docker compose logs app.'
  else
    token=$(env_value SETUP_TOKEN)
    [[ -n $token ]] || die 'There is no SETUP_TOKEN in .env. Issue one with ./scripts/install.sh setup-token --new.'
    key=$(env_value APP_SECRET_KEY)
    [[ -n $key && $token != "$key" ]] ||
      die 'Not shown: this SETUP_TOKEN is also the key root (APP_SECRET_KEY is missing or equal to it), and printing it would expose that key.' \
        'Issue a separate token with ./scripts/install.sh setup-token --new (it keeps the key).'
  fi
  printf 'Setup token: %s\n' "$token"
  printf 'Setup page: %s\n' "$(setup_page_hint)"
}

cmd_add_mirror() {
  local key_dir='' given='' expected='' project
  while (($#)); do
    case $1 in
      --mirror-onion-key-dir) (($# >= 2)) && [[ -n $2 ]] || usage; key_dir=$2 ;;
      --mirror-onion-address) (($# >= 2)) || usage; given=$(onion_address_option "$1" "$2") ;;
      *) usage ;;
    esac
    shift 2
  done
  if [[ -n $given && -z $key_dir ]]; then echo '--mirror-onion-address needs --mirror-onion-key-dir DIR.' >&2; exit 2; fi
  require_env
  legacy_pooled_guard || exit 1
  lock_env_or_die
  local profiles mirror main state
  [[ $(env_value APP_MODE) == tor ]] || die 'The onion mirror is a second Tor address, so it needs a Tor install (APP_MODE=tor). Nothing was changed.'
  run_check || exit 1
  profiles=$(env_value COMPOSE_PROFILES)
  if [[ -n $key_dir ]]; then
    # A-526: an existing identity for the mirror, imported before the mirror's Tor first starts, and only then: a mirror
    # that ran has a published address, which an import would replace (A-532).
    project=$(project_name)
    ! has_item "$profiles" mirror ||
      die 'Refusing: the mirror is already set up (mirror is in COMPOSE_PROFILES), so it has an onion address; an import would replace it. Nothing was changed.'
    [[ -z $(docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=tor-mirror) ]] ||
      die 'Refusing: a tor-mirror container exists, so the mirror has run and has an onion address; an import would replace it. Nothing was changed.'
    [[ -n $given || -t 0 ]] ||
      die 'Refusing: with no terminal to confirm the imported onion address on, give it as --mirror-onion-address ADDR.onion. Nothing was changed.'
    onion_key_check "$key_dir"
    main=$(onion_address tor) ||
      die 'Refusing: could not read the main onion address, which the mirror key must differ from. Start the install (docker compose up -d) and try again. Nothing was changed.'
    if ! docker image inspect "$project-tor" >/dev/null 2>&1; then
      docker compose build tor || die 'Could not build the Tor image (the message above says why). Nothing was changed.'
    fi
    expected=$(onion_dry_run "$project-tor") || exit 1
    [[ $expected != "$main" ]] ||
      die "Refusing: that key is the main onion's ($main); the mirror needs an identity of its own. Nothing was changed."
    onion_confirm "$expected" "$given"
    if ! docker volume inspect "${project}_tor_mirror_identity" >/dev/null 2>&1; then
      onion_volume_create "$project" tor_mirror_identity || die 'Could not create the mirror volume. Nothing was changed.'
    fi
    onion_import "$project-tor" "${project}_tor_mirror_identity" ||
      die "Could not import the onion key into the volume ${project}_tor_mirror_identity (the message above says why). .env was not changed." \
        "If Tor never ran on it, remove it (docker volume rm ${project}_tor_mirror_identity) and try again."
  fi
  if has_item "$profiles" mirror; then
    echo 'mirror is already in COMPOSE_PROFILES.'
  else
    # In .env rather than as a Compose profile option, which would replace the other profiles for that one
    # command (A-140).
    env_set COMPOSE_PROFILES "${profiles:+$profiles,}mirror"
    echo 'Added mirror to COMPOSE_PROFILES in .env (the other profiles are kept).'
  fi
  up_wait || die 'The services did not become healthy. Inspect docker compose ps and docker compose logs tor-mirror.'
  mirror=$(wait_onion tor-mirror) || mirror=''
  if [[ -n $expected && $mirror != "$expected" ]]; then
    docker compose stop tor-mirror || true
    die "Refusing: Tor serves ${mirror:-no onion address}, not the imported $expected, so the tor-mirror service was stopped." \
      'Inspect docker compose logs tor-mirror.'
  fi
  [[ -n $mirror ]] || die 'tor-mirror did not write its onion address within 90 seconds. Run ./scripts/install.sh status later to see it.'
  main=$(onion_address tor) || main='(not available: is tor running?)'
  if state=$(app_status 2>/dev/null); then retire_setup_token "$state" || true; fi
  echo
  printf 'Main onion:   %s\n' "$main"
  printf 'Mirror onion: %s\n' "$mirror"
  echo 'Its descriptor can take 1-3 more minutes to reach the Tor network.'
  echo
  echo 'Back up the mirror onion keys now: they are its address. In this directory, with age installed, run'
  echo '  identity=tor_mirror_identity AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT'
  echo 'and then the "Back up an onion identity" commands in docs/operator-guide.md#secrets-env-and-onion-identities.'
  if [[ -n $expected ]]; then onion_import_notes "$key_dir"; fi
  echo 'Publish both addresses in your operator-signed verification document; the installer does not sign anything.'
}

# ---- malware scanner (photo uploads) ----------------------------------------------------------------------

# host_memory_check: clamd holds ClamAV's signatures in memory (about 1 GiB, up to its 3 GiB limit) beside the
# app and database. Refuse a Docker host with less than 3 GiB (status 1) and warn below 4 GiB.
host_memory_check() {
  local total
  total=$(docker info --format '{{.MemTotal}}' 2>/dev/null) || total=''
  if ! [[ $total =~ ^[0-9]+$ ]]; then
    echo 'Could not read the memory of the Docker host (docker info). Nothing was changed.' >&2
    return 1
  fi
  if ((total < 3 * 1073741824)); then
    printf '%s\n' "The malware scanner needs a Docker host with at least 3 GiB of memory; this one has $((total / 1048576)) MiB." \
      'clamd keeps the ClamAV signatures in memory beside the app and the database. Nothing was changed.' >&2
    return 1
  fi
  if ((total < 4 * 1073741824)); then
    printf 'warning: this Docker host has %s MiB of memory; with the malware scanner, 4 GiB or more is advised.\n' "$((total / 1048576))" >&2
  fi
}

# node_disk_check GB: compare the free space under Docker's data directory (docker info's DockerRootDir) with GB, the
# local nodes' estimated size. A-523 (D-66): it warns and never refuses, and says so when it cannot read the value (a
# remote Docker host, or Docker in a VM, keeps its data directory off this filesystem).
node_disk_check() {
  local root='' available=''
  root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null) || root=''
  if [[ $root == /* && -d $root ]]; then
    available=$(df -Pk "$root" 2>/dev/null | awk 'NR == 2 { print $4 }') || available=''
  fi
  if ! [[ $available =~ ^[0-9]+$ ]]; then
    echo "Could not read the free space of the Docker data directory (docker info gave ${root:-nothing}); make sure it has about $1 GB free for the local nodes. Continuing."
    return 0
  fi
  available=$((available / 1048576))
  if ((available < $1)); then
    printf 'WARNING: the Docker data directory (%s) has %s GB free, less than the about %s GB the local nodes need (an estimate). Continuing: free space before they sync, or they stop when the disk is full.\n' "$root" "$available" "$1" >&2
  else
    printf 'Docker data directory (%s) has %s GB free.\n' "$root" "$available"
  fi
}

# clamd_version: clamd's VERSION reply (ClamAV <release>/<daily signature version>/<signature date>). Status 1
# when clamd does not answer: clamdscan then prints only its own version.
clamd_version() {
  local version
  version=$(docker compose exec -T clamd clamdscan --version 2>/dev/null) || return 1
  [[ $version =~ ^ClamAV\ [^/]+/[0-9]+/.+$ ]] || return 1
  printf '%s' "$version"
}

# wait_clamd: wait up to 10 minutes for clamd to load its signatures and answer, and print its VERSION.
wait_clamd() {
  local attempt deadline=$((SECONDS + 600))
  for ((attempt = 0; attempt < 120 && SECONDS < deadline; attempt++)); do
    clamd_version && return 0
    sleep 5
  done
  return 1
}

# scanner_layout_problems CONFIG: what is wrong with the scanner in CONFIG, `docker compose config` output
# (Compose's own normalised YAML, with two-space indents): clamd on the internal scanner network alone, which
# only the app joins, with no host port, no volume and a pinned image with signatures. One line per problem.
scanner_layout_problems() {
  local facts members networks image
  facts=$(awk '
    /^[^ ]/ { top = $1; section = ""; next }
    top == "services:" && /^  [^ ]/ { service = $1; sub(/:$/, "", service); section = ""; next }
    top == "services:" && /^    [^ ]/ {
      section = $1; sub(/:.*/, "", section)
      if (section == "network_mode" || section == "ports") print service, section
      if (section == "image") print service, "image", $2
      next
    }
    top == "services:" && section == "networks" && /^      [^ -]/ { name = $1; sub(/:.*/, "", name); print service, "network", name; next }
    top == "services:" && section == "volumes" && /^      - type: / { print service, "mount", $3; next }
    top == "networks:" && /^  [^ ]/ { network = $1; sub(/:$/, "", network); next }
    top == "networks:" && /^    internal: true$/ { print network, "internal" }
  ' <<< "$1")
  members=$(awk '$2 == "network" && $3 == "scanner" { print $1 }' <<< "$facts" | sort | tr '\n' ' ')
  networks=$(awk '$1 == "clamd" && $2 == "network" { print $3 }' <<< "$facts" | sort | tr '\n' ' ')
  image=$(awk '$1 == "clamd" && $2 == "image" { print $3 }' <<< "$facts")
  [[ $networks == 'scanner ' ]] || echo "  - clamd must be on the scanner network only (it is on: ${networks% })"
  [[ $members == 'app clamd ' ]] || echo "  - only app and clamd may join the scanner network (it has: ${members% })"
  grep -qx 'scanner internal' <<< "$facts" || echo '  - the scanner network must be internal (no route out of the host)'
  if grep -qx 'clamd network_mode' <<< "$facts"; then echo '  - clamd must not set network_mode'; fi
  if grep -qx 'clamd ports' <<< "$facts"; then echo '  - clamd must publish no port'; fi
  if grep -q '^clamd mount [^b]' <<< "$facts"; then
    echo '  - clamd must mount no volume: its signatures come from its image (docs/operator-guide.md#photo-uploads-and-the-malware-scanner)'
  fi
  clamav_image_ok "$image" || echo "  - clamd runs ${image:-no image}: it must be one ClamAV release with signatures (a MAJOR.MINOR.PATCH tag, optionally -debian, or an @sha256 digest)"
}

# refresh_scanner: have Docker fetch the newest build of the pinned tag (upstream rebuilds it with newer
# signatures) and recreate clamd from it when it changed, then print the signature versions before and after.
refresh_scanner() {
  local before after
  before=$(clamd_version) || before='(clamd not answering)'
  printf 'Signatures before: %s\n' "$before"
  docker compose pull clamd || {
    printf '%s\n' 'Could not pull the scanner image (the message above says why); clamd keeps its current signatures.' \
      'On a host without Internet access, load the image instead: docs/operator-guide.md#photo-uploads-and-the-malware-scanner.' >&2
    return 1
  }
  docker compose up -d clamd || { echo 'Could not recreate clamd. Inspect docker compose logs clamd.' >&2; return 1; }
  after=$(wait_clamd) || {
    echo 'clamd did not answer within 10 minutes; photo uploads are refused until it does. Inspect docker compose logs clamd.' >&2
    return 1
  }
  printf 'Signatures now:    %s\n' "$after"
}

# scanner_config_changed: compare the config currently in clamd with the bind-mounted file in this checkout.
# Compose does not hash bind-mounted file contents, so an upgrade must explicitly recreate clamd when they differ.
# If the old container cannot be read (for example, it is restarting after a bad config), treat that as a mismatch:
# recreating it is the safe way to apply the checked-in config. No config contents are printed.
scanner_config_changed() {
  docker compose exec -T clamd cat /etc/clamav/clamd.conf 2>/dev/null | cmp -s - deploy/clamd.conf || return 0
  return 1
}

cmd_add_scanner() {
  [[ $# -eq 0 ]] || usage
  require_env
  legacy_pooled_guard || exit 1
  lock_env_or_die
  local staged previous files profiles version
  host_memory_check || exit 1
  files=$(env_value COMPOSE_FILE)
  [[ -n $files ]] || die 'COMPOSE_FILE is not set in .env; run ./scripts/install.sh upgrade first, which adds it. Nothing was changed.'
  # All three keys change in a copy that replaces .env whole, so .env never enables half the scanner.
  staged=$(mktemp .env.edit.XXXXXX) && previous=$(mktemp .env.edit.XXXXXX) || die 'Could not write .env; it was not changed.'
  temporaries="$temporaries $staged $previous"
  { cat .env > "$staged" && cat .env > "$previous"; } || die 'Could not write .env; it was not changed.'
  env_file=$staged
  if [[ -z $(env_value CLAMAV_IMAGE) ]]; then env_set CLAMAV_IMAGE "$clamav_image_default"; fi
  compose_has compose.scanner.yaml || env_set COMPOSE_FILE "$files:compose.scanner.yaml"
  profiles=$(env_value COMPOSE_PROFILES)
  # In .env rather than as a Compose profile option, which would replace the other profiles for that one
  # command (A-140).
  has_item "$profiles" scanner || env_set COMPOSE_PROFILES "${profiles:+$profiles,}scanner"
  [[ $(env_value CLAMD_ADDRESS) == "$clamd_address" ]] || env_set CLAMD_ADDRESS "$clamd_address"
  env_file=.env
  # The whole configuration is checked with the scanner in it; a refusal puts the previous .env back.
  if cmp -s "$staged" .env; then
    run_check || exit 1
    echo 'The malware scanner is already configured in .env.'
  else
    chmod 600 "$staged" && mv -f "$staged" .env || die 'Could not write .env; it was not changed.'
    if ! run_check; then
      chmod 600 "$previous" && mv -f "$previous" .env
      die 'The configuration with the scanner failed the check above, so .env was not changed.'
    fi
    echo 'Added the malware scanner to .env: the scanner profile, compose.scanner.yaml and CLAMD_ADDRESS.'
  fi
  docker compose up -d clamd || die 'Could not start clamd. Inspect docker compose logs clamd.'
  echo 'Waiting for clamd to load its signatures (a minute or two; at most 10 minutes).'
  version=$(wait_clamd) || die 'clamd did not answer within 10 minutes. Inspect docker compose logs clamd, fix the cause and run' \
    './scripts/install.sh add-scanner again. Until then photo uploads are refused; the rest of the market is unaffected.'
  # The app learns CLAMD_ADDRESS and joins the scanner network when Compose recreates it (Tor restarts with it).
  up_wait || die 'The services did not become healthy. Inspect docker compose ps and docker compose logs app.'
  printf 'Malware scanner ready. Signatures: %s\n' "$version"
  echo 'ClamAV updates its signatures daily; refresh them at least monthly with ./scripts/install.sh refresh-scanner'
  echo '(upgrade does it too). docs/operator-guide.md#photo-uploads-and-the-malware-scanner explains the options.'
}

cmd_refresh_scanner() {
  [[ $# -eq 0 ]] || usage
  require_env
  lock_env_or_die
  has_item "$(env_value COMPOSE_PROFILES)" scanner ||
    die 'The malware scanner is off in this install (COMPOSE_PROFILES lacks scanner): there is nothing to refresh. ./scripts/install.sh add-scanner turns it on.'
  run_check || exit 1
  refresh_scanner || exit 1
}

# ---- upgrade ----------------------------------------------------------------------------------------------

cmd_upgrade() {
  local no_backup=false
  case $# in
    0) ;;
    1) [[ $1 == --no-backup ]] || usage; no_backup=true ;;
    *) usage ;;
  esac
  require_env
  legacy_pooled_guard || exit 1
  lock_env_or_die
  # backup.sh dumps the internal database through Compose; an external one only from BACKUP_DATABASE_URL.
  local external_backup='' database_url
  database_url=$(env_value DATABASE_URL)
  if [[ -n $database_url && $(url_host "$database_url") != db ]]; then
    external_backup='With an external database, backup.sh also needs its URL: BACKUP_DATABASE_URL=YOUR_EXTERNAL_DATABASE_URL AGE_RECIPIENT=age1YOUR_PUBLIC_RECIPIENT ./scripts/install.sh upgrade (the URL of DATABASE_URL in .env serves; PostgreSQL client tools and Python 3 must be installed; see UPGRADING.md).'
  fi
  if [[ -z ${AGE_RECIPIENT:-} && $no_backup == false ]]; then
    die 'Refusing to upgrade without a database backup: migrations are one-way.' \
      'Set AGE_RECIPIENT to your age public key (AGE_RECIPIENT=age1... ./scripts/install.sh upgrade), or pass --no-backup if you have just taken one.' \
      ${external_backup:+"$external_backup"}
  fi
  if [[ $no_backup == false && -n $external_backup && -z ${BACKUP_DATABASE_URL:-} ]]; then
    die 'Refusing to upgrade: DATABASE_URL names an external database, which the backup reads only from BACKUP_DATABASE_URL. Nothing was changed.' \
      "$external_backup" 'Or pass --no-backup if you have just taken a backup.'
  fi
  database_url=''
  local project listing entry candidates='' count stamp copy changes='' key value default mode files profiles token database host egress
  local recorded_clamav_default
  local auth_profiles bitcoin_auth_to_add=''
  # The project whose volumes hold this install: COMPOSE_PROJECT_NAME, else the one its containers were created
  # under from this directory, else the caller's COMPOSE_PROJECT_NAME or Compose's default.
  if ! env_get COMPOSE_PROJECT_NAME >/dev/null; then
    listing=$(all_containers) || die 'Could not list Docker containers (is the Docker daemon running?); nothing was changed.'
    while IFS= read -r entry; do
      [[ -n $entry ]] || continue
      if same_dir "${entry#*|}"; then candidates="$candidates${entry%%|*}"$'\n'; fi
    done <<< "$listing"
    candidates=$(printf '%s' "$candidates" | sort -u)
    count=$(printf '%s' "$candidates" | grep -c . || true)
    if ((count > 1)); then
      die 'Containers created from this directory belong to more than one Compose project:' "$(printf '%s\n' "$candidates" | sed 's/^/  /')" \
        'Set COMPOSE_PROJECT_NAME in .env to the one this install uses, then run upgrade again. Nothing was changed.'
    fi
    project=${candidates:-${caller_project:-opsecmkt}}
    if [[ -n $caller_project && $caller_project != "$project" ]]; then
      die "COMPOSE_PROJECT_NAME=$caller_project in your shell, but this directory's containers belong to project $project. Nothing was changed."
    fi
    [[ $project =~ $project_pattern ]] || die 'COMPOSE_PROJECT_NAME must use lower-case letters, digits, hyphens and underscores.'
  else
    project=$(project_name)
  fi
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  if [[ $no_backup == false ]]; then
    mkdir -p backups
    COMPOSE_PROJECT_NAME=$project scripts/backup.sh "backups/pre-upgrade-$stamp.dump.age" ||
      die 'The backup failed, so nothing was changed. Fix it (the message above says why) or pass --no-backup.' \
        ${external_backup:+"$external_backup"}
  fi
  # The one other copy of the secrets the installer writes (I9): .env as it was, for rolling back.
  copy=.env.pre-upgrade-$stamp
  count=1
  while [[ -e $copy ]]; do count=$((count + 1)); copy=.env.pre-upgrade-$stamp-$count; done
  temporaries="$temporaries $copy"
  cat .env > "$copy" && chmod 600 "$copy" || die "Could not copy .env to $copy (is the disk full?); nothing was changed."
  temporaries=${temporaries% "$copy"}

  # Decide whether an old install has a local Bitcoin profile before changing .env. Legacy installs infer the
  # profile from the app URL. A failed salt/HMAC operation leaves every current setting untouched.
  auth_profiles=$(env_value COMPOSE_PROFILES)
  if ! env_get COMPOSE_PROFILES >/dev/null && [[ -n $(env_value BITCOIN_RPC_PASSWORD) ]]; then
    auth_profiles="${auth_profiles:+$auth_profiles,}bitcoin"
  fi
  if has_item "$auth_profiles" bitcoin && [[ -z $(env_value BITCOIN_RPC_AUTH) ]]; then
    value=$(env_value BITCOIN_RPC_PASSWORD)
    [[ -n $value ]] || die 'The local Bitcoin node needs BITCOIN_RPC_PASSWORD; nothing was changed or restarted.'
    bitcoin_auth_to_add=$(bitcoin_rpcauth "$value") ||
      die 'Could not generate the local Bitcoin RPC verifier; nothing was changed or restarted.'
    value=''
  fi

  if ! env_get COMPOSE_PROJECT_NAME >/dev/null; then
    env_set COMPOSE_PROJECT_NAME "$project"
    changes="$changes  added COMPOSE_PROJECT_NAME ($project, the project its containers and volumes use)"$'\n'
  fi
  # The key root split (D3): APP_SECRET_KEY takes SETUP_TOKEN's value, so every derived key stays the same.
  token=$(env_value SETUP_TOKEN)
  if [[ -z $(env_value APP_SECRET_KEY) && -n $token ]]; then
    env_set APP_SECRET_KEY "$token"
    changes="$changes  set APP_SECRET_KEY to the current SETUP_TOKEN value (keys, sessions and TOTP unchanged)"$'\n'
  fi
  token=''
  files=$(env_value COMPOSE_FILE)
  if ! env_get APP_MODE >/dev/null; then
    mode=clearnet
    if compose_has compose.tor.yaml; then mode=tor; fi
    env_set APP_MODE "$mode"
    changes="$changes  added APP_MODE ($mode, from COMPOSE_FILE)"$'\n'
  fi
  mode=$(env_value APP_MODE)
  database=$(env_value DATABASE_URL)
  if ! env_get COMPOSE_PROFILES >/dev/null; then
    profiles=''
    if [[ $(url_host "$database") == db ]]; then profiles=internal-db; fi
    if [[ -n $(env_value BITCOIN_RPC_PASSWORD) ]]; then profiles="${profiles:+$profiles,}bitcoin"; fi
    if [[ -n $(env_value MONERO_IMAGE) ]]; then profiles="${profiles:+$profiles,}monero"; fi
    if [[ $(url_host "$(env_value ESCROW_XMR_WALLET_RPC_URL)") == monero-escrow-wallet ]]; then profiles="${profiles:+$profiles,}monero-escrow-wallet"; fi
    env_set COMPOSE_PROFILES "$profiles"
    changes="$changes  added COMPOSE_PROFILES (from the database and node URLs)"$'\n'
  fi
  profiles=$(env_value COMPOSE_PROFILES)
  # Old local-node installs have only the private password and URL. Add its verifier after profile inference,
  # without rotating either value; an existing mismatched verifier is left untouched and check refuses startup.
  if [[ -n $bitcoin_auth_to_add ]]; then
    env_set BITCOIN_RPC_AUTH "$bitcoin_auth_to_add"
    changes="$changes  added BITCOIN_RPC_AUTH for the existing local Bitcoin password (password and RPC URL unchanged)"$'\n'
    bitcoin_auth_to_add=''
  fi
  if [[ -z $files ]] && ! env_get COMPOSE_FILE >/dev/null; then
    files="compose.yaml:compose.$mode.yaml:compose.nodes.yaml"
    if has_item "$profiles" internal-db; then files="$files:compose.internal-db.yaml"; fi
    if [[ $(env_value ESCROW_XMR_SEED_KEY) == "$xmr_seed_key_path" ]]; then files="$files:compose.escrow-xmr.yaml"; fi
    # Tor with an external database needs direct egress.
    if [[ $mode == tor ]]; then
      egress=false
      if [[ $(url_host "$database") != db ]]; then egress=true; fi
      if [[ $egress == true ]]; then files="$files:compose.external-egress.yaml"; fi
    fi
    env_set COMPOSE_FILE "$files"
    changes="$changes  added COMPOSE_FILE (from APP_MODE and the profiles)"$'\n'
  fi
  if ! env_get APP_PORT >/dev/null; then
    # The port the install publishes now: the caller's APP_PORT when given, else the host port of the app
    # container (docker compose port prints ADDRESS:PORT), else Compose's default.
    local source='the port from your shell'
    value=$caller_app_port
    if [[ -z $value ]]; then
      value=$(docker compose port app 8080 2>/dev/null | head -n 1) || value=''
      value=${value##*:}
      source='the port the app container publishes'
      if ! [[ $value =~ ^[0-9]{1,5}$ ]]; then value=8080 source='the default'; fi
    fi
    [[ $value =~ ^[0-9]{1,5}$ ]] && ((10#$value > 0 && 10#$value <= 65535)) || die 'APP_PORT must be between 1 and 65535.'
    env_set APP_PORT "$value"
    changes="$changes  added APP_PORT ($value, $source)"$'\n'
  fi
  local defaulted=''
  while IFS= read -r entry; do
    key=${entry%%=*}
    if ! env_get "$key" >/dev/null; then
      env_set "$key" "${entry#*=}"
      defaulted="${defaulted:+$defaulted, }$key"
    fi
  done <<< "$upgrade_defaults"
  if [[ -n $defaulted ]]; then
    changes="$changes  added, with the values Compose already used: $defaulted"$'\n'
  fi
  # Newer installer defaults may replace an image only when the prior .env marker records the current value as
  # was installer-managed. A markerless custom or historical value is unknown and stays untouched; seed a marker
  # for markerless installs only when the image equals the default this version knows.
  recorded_clamav_default=$(env_value CLAMAV_IMAGE_DEFAULT)
  value=$(env_value CLAMAV_IMAGE)
  if [[ -n $recorded_clamav_default ]]; then
    if [[ -n $value && $value == "$recorded_clamav_default" && $value != "$clamav_image_default" ]]; then
      env_set CLAMAV_IMAGE "$clamav_image_default"
      changes="$changes  advanced the install-managed ClamAV image to the current default"$'\n'
    fi
    if [[ $recorded_clamav_default != "$clamav_image_default" ]]; then
      env_set CLAMAV_IMAGE_DEFAULT "$clamav_image_default"
      changes="$changes  recorded the current ClamAV image default for future upgrades"$'\n'
    fi
  elif [[ $value == "$clamav_image_default" ]]; then
    env_set CLAMAV_IMAGE_DEFAULT "$clamav_image_default"
    changes="$changes  recorded the current ClamAV image default for future upgrades"$'\n'
  fi
  # Installs from before A-100 float the node and wallet images on :latest (A-174): pin the verified releases.
  for key in BITCOIN_IMAGE MONERO_IMAGE MONERO_WALLET_IMAGE; do
    case $key in
      BITCOIN_IMAGE) default=$bitcoin_image_default ;;
      MONERO_IMAGE) default=$monero_image_default ;;
      MONERO_WALLET_IMAGE) default=$monero_wallet_image_default ;;
    esac
    value=$(env_value "$key")
    if [[ -n $value ]] && ! image_pinned "$value" && [[ $(image_repository "$value") == "$(image_repository "$default")" ]]; then
      env_set "$key" "$default"
      changes="$changes  pinned $key to $default (was $value)"$'\n'
    fi
  done
  # A mirror started with a Compose profile option is left out of every later command (A-140): keep it in .env.
  if [[ $mode == tor ]] && ! has_item "$profiles" mirror &&
    [[ -n $(docker ps -a -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=tor-mirror) ]]; then
    env_set COMPOSE_PROFILES "${profiles:+$profiles,}mirror"
    changes="$changes  added mirror to COMPOSE_PROFILES (a tor-mirror container exists)"$'\n'
  fi
  if [[ -n $changes ]]; then
    printf 'Updated .env (the previous file is %s):\n%s' "$copy" "$changes"
  else
    echo '.env is already up to date.'
  fi
  if [[ -z $(env_value AUDIT_SIGNING_KEY) ]]; then
    echo 'Note: AUDIT_SIGNING_KEY is not set, so signed audit exports stay off (see UPGRADING.md).'
  fi
  run_check || die "Nothing was restarted. Fix .env, or put the previous file back with: mv $copy .env"
  up_wait --build ||
    die "The services did not become healthy. Inspect docker compose ps and docker compose logs app; the previous .env is $copy (UPGRADING.md, Rolling back)."
  local state recreate=false
  if state=$(app_status) && retire_setup_token "$state"; then recreate=true; fi
  if [[ $recreate == true ]]; then
    up_wait || die 'The app did not become healthy. Inspect docker compose logs app.'
  fi
  if cmp -s .env "$copy"; then
    rm -f "$copy"
  elif [[ $(env_value APP_MODE) == tor ]]; then
    # Tor resolves the app's address once, at start; recreate it once after a change like this one.
    if has_item "$(env_value COMPOSE_PROFILES)" mirror; then
      docker compose up -d --wait --force-recreate tor tor-mirror || die 'Tor did not become healthy. Inspect docker compose logs tor tor-mirror.'
    else
      docker compose up -d --wait --force-recreate tor || die 'Tor did not become healthy. Inspect docker compose logs tor.'
    fi
  fi
  # The scanner's signatures age with its image: every upgrade refreshes them (never turns the scanner on).
  if has_item "$(env_value COMPOSE_PROFILES)" scanner; then
    if scanner_config_changed; then
      echo 'The malware scanner configuration changed (or the running config could not be read); recreating clamd.'
      docker compose up -d --force-recreate clamd ||
        echo 'warning: clamd could not be recreated to apply its configuration; photo uploads may remain unavailable. Inspect docker compose logs clamd.' >&2
    fi
    refresh_scanner || printf '%s\n' 'warning: the malware scanner signatures were not refreshed (see above); the upgrade itself is complete.' \
      'Run ./scripts/install.sh refresh-scanner once the cause is fixed.' >&2
  fi
  echo 'Upgrade complete. ./scripts/install.sh status shows the result.'
}

# Sourced (scripts/test-tor-identity-import.sh), the installer only defines its functions.
if [[ ${BASH_SOURCE[0]} != "$0" ]]; then return 0; fi
# From here on Docker Compose reads .env alone; the values the installer honours were captured above.
isolate_env
case ${1-} in
  -h|--help|help) usage_text; exit 0 ;;
  init) shift; cmd_init "$@" ;;
  check) shift; [[ $# -eq 0 ]] || usage; require_docker; cmd_check ;;
  status) shift; [[ $# -eq 0 ]] || usage; require_docker; cmd_status ;;
  setup-token) shift; require_docker; cmd_setup_token "$@" ;;
  add-mirror) shift; require_docker; cmd_add_mirror "$@" ;;
  add-scanner) shift; require_docker; cmd_add_scanner "$@" ;;
  refresh-scanner) shift; require_docker; cmd_refresh_scanner "$@" ;;
  upgrade) shift; require_docker; cmd_upgrade "$@" ;;
  ''|--*) cmd_init "$@" ;;
  *) usage ;;
esac
