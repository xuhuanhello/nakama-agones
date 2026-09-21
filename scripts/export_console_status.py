#!/usr/bin/env python3
"""Publish a credential-free Fleet status projection for the local console user."""
import argparse
from contextlib import contextmanager
import fcntl
import grp
import http.client
import ipaddress
import json
import math
import os
from pathlib import Path
import re
import secrets
import signal
import ssl
import stat
import sys
from urllib.parse import urlsplit


ROOT_UID = 0
MAX_RESPONSE = 8 << 20
MAX_CREDENTIALS = 1 << 20
REQUEST_TIMEOUT = 4
STATUS_PATH = "/agones/fleet/v1/admin/status"
TERMINAL = {"completed", "cancelled", "expired", "failed"}
INTEGER_METRICS = ("simulation_pending", "simulation_active", "memory_bytes", "audit_pending", "audit_active", "pending_results")
FLOAT_METRICS = ("simulation_oldest_seconds", "frame_p99_ms")
REDACTED = "[REDACTED: sensitive status text]"
CREDENTIAL_LINE = re.compile(
    r'''(token|password|passwd|secret|credential|authorization|key|dsn|connection[_ -]?string|boot[_-]?id|reservation[_-]?id)[\s"'\\]*[:=]'''
    r'''|(?:bearer|basic)\s+[A-Za-z0-9._~+/=-]+|postgres(?:ql)?://'''
    r'''|\b[a-z][a-z0-9+.-]{0,31}://[^\s/@:]{1,256}:[^\s@]{1,4096}@'''
    r'''|(?<![A-Za-z0-9_-])[A-Za-z0-9_-]{10,32768}\.[A-Za-z0-9_-]{10,32768}\.[A-Za-z0-9_-]{10,32768}'''
    r'''|\bpf(?:client)?_[A-Za-z0-9_-]{16,}|-----[A-Z ]*PRIVATE KEY-----''', re.IGNORECASE)


