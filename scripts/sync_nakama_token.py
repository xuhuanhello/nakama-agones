#!/usr/bin/env python3
"""Pull a scoped Kubernetes token over pinned SSH without exposing its contents."""
import argparse
import base64
from contextlib import contextmanager
from datetime import datetime, timezone
import fcntl
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import selectors
import stat
import subprocess
import sys
import time
from urllib.parse import urlsplit


MAX_BUNDLE_BYTES = 65536
MAX_TOKEN_BYTES = 32768
MAX_REMAINING_SECONDS = 7200
REMOTE_REQUEST = "nakama-agones-token-bundle-v1"
TOKEN_FILE = "token"
METADATA_FILE = "token.metadata.json"
BUNDLE_KEYS = {"schema", "api_url", "namespace", "service_account", "token", "expires_at"}
METADATA_KEYS = (BUNDLE_KEYS - {"token"}) | {"installed_at", "token_sha256"}
JWT_PATTERN = re.compile(r"[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\Z")
LABEL_PATTERN = re.compile(r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?\Z")


class SyncError(Exception):
    """Only fixed, non-secret error codes may be passed to this exception."""


class Parser(argparse.ArgumentParser):
    def error(self, message):
        # argparse normally repeats the invalid argument, which may be sensitive.
        raise SyncError("invalid_arguments")


def utc_now():
    return datetime.now(timezone.utc)


def timestamp(value):
    if not isinstance(value, str) or len(value) > 40:
        raise SyncError("invalid_expiration")
    try:
        result = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError:
        raise SyncError("invalid_expiration") from None
    if result.tzinfo is None:
        raise SyncError("invalid_expiration")
    return result.astimezone(timezone.utc)


def iso(value):
    return value.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")


def strict_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            if key in result:
                raise SyncError("invalid_json")
            result[key] = value
        return result

    def nonfinite(value):
        raise SyncError("invalid_json")

    if not isinstance(raw, bytes) or not 0 < len(raw) <= MAX_BUNDLE_BYTES:
        raise SyncError("bundle_size")
    try:
        result = json.loads(raw, object_pairs_hook=unique, parse_constant=nonfinite)
    except (ValueError, UnicodeError, RecursionError):
        raise SyncError("invalid_json") from None
    if not isinstance(result, dict):
        raise SyncError("invalid_json")
    return result


def host_name(value):
    if not isinstance(value, str) or not value or len(value) > 253 or "%" in value:
        raise SyncError("invalid_ssh_host")
    try:
        ipaddress.ip_address(value)
        return value
    except ValueError:
        pass
    labels = value.split(".")
    if not all(re.fullmatch(r"[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?", part) for part in labels):
        raise SyncError("invalid_ssh_host")
    return value


def api_url(value):
    if not isinstance(value, str) or len(value) > 512 or any(ord(c) <= 32 for c in value):
        raise SyncError("invalid_api_url")
    try:
        parsed = urlsplit(value)
        if (parsed.scheme != "https" or not parsed.hostname or parsed.username is not None
                or parsed.password is not None or parsed.query or parsed.fragment or parsed.path not in ("", "/")
                or (parsed.port is not None and not 1 <= parsed.port <= 65535)):
            raise SyncError("invalid_api_url")
        host_name(parsed.hostname)
    except (ValueError, SyncError):
        raise SyncError("invalid_api_url") from None
    return "https://" + parsed.netloc.lower()


def identity_label(value):
    if not isinstance(value, str) or not LABEL_PATTERN.fullmatch(value):
        raise SyncError("invalid_identity")
    return value


def absolute_path(value):
    path = Path(value)
    if not path.is_absolute() or ".." in path.parts or any(ord(c) < 32 for c in str(path)):
        raise SyncError("invalid_path")
    return path


def token_claims(token, namespace, service_account, expires):
    if not isinstance(token, str) or len(token) > MAX_TOKEN_BYTES or not JWT_PATTERN.fullmatch(token):
        raise SyncError("invalid_token")
    try:
        payload = token.split(".")[1]
        claims = strict_json(base64.b64decode(payload + "=" * (-len(payload) % 4), altchars=b"-_", validate=True))
    except (ValueError, SyncError):
        raise SyncError("invalid_token") from None
    if claims.get("sub") != "system:serviceaccount:" + namespace + ":" + service_account:
        raise SyncError("token_identity_mismatch")
    expiration = claims.get("exp")
    if type(expiration) is not int or abs(expiration - expires.timestamp()) > 1:
        raise SyncError("token_expiration_mismatch")
    return claims


def validate_bundle(raw, expected_api_url, namespace, service_account, now, minimum_remaining=900):
    bundle = strict_json(raw)
    if set(bundle) != BUNDLE_KEYS or type(bundle.get("schema")) is not int or bundle["schema"] != 1:
        raise SyncError("invalid_schema")
    expected = api_url(expected_api_url)
    identity_label(namespace)
    identity_label(service_account)
    if (api_url(bundle["api_url"]) != expected or bundle["namespace"] != namespace
            or bundle["service_account"] != service_account):
        raise SyncError("bundle_identity_mismatch")
    expires = timestamp(bundle["expires_at"])
    remaining = (expires - now).total_seconds()
    if remaining < minimum_remaining:
        raise SyncError("token_too_close_to_expiry")
    if remaining > MAX_REMAINING_SECONDS:
        raise SyncError("token_lifetime_too_long")
    claims = token_claims(bundle["token"], namespace, service_account, expires)
    for field in ("nbf", "iat"):
        if field in claims and (type(claims[field]) is not int or claims[field] > now.timestamp() + 30):
            raise SyncError("token_not_yet_valid")
    # Pinned SSH authenticates the issuing host. This only validates claims;
    # the Kubernetes API, not this receiver, verifies the JWT signature on use.
    bundle["api_url"] = expected
    bundle["expires_at"] = iso(expires)
    return bundle


def checked_ssh_file(path, private):
    path = absolute_path(path)
    # OpenSSH parses this path as an option value; disallow expansion/quoting.
    if any(c.isspace() or c in "%\"'\\" for c in str(path)):
        raise SyncError("invalid_ssh_path")
    try:
        info = path.lstat()
    except OSError:
        raise SyncError("ssh_file_unavailable") from None
    mode = stat.S_IMODE(info.st_mode)
    if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid() or info.st_nlink != 1
            or (private and mode not in (0o400, 0o600)) or (not private and mode & 0o022)):
        raise SyncError("unsafe_ssh_file")
    if info.st_size == 0:
        raise SyncError("empty_ssh_file")
    return str(path)


