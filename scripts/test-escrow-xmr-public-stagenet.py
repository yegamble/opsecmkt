#!/usr/bin/env python3
"""Manual, funded public-stagenet Monero escrow smoke (A-447/A-491).

Never CI and never fakechain. This script invokes only read-only daemon RPCs
before any wallet call. It fetches a recent block from the public stagenet
explorer API and checks its height/hash against the local daemon; a private
fork claiming stagenet is refused. It requires an already funded,
open, dedicated funder wallet and an *empty* host-mounted escrow wallet directory.
It sends 0.01 stagenet XMR through a real digital escrow and waits for the real
claim settlement to confirm. No block generation, clock changes, or deadline edits.

Required environment:
  ESCROW_XMR_PUBLIC_DAEMON_URL, ESCROW_XMR_PUBLIC_WALLET_URL,
  ESCROW_XMR_PUBLIC_FUNDER_URL     authenticated loopback HTTP RPC URLs
  ESCROW_XMR_PUBLIC_WALLET_DIR    initially empty host-mounted escrow wallet dir
  ESCROW_XMR_PUBLIC_NODE_IMAGE, ESCROW_XMR_PUBLIC_WALLET_IMAGE
                                  local pinned Docker images actually in use
  ESCROW_XMR_PUBLIC_NODE_CONTAINER, ESCROW_XMR_PUBLIC_ESCROW_CONTAINER,
  ESCROW_XMR_PUBLIC_FUNDER_CONTAINER
                                  running Docker containers checked against images;
                                  each RPC URL must match its published loopback port;
                                  escrow container must use the empty wallet dir
  ESCROW_XMR_PUBLIC_ANCHOR_HEIGHT, ESCROW_XMR_PUBLIC_ANCHOR_HASH,
  ESCROW_XMR_PUBLIC_ANCHOR_SOURCE
                                  near-tip public block URL on exactly
                                  stagenet.xmrchain.net or xmr-stagenet.librenode.com;
                                  this selects the same host's JSON block API
  TEST_DATABASE_URL               disposable loopback PostgreSQL, CREATEDB role
Optional: ESCROW_XMR_PUBLIC_WAIT_MINUTES (30..1440, default 480),
          ESCROW_XMR_PUBLIC_LOG (private raw Go log; default private temp file).

On success, writes docs/evidence/public-testnet/<UTC>-<commit12>/stagenet-xmr/
manifest.json and transcript.log. The latter contains only validated proof fields,
never raw Go output, secrets, addresses, seeds, passwords or credential URLs.
The requested-to-confirmed duration is one digital-claim sample. It does not
measure refund timing or validate the experimental 24-hour staff-alert
threshold (A-482).
"""

from __future__ import annotations

import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from urllib.parse import parse_qs, unquote, urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener
import ipaddress

ROOT = Path(__file__).resolve().parent.parent
HEX64 = re.compile(r"[0-9a-f]{64}\Z")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
TIMESTAMP = re.compile(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\Z")
IMAGE = re.compile(r"[a-zA-Z0-9./:_-]{1,200}\Z")
EVENTS = ("funding_submitted", "funding_confirmed", "settlement_requested",
          "settlement_built", "settlement_relayed", "settlement_confirmed")
MAX_RPC_BYTES = 2 * 1024 * 1024
MAX_EXPLORER_BYTES = 2 * 1024 * 1024
EXPLORER_HOSTS = frozenset(("stagenet.xmrchain.net", "xmr-stagenet.librenode.com"))


class Refused(Exception):
    """Fixed, credential-free refusal shown to the operator."""


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, newurl):
        return None


def loopback_url(raw: str, schemes: tuple[str, ...] = ("http",)) -> bool:
    try:
        p = urlsplit(raw)
        path_ok = (bool(p.path.strip("/")) if p.scheme in ("postgres", "postgresql") else not p.path.strip("/"))
        query_ok = (parse_qs(p.query) == {"sslmode": ["disable"]}
                    if p.scheme in ("postgres", "postgresql") else not p.query)
        return (p.scheme in schemes and bool(p.port) and path_ok
                and query_ok and not p.fragment
                and (p.hostname == "localhost" or ipaddress.ip_address(p.hostname).is_loopback))
    except (ValueError, TypeError):
        return False


