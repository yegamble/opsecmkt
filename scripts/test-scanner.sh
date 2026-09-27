#!/usr/bin/env bash
# Run the malware scanner exactly as compose.scanner.yaml defines it, but with no network at all, and prove what
# the war room ruled (A-184, ruling C1 and C6): clamd starts from the pinned image with its own signatures (no
# volume, no freshclam), read-only, as user clamav, with every capability dropped; it answers PING and VERSION,
# whose signature date is the image's own; SHUTDOWN, RELOAD and STATS are refused; deploy/clamd.conf's limits
# are in force. It also records why compose.scanner.yaml replaces the image's entrypoint.
#
# Requires Docker with Compose v2 and Python 3. Pulls the image if it is missing (CLAMAV_IMAGE overrides it, for
# example to try a new tag before changing the default). Never reads the checkout's .env; the exit trap removes
# every container it started. Loading the signatures takes up to a minute or two and about 1 GiB of memory.
set -euo pipefail
cd "$(dirname "$0")/.."
for dependency in docker python3 openssl; do
  command -v "$dependency" >/dev/null || { echo "Missing dependency: $dependency" >&2; exit 1; }
done
docker compose version >/dev/null
# Only CLAMAV_IMAGE may change what is tested; --env-file /dev/null below keeps the checkout's .env out.
unset COMPOSE_FILE COMPOSE_PROFILES COMPOSE_PROJECT_NAME COMPOSE_ENV_FILES LOG_DRIVER
name="opsecmkt-scanner-check-$(openssl rand -hex 6)"
containers=()
fail() { echo "FAIL: $*" >&2; exit 1; }
cleanup() {
  local status=$? container
  trap - EXIT
  for container in ${containers[@]+"${containers[@]}"}; do
    ((status == 0)) || { echo "--- logs of $container ---" >&2; docker logs "$container" 2>&1 | tail -n 30 >&2 || true; }
    docker rm -f "$container" >/dev/null 2>&1 || true
  done
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP

# The clamd service as Compose resolves it, turned into `docker run` options with --network none in place of
# its networks. Python also refuses a definition that loses a hardening key, gains a volume, a port or a
# network, or depends on anything.
config=$(DATABASE_URL=postgres://unused@db/unused docker compose --env-file /dev/null -p "$name" \
  -f compose.yaml -f compose.scanner.yaml --profile scanner config --format json)
options=$(python3 -c '
import json, re, sys
config = json.load(sys.stdin)
clamd = config["services"]["clamd"]
def need(ok, message):
    if not ok:
        sys.exit("compose.scanner.yaml: clamd " + message)
networks = clamd.get("networks")
need(networks == {"scanner": None}, f"must be on the scanner network only, got {networks!r}")
need(config["networks"]["scanner"].get("internal") is True, "network scanner must be internal")
need(clamd.get("read_only") is True and clamd.get("user") == "clamav", "must run read_only as user clamav")
need(clamd.get("cap_drop") == ["ALL"] and clamd.get("security_opt") == ["no-new-privileges:true"], "must drop every capability with no-new-privileges")
need(not clamd.get("ports") and not clamd.get("depends_on") and not clamd.get("network_mode"), "must publish no port and depend on nothing")
need(not clamd.get("environment"), "must take no environment")
volumes = clamd.get("volumes", [])
need(len(volumes) == 1 and volumes[0]["type"] == "bind" and volumes[0]["target"] == "/etc/clamav/clamd.conf"
     and volumes[0]["source"].endswith("/deploy/clamd.conf") and volumes[0].get("read_only") is True,
     f"must mount only deploy/clamd.conf, read-only (no signature volume), got {volumes!r}")
need(clamd.get("entrypoint") == ["clamd"] and clamd.get("command") == ["--foreground", "--config-file=/etc/clamav/clamd.conf"],
     "must run clamd --foreground --config-file=/etc/clamav/clamd.conf directly")
image = clamd["image"]
reference, _, digest = image.partition("@")
tag = reference.rpartition("/")[2].partition(":")[2]
# One release with signatures: MAJOR.MINOR.PATCH (optionally -debian) or a digest, never a moving or _base tag.
need(re.fullmatch(r"\d+\.\d+\.\d+(-debian)?", tag) or (digest and "_base" not in tag),
     f"image must be one ClamAV release with signatures, got {image!r}")
options = ["--user", clamd["user"], "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
           "--memory", str(clamd["mem_limit"]), "--pids-limit", str(clamd["pids_limit"]),
           "--cpu-period", str(clamd["cpu_period"]), "--cpu-quota", str(clamd["cpu_quota"]),
           "--mount", "type=bind,source=%s,target=%s,readonly" % (volumes[0]["source"], volumes[0]["target"]),
           "--entrypoint", clamd["entrypoint"][0]]
for tmpfs in clamd["tmpfs"]:
    options += ["--tmpfs", tmpfs]
print("\n".join(options + [image] + clamd["command"]))
' <<< "$config")
run_options=()
while IFS= read -r line; do run_options+=("$line"); done <<< "$options"
image=${run_options[${#run_options[@]} - 3]}
echo "image: $image"
docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null

# Why compose.scanner.yaml replaces the entrypoint: the image's /init creates /run/clamav and chowns
# /var/lib/clamav before it runs anything, which a read-only container refuses.
if output=$(docker run --rm --network none --read-only --tmpfs /tmp --tmpfs /run/clamav --user clamav --cap-drop ALL \
  --security-opt no-new-privileges:true "$image" clamd --version 2>&1); then
  echo "note: the image's own entrypoint now runs read-only; the entrypoint override is no longer required"
else
  grep -q 'Read-only file system' <<< "$output" || fail "the image's entrypoint failed for another reason: $output"
  echo "entrypoint: the image's /init stops read-only ($(grep -m1 'Read-only file system' <<< "$output")), so clamd runs directly"
fi

containers+=("$name")
docker run -d --name "$name" --network none "${run_options[@]}" >/dev/null
started=$SECONDS
until docker exec "$name" clamdcheck.sh >/dev/null 2>&1; do
  [[ $(docker inspect -f '{{.State.Running}}' "$name") == true ]] || fail 'clamd exited while loading its signatures'
  ((SECONDS - started < 600)) || fail 'clamd did not answer PING within 10 minutes'
  sleep 2
done
echo "PING answered after $((SECONDS - started)) s (the Compose healthcheck, clamdcheck.sh, passes)"

# The container as Docker runs it.
docker inspect "$name" | python3 -c '
import json, sys
state = json.load(sys.stdin)[0]
host, config = state["HostConfig"], state["Config"]
checks = {
    "read-only root": host["ReadonlyRootfs"] is True,
    "user clamav": config["User"] == "clamav",
    "no network": host["NetworkMode"] == "none",
    "capabilities dropped": host["CapDrop"] == ["ALL"] and not host.get("CapAdd"),
    "no-new-privileges": host["SecurityOpt"] == ["no-new-privileges:true"],
    "only the read-only clamd.conf mounted": [(m["Type"], m["Destination"], m["RW"]) for m in state["Mounts"]] == [("bind", "/etc/clamav/clamd.conf", False)],
}
for name, ok in checks.items():
    if not ok:
        sys.exit(f"FAIL: container is not {name}")
print("hardening:", ", ".join(checks))
'
[[ $(docker exec "$name" id -un) == clamav ]] || fail 'processes in the container do not run as clamav'
docker exec "$name" touch /var/lib/clamav/probe 2>/dev/null && fail 'the signature directory is writable'
docker exec "$name" touch /etc/clamav/clamd.conf 2>/dev/null && fail 'clamd.conf is writable'
docker exec "$name" touch /tmp/probe || fail '/tmp (the scan tmpfs) is not writable'
# No route anywhere: no IPv4 route at all, IPv6 routes only on loopback (a kernel may list idle tunnel devices).
routes=$(docker exec "$name" cat /proc/net/route /proc/net/ipv6_route |
  awk 'NF == 11 && $1 != "Iface" { print $1 } NF == 10 && $10 != "lo" { print $10 }')
[[ -z $routes ]] || fail "the container has routes besides loopback: $routes"
processes=$(docker exec "$name" cat /proc/1/comm)
[[ $processes == clamd ]] || fail "process 1 is $processes, not clamd"
if docker exec "$name" sh -c 'cat /proc/[0-9]*/comm' | grep -q freshclam; then fail 'freshclam is running'; fi
echo 'read-only: signatures and clamd.conf cannot be written; /tmp can. Routes: loopback only. Processes: clamd, no freshclam.'

# clamd_send BYTES: send one command (Python bytes literal) to clamd over the container's loopback, with the
# image's own nc, and print the reply without its terminating NUL.
clamd_send() {
  python3 -c "import sys; sys.stdout.buffer.write($1)" | docker exec -i "$name" nc 127.0.0.1 3310 | tr -d '\0'
}
reply=$(clamd_send "b'zPING\0'")
[[ $reply == PONG ]] || fail "PING answered '$reply'"
version=$(clamd_send "b'zVERSION\0'")
[[ $version =~ ^ClamAV\ [0-9.]+/([0-9]+)/(.+)$ ]] || fail "VERSION answered '$version'"
daily=${BASH_REMATCH[1]}
# The daily database built into the image, read without clamd.
info=$(docker exec "$name" sigtool --info /var/lib/clamav/daily.cvd)
grep -qx "Version: $daily" <<< "$info" || fail "VERSION reports daily $daily, but the image's daily.cvd is: $(grep '^Version' <<< "$info")"
echo "VERSION: $version (the image's own daily.cvd, $(grep '^Build time' <<< "$info"))"
for command in SHUTDOWN RELOAD STATS; do
  reply=$(clamd_send "b'z$command\0'")
  [[ $reply == 'COMMAND UNAVAILABLE' ]] || fail "$command answered '$reply', not COMMAND UNAVAILABLE"
done
sleep 2
reply=$(clamd_send "b'zPING\0'")
[[ $reply == PONG && $(docker inspect -f '{{.State.Running}} {{.RestartCount}}' "$name") == 'true 0' ]] ||
  fail 'clamd stopped after SHUTDOWN'
echo 'SHUTDOWN, RELOAD and STATS: COMMAND UNAVAILABLE; clamd keeps running'

# Scans over INSTREAM, as the app sends them: 4-byte big-endian chunk lengths and a zero-length end. The EICAR
# test string is assembled here so this file never contains it.
instream() {
  python3 -c "import struct, sys; data = $1; sys.stdout.buffer.write(b'zINSTREAM\0' + b''.join(struct.pack('>I', len(data[i:i + 65536])) + data[i:i + 65536] for i in range(0, len(data), 65536)) + b'\0\0\0\0')" |
    docker exec -i "$name" nc 127.0.0.1 3310 | tr -d '\0'
}
reply=$(instream "b'OPSEC Market scanner check: an ordinary text file.\n'")
[[ $reply == 'stream: OK' ]] || fail "a clean stream answered '$reply'"
reply=$(instream "b'X5O!P%@AP[4\\\\PZX54(P^)7CC)7}\$EICAR' + b'-STANDARD-ANTIVIRUS-TEST-FILE!\$H+H*'")
[[ $reply == 'stream: '*' FOUND' ]] || fail "the EICAR test string answered '$reply'"
echo "scans: clean text -> stream: OK; EICAR -> $reply"
# StreamMaxLength 9M: one byte over is refused, not scanned in part.
reply=$(instream "b'A' * (9 * 1024 * 1024 + 1)")
[[ $reply == 'INSTREAM size limit exceeded. ERROR' ]] || fail "a 9 MiB + 1 byte stream answered '$reply'"
echo "StreamMaxLength: 9 MiB + 1 byte -> $reply"

# The options clamd parsed from deploy/clamd.conf: clamconf's clamd.conf section, which ends where
# freshclam.conf's begins.
settings=$(docker exec "$name" clamconf -c /etc/clamav)
settings=$(sed '/^Config file: freshclam.conf/q' <<< "$settings")
for setting in 'AlertExceedsMax = "yes"' 'ScanImage = "yes"' 'ScanImageFuzzyHash disabled' 'AlertBrokenMedia disabled' \
  'EnableShutdownCommand disabled' 'EnableReloadCommand disabled' 'EnableStatsCommand disabled' 'EnableVersionCommand = "yes"' \
  'ConcurrentDatabaseReload disabled' 'StreamMaxLength = "9437184"' 'MaxFileSize = "9437184"' 'MaxScanSize = "18874368"' \
  'TemporaryDirectory = "/tmp"' 'LeaveTemporaryFiles disabled' 'LogClean disabled' 'SelfCheck = "600"'; do
  grep -qxF "$setting" <<< "$settings" || fail "clamconf does not show $setting"
done
echo 'clamd.conf: limits, image options and disabled commands in force (clamconf)'
peak=$(docker exec "$name" cat /sys/fs/cgroup/memory.peak 2>/dev/null) || peak=''
echo "memory: $(docker stats --no-stream --format '{{.MemUsage}}' "$name")${peak:+, peak $((peak / 1048576)) MiB}"
echo 'Scanner container check passed.'