def ssh_command(args):
    host = host_name(args.ssh_host)
    if not re.fullmatch(r"[a-z_][a-z0-9_-]{0,31}", args.ssh_user):
        raise SyncError("invalid_ssh_user")
    if not 1 <= args.ssh_port <= 65535:
        raise SyncError("invalid_ssh_port")
    key = checked_ssh_file(args.ssh_key_file, private=True)
    known_hosts = checked_ssh_file(args.known_hosts, private=False)
    return ["/usr/bin/ssh", "-F", "/dev/null", "-T", "-l", args.ssh_user, "-p", str(args.ssh_port), "-i", key,
            "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + known_hosts,
            "-o", "GlobalKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes", "-o", "IdentityAgent=none",
            "-o", "PasswordAuthentication=no", "-o", "KbdInteractiveAuthentication=no", "-o", "ConnectTimeout=10",
            "-o", "ClearAllForwardings=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none",
            "-o", "ProxyCommand=none", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=1",
            host, REMOTE_REQUEST]


def fetch_bundle(args, timeout=30):
    command = ssh_command(args)
    try:
        child = subprocess.Popen(command, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                                 shell=False, env={"PATH": "/usr/bin:/bin", "LC_ALL": "C"})
    except OSError:
        raise SyncError("ssh_unavailable") from None
    try:
        deadline = time.monotonic() + timeout
        content = bytearray()
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout, selectors.EVENT_READ)
            while True:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise SyncError("ssh_timeout")
                if not selector.select(remaining):
                    raise SyncError("ssh_timeout")
                chunk = os.read(child.stdout.fileno(), 4096)
                if not chunk:
                    break
                content.extend(chunk)
                if len(content) > MAX_BUNDLE_BYTES:
                    raise SyncError("bundle_size")
        try:
            code = child.wait(timeout=max(0.001, deadline - time.monotonic()))
        except subprocess.TimeoutExpired:
            raise SyncError("ssh_timeout") from None
        if code != 0:
            raise SyncError("ssh_failed")
        return bytes(content)
    finally:
        if child.poll() is None:
            child.kill()
        child.wait()
        child.stdout.close()