def authenticated_loopback_url(raw: str) -> bool:
    if not loopback_url(raw):
        return False
    p = urlsplit(raw)
    return bool(p.username and p.password)


def daemon_refusal(info: dict, fork: dict, header: dict, now: datetime) -> str:
    if (info.get("nettype") != "stagenet" or not info.get("stagenet")
            or info.get("mainnet") or info.get("testnet")):
        return "daemon is not public stagenet"
    if not isinstance(fork.get("version"), int) or fork["version"] < 16:
        return "hard fork is below 16 or unreadable"
    if info.get("restricted"):
        return "restricted daemon RPC cannot prove peer count"
    if info.get("offline") or info.get("synchronized") is not True:
        return "daemon is offline or unsynchronized"
    for k in ("height", "target_height", "outgoing_connections_count", "incoming_connections_count"):
        if not isinstance(info.get(k), int) or isinstance(info[k], bool):
            return "daemon height or peer count is unreadable"
    if info["height"] <= 0 or info["target_height"] > info["height"]:
        return "daemon is behind its target"
    if info["outgoing_connections_count"] + info["incoming_connections_count"] <= 0:
        return "daemon has no peers"
    stamp = header.get("block_header", {}).get("timestamp")
    if not isinstance(stamp, int) or isinstance(stamp, bool) or stamp <= 0:
        return "tip time is unreadable"
    age = now.timestamp() - stamp
    if age > 3600 or age < -300:
        return "tip is stale or in the future"
    return ""


def anchor_source_ok(source: str, height: int, block_hash: str) -> bool:
    return (isinstance(source, str) and type(height) is int and height > 0
            and isinstance(block_hash, str) and HEX64.fullmatch(block_hash) is not None
            and any(source in (f"https://{host}/block/{block_hash}", f"https://{host}/block/{height}")
                    for host in EXPLORER_HOSTS))


def anchor_refusal(current_height: int, anchor_height: int, anchor_hash: str, source: str,
                   block: dict, require_recent: bool) -> str:
    if (type(anchor_height) is not int or anchor_height <= 0 or not isinstance(anchor_hash, str)
            or not HEX64.fullmatch(anchor_hash) or not anchor_source_ok(source, anchor_height, anchor_hash)):
        return "public anchor height, hash or explorer source is invalid"
    if type(current_height) is not int or current_height < anchor_height:
        return "public anchor is above the local daemon height"
    if require_recent and current_height - anchor_height > 20:
        return "public anchor is more than 20 blocks behind the daemon"
    header = block.get("block_header") if isinstance(block, dict) else None
    if not isinstance(header, dict) or header.get("hash") != anchor_hash or header.get("height") != anchor_height:
        return "local daemon block does not match the independent public anchor"
    return ""


def explorer_api_url(source: str, height: int, block_hash: str) -> str:
    if not anchor_source_ok(source, height, block_hash):
        raise Refused("public anchor source is not an approved explorer block URL")
    return f"https://{urlsplit(source).hostname}/api/block/{height}"


def explorer_refusal(height: int, block_hash: str, payload: dict) -> str:
    data = payload.get("data") if isinstance(payload, dict) and payload.get("status") == "success" else None
    if (not isinstance(data, dict) or type(data.get("block_height")) is not int
            or data["block_height"] != height or data.get("hash") != block_hash):
        return "public explorer block height or hash does not match the supplied anchor"
    return ""