class ExportError(Exception):
    """Arguments must be fixed error codes, never remote text or exception text."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        raise ExportError("invalid_arguments")


def absolute_path(value):
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts or any(ord(char) < 32 for char in str(path)):
        raise ExportError("invalid_path")
    return path


def parse_url(value):
    if not isinstance(value, str) or len(value) > 512 or any(ord(char) <= 32 for char in value):
        raise ExportError("invalid_upstream_url")
    try:
        url = urlsplit(value)
        if (url.scheme not in ("http", "https") or not url.hostname or url.username is not None or url.password is not None
                or url.path not in ("", "/") or "?" in value or "#" in value or "%" in value
                or not ipaddress.ip_address(url.hostname).is_loopback):
            raise ValueError()
        port = url.port or (443 if url.scheme == "https" else 80)
        if url.port == 0 or not 1 <= port <= 65535:
            raise ValueError()
    except ValueError:
        raise ExportError("invalid_upstream_url") from None
    return url.scheme, url.hostname, port


def decode_object(raw, limit):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise ExportError("invalid_json")
            result[key] = value
        return result

    def invalid_constant(value):
        raise ExportError("invalid_json")

    if not raw or len(raw) > limit:
        raise ExportError("response_size_limit")
    try:
        value = json.loads(raw, object_pairs_hook=unique, parse_constant=invalid_constant)
    except (ValueError, UnicodeError, RecursionError):
        raise ExportError("invalid_json") from None
    if not isinstance(value, dict):
        raise ExportError("invalid_json")
    return value


def read_admin_token(path):
    try:
        fd = os.open(absolute_path(path), os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK)
    except OSError:
        raise ExportError("credentials_unavailable") from None
    try:
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != ROOT_UID or stat.S_IMODE(info.st_mode) != 0o600
                or info.st_nlink != 1 or not 0 < info.st_size <= MAX_CREDENTIALS):
            raise ExportError("unsafe_credentials_file")
        with os.fdopen(fd, "rb", closefd=False) as reader:
            value = decode_object(reader.read(MAX_CREDENTIALS + 1), MAX_CREDENTIALS)
    finally:
        os.close(fd)
    token = value.get("admin_token")
    if not isinstance(token, str) or not 1 <= len(token) <= 32768 or any(not 32 < ord(char) < 127 for char in token):
        raise ExportError("invalid_admin_token")
    return token


@contextmanager
def network_deadline(seconds):
    # A socket timeout alone can be extended indefinitely by a slow response.
    # This standalone Linux/root timer runs on the main thread; cap the entire
    # connect + headers + body operation, including continuously trickled data.
    def expired(signum, frame):
        raise ExportError("upstream_timeout")

    previous = signal.signal(signal.SIGALRM, expired)
    old_timer = signal.setitimer(signal.ITIMER_REAL, seconds)
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)
        if old_timer[0] > 0:
            signal.setitimer(signal.ITIMER_REAL, *old_timer)


def fetch_status(base_url, token):
    scheme, host, port = parse_url(base_url)
    # http.client never uses environment proxies and never follows redirects.
    if scheme == "https":
        context = ssl.create_default_context()
        context.minimum_version = ssl.TLSVersion.TLSv1_2
        connection = http.client.HTTPSConnection(host, port, timeout=REQUEST_TIMEOUT, context=context)
    else:
        connection = http.client.HTTPConnection(host, port, timeout=REQUEST_TIMEOUT)
    try:
        with network_deadline(REQUEST_TIMEOUT):
            connection.request("GET", STATUS_PATH, headers={"Authorization": "Bearer " + token,
                                                           "Accept": "application/json", "Connection": "close"})
            response = connection.getresponse()
            if response.status != 200:
                raise ExportError("upstream_status_rejected")
            if response.getheader("Content-Encoding", "identity").lower() != "identity":
                raise ExportError("upstream_encoding_rejected")
            declared = response.getheader("Content-Length")
            if declared is not None:
                try:
                    if not 0 <= int(declared) <= MAX_RESPONSE:
                        raise ValueError()
                except ValueError:
                    raise ExportError("response_size_limit") from None
            raw = response.read(MAX_RESPONSE + 1)
        return decode_object(raw, MAX_RESPONSE)
    except (OSError, http.client.HTTPException):
        raise ExportError("upstream_unavailable") from None
    finally:
        connection.close()


def string(value, secret_values):
    if value is None:
        return ""
    if not isinstance(value, str):
        raise ExportError("invalid_state_field")
    if CREDENTIAL_LINE.search(value) or any(secret and secret in value for secret in secret_values):
        return REDACTED
    # Inspect for secrets before truncation, and encode JSON rather than HTML.
    return value if len(value) <= 16384 else value[:16384] + " [truncated]"


def integer(value):
    if value is None:
        return 0
    if type(value) is not int or not -(1 << 63) <= value < (1 << 63):
        raise ExportError("invalid_state_field")
    return value


def number(value):
    if value is None:
        return 0
    if type(value) not in (int, float) or not math.isfinite(value) or value < 0:
        raise ExportError("invalid_state_field")
    return value


def boolean(value):
    if value is None:
        return False
    if type(value) is not bool:
        raise ExportError("invalid_state_field")
    return value


def object_map(value):
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise ExportError("invalid_state_field")
    if any(item is not None and not isinstance(item, dict) for item in value.values()):
        raise ExportError("invalid_state_field")
    return value


def array(value):
    if value is None:
        return []
    if not isinstance(value, list):
        raise ExportError("invalid_state_field")
    return value


def project_status(state, secret_values=()):
    # A successful HTTP response containing an error object is not fresh state.
    if not isinstance(state, dict) or not {"revision", "workers", "allocations"}.issubset(state):
        raise ExportError("invalid_state")
    workers = object_map(state["workers"])
    allocations = object_map(state["allocations"])
    result = {"ok": True, "revision": integer(state["revision"]),
              "creation_blocked_reason": string(state.get("creation_blocked_reason"), secret_values),
              "workers": [], "rooms": []}
    occupied = {}
    for allocation in allocations.values():
        if allocation is not None and allocation.get("state") not in TERMINAL:
            worker_id = allocation.get("worker_id", "")
            if not isinstance(worker_id, str):
                raise ExportError("invalid_state_field")
            occupied[worker_id] = occupied.get(worker_id, 0) + 1
    for key in sorted(workers):
        worker = workers[key]
        if worker is None:
            continue
        item = {key: string(worker.get(key), secret_values) for key in ("id", "region", "build_hash", "state", "host", "error")}
        item.update({key: integer(worker.get(key)) for key in ("port", "max_rooms", "player_count", "created_at", "last_heartbeat")})
        item.update({key: boolean(worker.get(key)) for key in ("ready", "draining")})
        item["pod"] = "nag-" + item["id"] if re.fullmatch(r"[a-f0-9]{32}", item["id"]) else ""
        item["occupied_rooms"] = occupied.get(worker.get("id", ""), 0)
        item["metrics"] = None
        if item["last_heartbeat"] > 0:
            metrics = worker.get("metrics")
            if not isinstance(metrics, dict):
                raise ExportError("invalid_state_metrics")
            item["metrics"] = {key: integer(metrics.get(key)) for key in INTEGER_METRICS}
            item["metrics"].update({key: number(metrics.get(key)) for key in FLOAT_METRICS})
        result["workers"].append(item)
    ordered = [allocation for allocation in allocations.values() if allocation is not None]
    ordered.sort(key=lambda item: integer(item.get("created_at")), reverse=True)
    for allocation in ordered:
        item = {"id": string(allocation.get("room_id"), secret_values)}
        item.update({key: string(allocation.get(key), secret_values) for key in ("allocation_id", "worker_id", "state", "error")})
        item.update({key: integer(allocation.get(key)) for key in ("epoch", "created_at", "expires_at", "terminal_at")})
        worker_id = allocation.get("worker_id", "")
        if not isinstance(worker_id, str):
            raise ExportError("invalid_state_field")
        worker = workers.get(worker_id) or {}
        item["region"] = string(worker.get("region"), secret_values)
        item["players"] = []
        seen = set()
        for session in array(allocation.get("sessions")):
            if not isinstance(session, dict):
                raise ExportError("invalid_state_field")
            user_id = session.get("user_id", "")
            player = {"user_id": string(user_id, secret_values), "seat": integer(session.get("seat")),
                      "connected": boolean(session.get("connected")), "ever_connected": boolean(session.get("ever_connected")),
                      "reconnect_until": integer(session.get("reconnect_until"))}
            seen.add(user_id)
            item["players"].append(player)
        for seat, user_id in enumerate(array(allocation.get("user_ids"))):
            safe_id = string(user_id, secret_values)
            if user_id not in seen:
                item["players"].append({"user_id": safe_id, "seat": seat, "connected": False,
                                        "ever_connected": False, "reconnect_until": 0})
        result["rooms"].append(item)
    return result


def group_id(value):
    if not re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", value):
        raise ExportError("invalid_group")
    try:
        return grp.getgrnam(value).gr_gid
    except KeyError:
        raise ExportError("group_unavailable") from None


@contextmanager
def output_directory(output, gid):
    path = absolute_path(output)
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]{0,127}", path.name):
        raise ExportError("invalid_output_name")
    try:
        fd = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    except OSError:
        raise ExportError("output_directory_unavailable") from None
    try:
        info = os.fstat(fd)
        if info.st_uid != ROOT_UID or info.st_gid != gid or stat.S_IMODE(info.st_mode) != 0o750:
            raise ExportError("unsafe_output_directory")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ExportError("export_busy") from None
        try:
            existing = os.stat(path.name, dir_fd=fd, follow_symlinks=False)
        except FileNotFoundError:
            existing = None
        if existing is not None and (not stat.S_ISREG(existing.st_mode) or existing.st_uid != ROOT_UID
                                     or existing.st_gid != gid or existing.st_nlink != 1
                                     or stat.S_IMODE(existing.st_mode) != 0o640):
            raise ExportError("unsafe_output_file")
        yield fd, path.name
    finally:
        os.close(fd)


def publish(directory_fd, name, gid, projection):
    content = (json.dumps(projection, sort_keys=True, separators=(",", ":"), allow_nan=False) + "\n").encode("ascii")
    if len(content) > MAX_RESPONSE:
        raise ExportError("projection_size_limit")
    staged = ".fleet-status-" + secrets.token_hex(12)
    fd = os.open(staged, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=directory_fd)
    try:
        os.fchown(fd, ROOT_UID, gid)
        os.fchmod(fd, 0o640)
        with os.fdopen(fd, "wb", closefd=False) as writer:
            writer.write(content)
            writer.flush()
            os.fsync(fd)
        # This is a refreshable status cache, not a transaction ledger. There
        # are no fallible writes after the single atomic publication point.
        os.replace(staged, name, src_dir_fd=directory_fd, dst_dir_fd=directory_fd)
        staged = None
    finally:
        os.close(fd)
        if staged is not None:
            os.unlink(staged, dir_fd=directory_fd)


def export_status(args):
    if os.geteuid() != ROOT_UID:
        raise ExportError("root_required")
    parse_url(args.url)
    gid = group_id(args.group)
    with output_directory(args.output, gid) as (directory_fd, name):
        token = read_admin_token(args.credentials)
        projection = project_status(fetch_status(args.url, token), secret_values=(token,))
        publish(directory_fd, name, gid, projection)
    return projection


def main(argv=None):
    parser = Parser(description=__doc__)
    parser.add_argument("--credentials", type=absolute_path, default=Path("/opt/nakama-agones/agones-secrets.json"))
    parser.add_argument("--url", default="http://127.0.0.1:7350")
    parser.add_argument("--output", type=absolute_path, default=Path("/var/lib/fleet-console/status/fleet.json"))
    parser.add_argument("--group", default="fleet-console")
    try:
        result = export_status(parser.parse_args(argv))
        print(json.dumps({"ok": True, "code": "status_exported", "revision": result["revision"],
                          "workers": len(result["workers"]), "rooms": len(result["rooms"])}))
        return 0
    except (ExportError, OSError, ValueError, TypeError, KeyError, OverflowError, RecursionError) as error:
        print(json.dumps({"ok": False, "code": str(error) if isinstance(error, ExportError) else "export_failed"}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