class CredentialStore:
    """All opens/renames use the same already checked directory descriptor."""
    def __init__(self, directory):
        self.directory = absolute_path(directory)
        self.fd = None

    def __enter__(self):
        try:
            self.fd = os.open(self.directory, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
        except OSError:
            raise SyncError("credential_directory_unavailable") from None
        info = os.fstat(self.fd)
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid() or stat.S_IMODE(info.st_mode) != 0o700:
            self.__exit__(None, None, None)
            raise SyncError("unsafe_credential_directory")
        return self

    def __exit__(self, *args):
        if self.fd is not None:
            os.close(self.fd)
            self.fd = None

    def check_file(self, fd):
        info = os.fstat(fd)
        if (not stat.S_ISREG(info.st_mode) or info.st_uid != os.geteuid()
                or stat.S_IMODE(info.st_mode) != 0o600 or info.st_nlink != 1):
            raise SyncError("unsafe_credential_file")
        return info

    def read(self, name):
        try:
            fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK, dir_fd=self.fd)
        except FileNotFoundError:
            return None
        except OSError:
            raise SyncError("credential_file_unavailable") from None
        try:
            if self.check_file(fd).st_size > MAX_BUNDLE_BYTES:
                raise SyncError("credential_file_size")
            result = os.read(fd, MAX_BUNDLE_BYTES + 1)
            if len(result) > MAX_BUNDLE_BYTES:
                raise SyncError("credential_file_size")
            return result
        finally:
            os.close(fd)

    @contextmanager
    def locked(self):
        try:
            fd = os.open(".token-sync.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK,
                         0o600, dir_fd=self.fd)
        except OSError:
            raise SyncError("lock_unavailable") from None
        try:
            self.check_file(fd)
            try:
                fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                raise SyncError("sync_busy") from None
            yield
        finally:
            os.close(fd)

    def stage(self, content):
        name = ".token-sync-" + secrets.token_hex(12)
        fd = os.open(name, os.O_CREAT | os.O_EXCL | os.O_WRONLY | os.O_NOFOLLOW | os.O_CLOEXEC, 0o600, dir_fd=self.fd)
        try:
            os.fchmod(fd, 0o600)
            with os.fdopen(fd, "wb", closefd=False) as writer:
                writer.write(content)
                writer.flush()
                os.fsync(fd)
        except BaseException:
            self.remove(name)
            raise
        finally:
            os.close(fd)
        return name

    def remove(self, name):
        if name is not None:
            try:
                os.unlink(name, dir_fd=self.fd)
            except FileNotFoundError:
                pass

    def replace(self, staged, target):
        os.replace(staged, target, src_dir_fd=self.fd, dst_dir_fd=self.fd)

    def install(self, bundle, now):
        old_token = self.read(TOKEN_FILE)
        old_metadata = self.read(METADATA_FILE)
        old = inspect_pair(old_token, old_metadata, now)
        if old["state"] == "valid":
            previous = strict_json(old_metadata)
            if all(previous[key] == bundle[key] for key in ("api_url", "namespace", "service_account")):
                if timestamp(previous["expires_at"]) > timestamp(bundle["expires_at"]):
                    raise SyncError("token_expiration_regressed")
        token = (bundle["token"] + "\n").encode("ascii")
        metadata = {key: value for key, value in bundle.items() if key != "token"}
        metadata.update(installed_at=iso(now), token_sha256=hashlib.sha256(token).hexdigest())
        staged_token = staged_metadata = staged_rollback = None
        try:
            staged_token = self.stage(token)
            staged_metadata = self.stage((json.dumps(metadata, sort_keys=True) + "\n").encode("ascii"))
            if old_metadata is not None:
                staged_rollback = self.stage(old_metadata)
            self.replace(staged_metadata, METADATA_FILE)
            staged_metadata = None
            try:
                # Token replacement is the commit point. A bind mount of this
                # whole directory lets the provider see the new inode at once.
                self.replace(staged_token, TOKEN_FILE)
                staged_token = None
            except OSError:
                try:
                    if staged_rollback is None:
                        self.remove(METADATA_FILE)
                    else:
                        self.replace(staged_rollback, METADATA_FILE)
                        staged_rollback = None
                    os.fsync(self.fd)
                except OSError:
                    raise SyncError("metadata_recovery_failed") from None
                raise SyncError("token_replace_failed") from None
            try:
                os.fsync(self.fd)
            except OSError:
                raise SyncError("commit_durability_unknown") from None
        finally:
            for name in (staged_token, staged_metadata, staged_rollback):
                self.remove(name)
        return {"state": "valid", "expires_at": metadata["expires_at"], "installed_at": metadata["installed_at"],
                "remaining_seconds": int((timestamp(metadata["expires_at"]) - now).total_seconds())}