def fetch_public_anchor(source: str, height: int, block_hash: str) -> dict:
    # The operator's source label is not evidence on its own. Fetch the explorer's
    # documented JSON API independently over HTTPS before touching either wallet.
    api = explorer_api_url(source, height, block_hash)
    req = Request(api, headers={"Accept": "application/json", "User-Agent": "opsecmkt-public-stagenet-proof/1"})
    try:
        with build_opener(ProxyHandler({}), NoRedirect()).open(req, timeout=30) as resp:
            if resp.status != 200:
                raise Refused("public explorer API is unavailable")
            body = resp.read(MAX_EXPLORER_BYTES + 1)
        if len(body) > MAX_EXPLORER_BYTES:
            raise Refused("public explorer response is too large")
        payload = json.loads(body)
    except (OSError, ValueError, UnicodeError) as exc:
        raise Refused("public explorer API is unreadable") from exc
    why = explorer_refusal(height, block_hash, payload)
    if why:
        raise Refused(why)
    return {"api_url": api, "height": height, "hash": block_hash,
            "fetched_at": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")}


def curl_config_value(value: str) -> str:
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in value):
        raise Refused("RPC login contains an unsupported control character")
    return '"' + value.replace('\\', '\\\\').replace('"', '\\"') + '"'


def rpc(raw: str, method: str, params: dict | None = None) -> dict:
    p = urlsplit(raw)
    host = f"[{p.hostname}]" if ":" in p.hostname else p.hostname
    clean = f"{p.scheme}://{host}:{p.port}/json_rpc"
    body = json.dumps({"jsonrpc": "2.0", "id": "public-smoke", "method": method,
                       "params": params or {}}, separators=(",", ":"))
    # Monero's actual Digest challenge was verified with curl; Python's urllib
    # Digest handler returned 401. Credentials go only through curl config stdin,
    # never argv, a file, ambient proxy, redirect, or public record.
    login = unquote(p.username or "") + ":" + unquote(p.password or "")
    config = "user = " + curl_config_value(login) + "\n"
    args = ["curl", "--disable", "--silent", "--show-error", "--fail", "--digest", "--config", "-",
            "--noproxy", "*", "--proto", "=http",
            "--connect-timeout", "5", "--max-time", "30", "--max-filesize", str(MAX_RPC_BYTES),
            "--request", "POST", "--header", "Content-Type: application/json",
            "--data-raw", body, clean]
    try:
        proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                stderr=subprocess.DEVNULL)
        try:
            proc.stdin.write(config.encode())
            proc.stdin.close()
            raw_result = proc.stdout.read(MAX_RPC_BYTES + 1)
            if len(raw_result) > MAX_RPC_BYTES:
                raise Refused("daemon RPC response is too large")
            if proc.wait(timeout=35) != 0:
                raise Refused(f"daemon {method} is unreadable")
        finally:
            if proc.poll() is None:
                proc.kill()
                proc.wait()
        result = json.loads(raw_result)
    except (OSError, ValueError, subprocess.SubprocessError) as exc:
        raise Refused(f"daemon {method} is unreadable") from exc
    if not isinstance(result, dict) or result.get("error") or not isinstance(result.get("result"), dict):
        raise Refused(f"daemon {method} is unreadable")
    return result["result"]


def preflight(raw: str, anchor_height: int, anchor_hash: str, source: str,
              require_recent: bool) -> tuple[dict, dict, dict]:
    try:
        info = rpc(raw, "get_info")
        fork = rpc(raw, "hard_fork_info")
        header = rpc(raw, "get_last_block_header")
        block = rpc(raw, "get_block_header_by_height", {"height": anchor_height})
    except Exception as exc:
        raise Refused("daemon preflight is unreadable") from exc
    why = daemon_refusal(info, fork, header, datetime.now(timezone.utc))
    if why:
        raise Refused(why)
    why = anchor_refusal(info["height"], anchor_height, anchor_hash, source, block, require_recent)
    if why:
        raise Refused(why)
    return info, fork, header


