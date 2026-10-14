#!/bin/sh
# Tor ignores malformed .auth files and makes an onion public when none load. Refuse before starting it.
# Arguments: source allowlist, destination authorized_clients directory, then Tor command; or source --check.
# Copy the validated records into a Tor-owned snapshot, so changes on the host cannot race Tor's parser.
set -eu
export LC_ALL=C
fail() { echo 'Private admin onion requires at least one valid public client .auth file; refusing to start Tor.' >&2; exit 1; }
[ "$#" -ge 2 ] || fail
authdir=$1
shift
[ -d "$authdir" ] && [ ! -L "$authdir" ] && [ -r "$authdir" ] && [ -x "$authdir" ] || fail
snapshot=''
if [ "$1" = --check ]; then
  [ "$#" -eq 1 ] || fail
else
  [ "$#" -ge 2 ] || fail
  destination=$1
  shift
  case "$destination" in /*/authorized_clients) ;; *) fail ;; esac
  parent=${destination%/*}
  [ ! -L "$parent" ] || fail
  [ ! -L "$destination" ] || fail
  snapshot=$(mktemp -d)
  trap 'if [ -n "$snapshot" ]; then rm -rf "$snapshot"; fi' 0
fi
count=0
for file in "$authdir"/* "$authdir"/.[!.]* "$authdir"/..?*; do
  [ -e "$file" ] || [ -L "$file" ] || continue
  name=${file##*/}
  case "$name" in .auth) fail ;; *.auth) ;; *) fail ;; esac
  [ -f "$file" ] && [ ! -L "$file" ] && [ -r "$file" ] || fail
  # 32 raw bytes encode to 52 base32 characters, with only A/Q valid as the final padded symbol.
  bytes=$(wc -c < "$file")
  [ "$bytes" -eq 70 ] || [ "$bytes" -eq 71 ] || fail
  record=$(cat "$file") || fail
  [ "${#record}" -eq 70 ] || fail
  printf '%s\n' "$record" | grep -qxE 'descriptor:x25519:[A-Z2-7]{51}[AQ]' || fail
  if [ -n "$snapshot" ]; then
    # Only the captured, validated value is written, rather than rereading the host file after validation.
    (umask 077; printf '%s\n' "$record" > "$snapshot/$name")
  fi
  count=$((count + 1))
done
[ "$count" -gt 0 ] || fail
[ -n "$snapshot" ] || exit 0
# Only the private service initializes its own directory after its source has passed. The shared Tor image's
# /var/lib/tor must stay empty for storefront/mirror identity import and restore into fresh volumes.
if [ ! -e "$parent" ]; then
  mkdir -m 700 "$parent"
fi
[ -d "$parent" ] && [ ! -L "$parent" ] || fail
chmod 700 "$parent" # Tor also verifies the hidden-service directory's owner and private mode.
# These directories hold public client keys only. Replace the old snapshot so revoked clients cannot persist.
if [ -e "$destination" ]; then
  [ -d "$destination" ] || fail
  rm -rf "$destination"
fi
mv "$snapshot" "$destination"
snapshot=''
exec "$@"