def inspect_pair(token, raw_metadata, now):
    if token is None and raw_metadata is None:
        return {"state": "missing"}
    if token is None or raw_metadata is None:
        return {"state": "incomplete"}
    try:
        metadata = strict_json(raw_metadata)
        if (set(metadata) != METADATA_KEYS or type(metadata.get("schema")) is not int or metadata["schema"] != 1
                or metadata["token_sha256"] != hashlib.sha256(token).hexdigest()):
            return {"state": "metadata_mismatch"}
        api_url(metadata["api_url"])
        identity_label(metadata["namespace"])
        identity_label(metadata["service_account"])
        expires = timestamp(metadata["expires_at"])
        installed = timestamp(metadata["installed_at"])
        token_claims(token.decode("ascii").strip(), metadata["namespace"], metadata["service_account"], expires)
    except (SyncError, ValueError, UnicodeError, TypeError):
        return {"state": "invalid"}
    remaining = int((expires - now).total_seconds())
    return {"state": "valid" if remaining > 0 else "expired", "expires_at": iso(expires),
            "installed_at": iso(installed), "remaining_seconds": remaining}


def current_status(directory, now=None):
    try:
        with CredentialStore(directory) as store:
            return inspect_pair(store.read(TOKEN_FILE), store.read(METADATA_FILE), now or utc_now())
    except (SyncError, OSError, ValueError, TypeError):
        return {"state": "unavailable"}


def synchronize(args, now=None):
    api_url(args.expected_api_url)
    identity_label(args.namespace)
    identity_label(args.service_account)
    if not 60 <= args.minimum_remaining_seconds <= 3600:
        raise SyncError("invalid_minimum_remaining")
    with CredentialStore(args.output_dir) as store, store.locked():
        raw = fetch_bundle(args)
        # Compute freshness after the network request, not before a slow SSH.
        checked_at = now or utc_now()
        bundle = validate_bundle(raw, args.expected_api_url, args.namespace, args.service_account,
                                 checked_at, args.minimum_remaining_seconds)
        return store.install(bundle, checked_at)


def parser():
    cli = Parser(description=__doc__)
    cli.add_argument("--ssh-host")
    cli.add_argument("--ssh-user", default="root")
    cli.add_argument("--ssh-port", type=int, default=22)
    cli.add_argument("--ssh-key-file", type=absolute_path,
                     default=Path("/opt/nakama-agones/credential-sync/token-pull.key"))
    cli.add_argument("--known-hosts", type=absolute_path,
                     default=Path("/opt/nakama-agones/credential-sync/known_hosts"))
    cli.add_argument("--expected-api-url")
    cli.add_argument("--namespace", default="agones-control")
    cli.add_argument("--service-account", default="nakama-agones")
    cli.add_argument("--minimum-remaining-seconds", type=int, default=900)
    cli.add_argument("--output-dir", type=absolute_path, default=Path("/opt/nakama-agones/credentials"))
    cli.add_argument("--status", action="store_true", help="Read local expiry metadata; make no network request.")
    return cli


def main(argv=None):
    args = None
    try:
        args = parser().parse_args(argv)
        if args.status:
            state = current_status(args.output_dir)
            print(json.dumps({"ok": state["state"] == "valid", "credential": state}, sort_keys=True))
            return 0 if state["state"] == "valid" else 1
        if not args.ssh_host or not args.expected_api_url:
            raise SyncError("missing_sync_configuration")
        state = synchronize(args)
        print(json.dumps({"ok": True, "code": "token_synced", "credential": state}, sort_keys=True))
        return 0
    except (SyncError, OSError, ValueError, TypeError, OverflowError) as error:
        code = str(error) if isinstance(error, SyncError) else "local_io_or_configuration_error"
        result = {"ok": False, "code": code}
        if args is not None:
            result["credential"] = current_status(args.output_dir)
        print(json.dumps(result, sort_keys=True), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