def docker_metadata(image: str, container: str, rpc_url: str, wallet_dir: Path | None = None,
                    forbidden_dir: Path | None = None) -> str:
    if not IMAGE.fullmatch(image):
        raise Refused("image name has an unexpected form")
    if not re.fullmatch(r"[a-zA-Z0-9_.-]{1,100}", container):
        raise Refused("container name has an unexpected form")
    try:
        # Some Docker clients decode RepoDigests as []interface{}, which join rejects.
        cp = subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}}|{{range .RepoDigests}}{{.}} {{end}}", image],
                            capture_output=True, text=True, check=True, timeout=30)
        running = subprocess.run(["docker", "inspect", "--format",
                                  "{{.Image}}|{{.State.Running}}|{{json .Mounts}}|{{json .NetworkSettings.Ports}}", container],
                                 capture_output=True, text=True, check=True, timeout=30)
    except (OSError, subprocess.SubprocessError) as exc:
        raise Refused("pinned running Docker image or container is unavailable") from exc
    image_id, sep, digests = cp.stdout.strip().partition("|")
    actual_id, sep2, rest = running.stdout.strip().partition("|")
    state, sep3, rest = rest.partition("|")
    mounts_json, sep4, ports_json = rest.partition("|")
    if not all((sep, sep2, sep3, sep4)) or not re.fullmatch(r"sha256:[0-9a-f]{64}", image_id) or actual_id != image_id or state != "true":
        raise Refused("running container does not use the pinned local image")
    try:
        ports = json.loads(ports_json)
        wanted = urlsplit(rpc_url)
        bindings = [(key, bind) for key, entries in ports.items() if key.endswith("/tcp") and isinstance(entries, list)
                    for bind in entries if isinstance(bind, dict) and bind.get("HostPort") == str(wanted.port)]
        host = wanted.hostname
        allowed_host_ips = {"127.0.0.1", "::1"} if host == "localhost" else {host}
        if len(bindings) != 1 or bindings[0][1].get("HostIp") not in allowed_host_ips:
            raise Refused("RPC URL does not name this container's published loopback port")
        key = bindings[0][0]
        if any(bind.get("HostIp") not in ("127.0.0.1", "::1") for bind in ports.get(key, []) if isinstance(bind, dict)):
            raise Refused("the RPC container port is also published outside loopback")
    except (ValueError, TypeError, AttributeError) as exc:
        raise Refused("running container port mapping is unreadable") from exc
    if wallet_dir is not None or forbidden_dir is not None:
        try:
            mounts = json.loads(mounts_json)
        except json.JSONDecodeError as exc:
            raise Refused("escrow wallet mount is unreadable") from exc
        if not isinstance(mounts, list):
            raise Refused("wallet mounts are unreadable")
        sources = {Path(m.get("Source", "")).resolve() for m in mounts
                   if isinstance(m, dict) and m.get("Type") == "bind"}
        if wallet_dir is not None and wallet_dir.resolve() not in sources:
            raise Refused("empty escrow wallet directory is not bind-mounted into escrow wallet-RPC")
        if forbidden_dir is not None and forbidden_dir.resolve() in sources:
            raise Refused("funder wallet-RPC shares the escrow wallet directory")
        if wallet_dir is not None:
            # Only the selected Args field is read into memory. Never print it: RPC login
            # arguments may contain credentials. Match --wallet-dir to the verified mount.
            try:
                cmd = subprocess.run(["docker", "inspect", "--format", "{{json .Args}}", container],
                                     capture_output=True, text=True, check=True, timeout=30)
                args = json.loads(cmd.stdout)
            except (OSError, subprocess.SubprocessError, json.JSONDecodeError) as exc:
                raise Refused("escrow wallet command's wallet directory is unreadable") from exc
            wallet_destinations = {m.get("Destination") for m in mounts if isinstance(m, dict)
                                   and m.get("Type") == "bind" and Path(m.get("Source", "")).resolve() == wallet_dir.resolve()}
            if not isinstance(args, list) or not all(isinstance(a, str) for a in args):
                raise Refused("escrow wallet command's wallet directory is unreadable")
            configured = ""
            for i, arg in enumerate(args):
                if arg == "--wallet-dir" and i + 1 < len(args):
                    configured = args[i + 1]
                elif arg.startswith("--wallet-dir="):
                    configured = arg.split("=", 1)[1]
            if configured not in wallet_destinations:
                raise Refused("escrow wallet --wallet-dir does not name the fresh bind mount")
    for item in digests.split():
        if re.fullmatch(r"[a-zA-Z0-9./:_-]{1,200}@sha256:[0-9a-f]{64}", item):
            return item
    raise Refused("local Docker image has no verified digest")


def validate_proof(proof: dict) -> None:
    if not isinstance(proof, dict) or set(proof) != {"network", "hard_fork", "start_height",
                      "anchor_height", "anchor_hash", "anchor_source", *EVENTS,
                      "escrow_wallet_rpc_version", "funder_wallet_rpc_version",
                      "funding_confirmations", "settlement_confirmations", "claim_received_piconero"}:
        raise Refused("Go proof has missing or extra fields")
    if proof["network"] != "stagenet" or type(proof["hard_fork"]) is not int or proof["hard_fork"] < 16:
        raise Refused("Go proof does not show stagenet hard fork 16+")
    if type(proof["start_height"]) is not int or proof["start_height"] <= 0:
        raise Refused("Go proof has an invalid height")
    if anchor_refusal(proof["start_height"], proof["anchor_height"], proof["anchor_hash"],
                      proof["anchor_source"], {"block_header": {"hash": proof["anchor_hash"],
                                                     "height": proof["anchor_height"]}}, True):
        raise Refused("Go proof lacks a recent, well-formed public anchor")
    if any(type(proof[k]) is not int or proof[k] <= 0 for k in
           ("escrow_wallet_rpc_version", "funder_wallet_rpc_version")):
        raise Refused("Go proof has an invalid wallet RPC version")
    if any(type(proof[k]) is not int or proof[k] < 10 for k in
           ("funding_confirmations", "settlement_confirmations")):
        raise Refused("Go proof lacks real confirmation counts")
    if type(proof["claim_received_piconero"]) is not int or not 0 < proof["claim_received_piconero"] < 10_000_000_000:
        raise Refused("Go proof lacks a funded claimant receipt")
    times, heights = [], []
    for name in EVENTS:
        event = proof[name]
        if not isinstance(event, dict) or set(event) != ({"at", "height"} if name == "settlement_requested" else {"at", "height", "txid"}):
            raise Refused("Go proof has an invalid event")
        if type(event["height"]) is not int or event["height"] < proof["start_height"]:
            raise Refused("Go proof has an invalid event height")
        heights.append(event["height"])
        if not isinstance(event["at"], str) or not TIMESTAMP.fullmatch(event["at"]):
            raise Refused("Go proof has an invalid timestamp")
        try:
            times.append(datetime.fromisoformat(event["at"].replace("Z", "+00:00")))
        except ValueError as exc:
            raise Refused("Go proof has an invalid timestamp") from exc
        if "txid" in event and (not isinstance(event["txid"], str) or not HEX64.fullmatch(event["txid"])):
            raise Refused("Go proof has an invalid transaction hash")
    if times != sorted(times):
        raise Refused("Go proof event times are out of order")
    if heights != sorted(heights):
        raise Refused("Go proof heights are out of order")
    if proof["funding_submitted"]["txid"] != proof["funding_confirmed"]["txid"]:
        raise Refused("funding transaction changed")
    if len({proof[n]["txid"] for n in EVENTS[3:]}) != 1:
        raise Refused("settlement transaction changed")
    if proof["settlement_built"]["txid"] == proof["funding_submitted"]["txid"]:
        raise Refused("funding and settlement hashes coincide")


def secret_scan(text: str, url_passwords: list[str]) -> None:
    for value in url_passwords:
        if value and value in text:
            raise Refused("record contains a configured RPC or database password")
    patterns = (r"[a-z][a-z0-9+.-]*://[^/@\s:]+:[^/@\s]+@", r"(?i)rpcpassword|rpcauth=|private.key|mnemonic|seed.phrase|wallet.password",
                r"-----BEGIN PGP PRIVATE KEY BLOCK-----", r"(?i)\b(?:xprv|tprv)[1-9A-HJ-NP-Za-km-z]{100,}")
    if any(re.search(p, text) for p in patterns):
        raise Refused("record contains secret-shaped material")


def run() -> None:
    needed = ("ESCROW_XMR_PUBLIC_DAEMON_URL", "ESCROW_XMR_PUBLIC_WALLET_URL", "ESCROW_XMR_PUBLIC_FUNDER_URL",
              "ESCROW_XMR_PUBLIC_WALLET_DIR", "ESCROW_XMR_PUBLIC_NODE_IMAGE", "ESCROW_XMR_PUBLIC_WALLET_IMAGE",
              "TEST_DATABASE_URL", "ESCROW_XMR_PUBLIC_NODE_CONTAINER", "ESCROW_XMR_PUBLIC_ESCROW_CONTAINER",
              "ESCROW_XMR_PUBLIC_FUNDER_CONTAINER", "ESCROW_XMR_PUBLIC_ANCHOR_HEIGHT",
              "ESCROW_XMR_PUBLIC_ANCHOR_HASH", "ESCROW_XMR_PUBLIC_ANCHOR_SOURCE")
    values = {k: os.environ.get(k, "") for k in needed}
    if any(not v for v in values.values()):
        raise Refused("required public smoke environment is incomplete")
    for k in needed[:3]:
        if not authenticated_loopback_url(values[k]):
            raise Refused(f"{k} must be an authenticated loopback HTTP endpoint")
    if len({urlsplit(values[k]).port for k in needed[:3]}) != 3:
        raise Refused("daemon, escrow and funder RPC endpoints must be distinct")
    if not loopback_url(values["TEST_DATABASE_URL"], ("postgres", "postgresql")):
        raise Refused("TEST_DATABASE_URL must be a disposable loopback PostgreSQL database")
    wallet_dir = Path(values["ESCROW_XMR_PUBLIC_WALLET_DIR"])
    if not wallet_dir.is_dir() or any(wallet_dir.iterdir()):
        raise Refused("escrow wallet directory must exist and be empty before the run")
    try:
        wait = int(os.environ.get("ESCROW_XMR_PUBLIC_WAIT_MINUTES", "480"))
    except ValueError as exc:
        raise Refused("wait must be 30–1440 minutes") from exc
    if not 30 <= wait <= 1440:
        raise Refused("wait must be 30–1440 minutes")
    try:
        anchor_height = int(values["ESCROW_XMR_PUBLIC_ANCHOR_HEIGHT"])
    except ValueError as exc:
        raise Refused("public anchor height is invalid") from exc
    anchor_hash = values["ESCROW_XMR_PUBLIC_ANCHOR_HASH"]
    anchor_source = values["ESCROW_XMR_PUBLIC_ANCHOR_SOURCE"]
    if not anchor_source_ok(anchor_source, anchor_height, anchor_hash) or not HEX64.fullmatch(anchor_hash):
        raise Refused("public anchor source or hash is invalid")
    started = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    info, fork, header = preflight(values["ESCROW_XMR_PUBLIC_DAEMON_URL"], anchor_height,
                                   anchor_hash, anchor_source, True) # no wallet call yet
    explorer = fetch_public_anchor(anchor_source, anchor_height, anchor_hash) # HTTPS, before wallet RPC
    node_version = info.get("version")
    if not isinstance(node_version, str) or not re.fullmatch(r"[A-Za-z0-9.()_/-]{1,80}", node_version):
        raise Refused("daemon version is missing or malformed")
    node_digest = docker_metadata(values["ESCROW_XMR_PUBLIC_NODE_IMAGE"], values["ESCROW_XMR_PUBLIC_NODE_CONTAINER"],
                                  values["ESCROW_XMR_PUBLIC_DAEMON_URL"])
    wallet_digest = docker_metadata(values["ESCROW_XMR_PUBLIC_WALLET_IMAGE"], values["ESCROW_XMR_PUBLIC_ESCROW_CONTAINER"],
                                    values["ESCROW_XMR_PUBLIC_WALLET_URL"], wallet_dir)
    if docker_metadata(values["ESCROW_XMR_PUBLIC_WALLET_IMAGE"], values["ESCROW_XMR_PUBLIC_FUNDER_CONTAINER"],
                       values["ESCROW_XMR_PUBLIC_FUNDER_URL"],
                       forbidden_dir=wallet_dir) != wallet_digest:
        raise Refused("funder wallet-RPC uses a different image digest")
    if subprocess.run(["git", "status", "--porcelain"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.strip():
        raise Refused("checkout must be clean before a public proof")
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    if not HEX40.fullmatch(commit):
        raise Refused("git commit is unreadable")
    day = datetime.now(timezone.utc).strftime("%Y-%m-%d")
    out_dir = ROOT / "docs/evidence/public-testnet" / f"{day}-{commit[:12]}" / "stagenet-xmr"
    if out_dir.exists():
        raise Refused("record already exists; public evidence is never overwritten")
    raw_path = Path(os.environ.get("ESCROW_XMR_PUBLIC_LOG", "")) if os.environ.get("ESCROW_XMR_PUBLIC_LOG") else None
    if raw_path is None:
        fd, name = tempfile.mkstemp(prefix="xmr-public-", suffix=".private.log")
        os.close(fd)
        raw_path = Path(name)
        raw_mode = "w"
    elif raw_path.exists():
        raise Refused("private raw log path already exists")
    else:
        raw_mode = "x"
    if raw_path.resolve().is_relative_to(ROOT):
        raise Refused("private raw log must be outside the repository")
    raw_path.parent.mkdir(parents=True, exist_ok=True)
    old_umask = os.umask(0o077)
    try:
        with raw_path.open(raw_mode, encoding="utf-8") as raw:
            env = os.environ.copy()
            env["ESCROW_XMR_PUBLIC_RUN"] = "I_ACKNOWLEDGE_STAGENET_FUNDS"
            env["ESCROW_XMR_PUBLIC_WAIT_MINUTES"] = str(wait)
            try:
                cp = subprocess.run(["go", "test", "-count=1", "-timeout", f"{2*wait+60}m", "-run",
                                     "^TestXMREscrowPublicStagenetSmoke$", "-v", "./internal/market/"],
                                    cwd=ROOT, env=env, stdout=raw, stderr=subprocess.STDOUT,
                                    timeout=(2*wait+65)*60, check=False)
            except (OSError, subprocess.TimeoutExpired) as exc:
                raise Refused("public Go smoke could not finish within the bounded wait") from exc
    finally:
        os.umask(old_umask)
    text = raw_path.read_text(encoding="utf-8", errors="replace")
    if cp.returncode or "--- PASS: TestXMREscrowPublicStagenetSmoke " not in text or "--- SKIP: TestXMREscrowPublicStagenetSmoke " in text:
        raise Refused("public Go smoke did not pass; inspect the private raw log locally")
    lines = [line.split("xmr-public: proof ", 1)[1] for line in text.splitlines() if "xmr-public: proof " in line]
    if len(lines) != 1:
        raise Refused("public Go smoke did not emit exactly one proof")
    try:
        proof = json.loads(lines[0])
    except json.JSONDecodeError as exc:
        raise Refused("public Go proof is malformed") from exc
    validate_proof(proof)
    if (proof["anchor_height"], proof["anchor_hash"], proof["anchor_source"]) != (anchor_height, anchor_hash, anchor_source):
        raise Refused("Go proof does not match the supplied independent anchor")
    end_info, end_fork, end_header = preflight(values["ESCROW_XMR_PUBLIC_DAEMON_URL"], anchor_height,
                                               anchor_hash, anchor_source, False)
    finished = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    if end_info["height"] < info["height"] or end_fork["version"] < 16:
        raise Refused("public node moved backward before proof completion")
    transcript = "xmr-public: proof " + json.dumps(proof, sort_keys=True, separators=(",", ":")) + "\nPASS: one digital claim confirmed on public stagenet\n"
    passwords = [unquote(urlsplit(values[k]).password or "") for k in (*needed[:3], "TEST_DATABASE_URL")]
    secret_scan(transcript, passwords)
    manifest = {
        "record": "opsecmkt Monero escrow public-stagenet digital claim smoke (A-447/A-491)",
        "network": "stagenet", "commit": commit, "started": started, "finished": finished,
        "public_anchor": {"height": anchor_height, "hash": anchor_hash, "source": anchor_source,
                          "explorer_api_url": explorer["api_url"],
                          "explorer_height": explorer["height"], "explorer_hash": explorer["hash"],
                          "explorer_fetched_at": explorer["fetched_at"],
                          "maximum_initial_lag_blocks": 20},
        "node": {"version": node_version, "image": values["ESCROW_XMR_PUBLIC_NODE_IMAGE"], "digest": node_digest,
                 "hard_fork": fork["version"], "start_height": info["height"], "end_height": end_info["height"],
                 "start_target_height": info["target_height"], "end_target_height": end_info["target_height"],
                 "start_peers": info["outgoing_connections_count"] + info["incoming_connections_count"],
                 "end_peers": end_info["outgoing_connections_count"] + end_info["incoming_connections_count"],
                 "synchronized_at_both_checks": True,
                 "start_tip_timestamp": header["block_header"]["timestamp"],
                 "end_tip_timestamp": end_header["block_header"]["timestamp"]},
        "wallet_rpc": {"image": values["ESCROW_XMR_PUBLIC_WALLET_IMAGE"], "digest": wallet_digest,
                       "escrow_rpc_version": proof["escrow_wallet_rpc_version"],
                       "funder_rpc_version": proof["funder_wallet_rpc_version"]},
        "rpc_bindings": {"daemon": {"container": values["ESCROW_XMR_PUBLIC_NODE_CONTAINER"],
                                    "loopback_port": urlsplit(values["ESCROW_XMR_PUBLIC_DAEMON_URL"]).port},
                         "escrow_wallet": {"container": values["ESCROW_XMR_PUBLIC_ESCROW_CONTAINER"],
                                           "loopback_port": urlsplit(values["ESCROW_XMR_PUBLIC_WALLET_URL"]).port},
                         "funder_wallet": {"container": values["ESCROW_XMR_PUBLIC_FUNDER_CONTAINER"],
                                           "loopback_port": urlsplit(values["ESCROW_XMR_PUBLIC_FUNDER_URL"]).port}},
        "flow": "digital checkout, actual funding, vendor claim, watcher build/relay, confirmed release",
        "price_piconero": 10_000_000_000, "fresh_escrow_wallet_directory_verified": True, "proof": proof,
        "a482_measurement": {"observed_requested_to_confirmed_seconds": int(
            (datetime.fromisoformat(proof["settlement_confirmed"]["at"].replace("Z", "+00:00")) -
             datetime.fromisoformat(proof["settlement_requested"]["at"].replace("Z", "+00:00"))).total_seconds()),
            "threshold_validated": False, "note": "digital claim only; refund timing unmeasured and one sample does not validate a 24-hour alert threshold"},
        "transcript": {"file": "transcript.log", "sha256": hashlib.sha256(transcript.encode()).hexdigest()},
        "secret_scan": "passed",
    }
    serialized = json.dumps(manifest, indent=2, sort_keys=True) + "\n"
    secret_scan(serialized, passwords)
    out_dir.mkdir(parents=True)
    try:
        (out_dir / "transcript.log").write_text(transcript, encoding="utf-8")
        (out_dir / "manifest.json").write_text(serialized, encoding="utf-8")
        secret_scan((out_dir / "transcript.log").read_text() + (out_dir / "manifest.json").read_text(), passwords)
    except Exception:
        for file in out_dir.iterdir():
            file.unlink()
        out_dir.rmdir()
        raise
    print(f"PASS: public stagenet claim; record {out_dir.relative_to(ROOT)}; raw private log {raw_path}")


if __name__ == "__main__":
    try:
        run()
    except Refused as exc:
        print(f"REFUSED: {exc}", file=sys.stderr)
        raise SystemExit(1)
