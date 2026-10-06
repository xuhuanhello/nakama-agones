#!/usr/bin/env python3
"""Prepare and validate the local single-host Nakama/GameFleet Compose stack."""
from __future__ import annotations

import argparse
import ipaddress
import os
from pathlib import Path
import re
import secrets
import shutil
import stat
import subprocess
import sys


REPO = Path(__file__).resolve().parent.parent
DEFAULT_DIR = Path("/opt/nakama")
SERVICE_TEMPLATE = REPO / "deploy" / "gamefleet-service.env.example"
ENV_TEMPLATE = REPO / "deploy" / "stack.env.example"
IMAGE_DIGEST = re.compile(r"[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}\Z")
LOCAL_IMAGE_ID = re.compile(r"sha256:[a-f0-9]{64}\Z")
SAFE_VALUE = re.compile(r"[^\s\x00-\x1f\x7f$`\\'\"]+\Z")
SAFE_MODULE = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]*\.so\Z")
SERVICE_KEYS = (
    "GAMEFLEET_SERVICE_URL",
    "GAMEFLEET_SERVICE_KEY_FILE",
    "GAMEFLEET_SERVICE_ID",
    "GAMEFLEET_SERVICE_APPLICATION_ID",
    "GAMEFLEET_SERVICE_IDENTITY_ISSUER",
    "GAMEFLEET_SERVICE_REGION",
    "GAMEFLEET_SERVICE_COMPATIBILITY",
)
ARCHIVE_KEYS = (
    "GAMEFLEET_ARCHIVE_URL",
    "GAMEFLEET_ARCHIVE_KEY_FILE",
    "GAMEFLEET_ARCHIVE_APPLICATION_ID",
    "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER",
    "GAMEFLEET_ARCHIVE_REGION",
    "GAMEFLEET_ARCHIVE_COMPATIBILITY",
    "GAMEFLEET_ARCHIVE_SERVICE_ID",
)
CONFIG_FILES = (
    (REPO / "deploy" / "compose.yaml", Path("compose.yaml")),
    (REPO / "deploy" / "Caddyfile", Path("deploy/Caddyfile")),
    (REPO / "deploy" / "nakama-entrypoint.sh", Path("deploy/nakama-entrypoint.sh")),
    (REPO / "deploy" / "tunnel" / "Dockerfile", Path("deploy/tunnel/Dockerfile")),
    (REPO / "deploy" / "tunnel" / "entrypoint.sh", Path("deploy/tunnel/entrypoint.sh")),
)
GENERATED_SECRETS = {
    "postgres-password": b"",
    "nakama-socket-server-key": b"",
    "nakama-session-encryption-key": b"",
    "nakama-refresh-encryption-key": b"",
    "nakama-runtime-http-key": b"",
    "nakama-console-password": b"",
    "nakama-console-signing-key": b"",
}
EXTERNAL_FILES = {
    "gamefleet-service-key": b"",
    "gamefleet-archive-key.disabled": b"",
    "platform-ssh-key": b"",
    "platform-known-hosts": b"",
    "application-credentials.json": b"{}\n",
}


class ConfigError(ValueError):
    """A safe diagnostic that does not echo configuration values."""


def _read_env(path: Path, *, allowed: set[str] | None = None) -> dict[str, str]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        raise ConfigError(f"configuration file is unavailable or not UTF-8: {path.name}") from None
    result: dict[str, str] = {}
    for line in lines:
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if "=" not in stripped:
            raise ConfigError(f"malformed setting in {path.name}")
        name, value = stripped.split("=", 1)
        name, value = name.strip(), value.strip()
        if not re.fullmatch(r"[A-Z][A-Z0-9_]*", name):
            raise ConfigError(f"invalid setting name in {path.name}")
        if allowed is not None and name not in allowed:
            raise ConfigError(f"unsupported setting name in {path.name}: {name}")
        if name in result:
            raise ConfigError(f"duplicate setting in {path.name}: {name}")
        if value and not SAFE_VALUE.fullmatch(value):
            raise ConfigError(f"unsupported quoting or expansion in {path.name}: {name}")
        result[name] = value
    return result


def _write_private(path: Path, data: bytes, *, mode: int = 0o400) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, mode)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
    except Exception:
        path.unlink(missing_ok=True)
        raise
    os.chmod(path, mode)


def _copy_if_absent(source: Path, target: Path, mode: int = 0o600) -> None:
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if target.exists():
        raise ConfigError(f"output already exists: {target}; init never overwrites an existing file")
    shutil.copyfile(source, target)
    os.chmod(target, mode)


def command_init(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        raise ConfigError("run init with sudo so private files and SSH credentials are root-owned for Compose")
    root: Path = args.directory.expanduser().resolve()
    root.mkdir(mode=0o700, parents=True, exist_ok=True)
    private = root / "private"
    private.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(private, 0o700)

    try:
        _copy_if_absent(ENV_TEMPLATE, root / ".env")
        for source, relative in CONFIG_FILES:
            _copy_if_absent(source, root / relative)
        _copy_if_absent(SERVICE_TEMPLATE, private / "gamefleet-service.env")
        for name in GENERATED_SECRETS:
            _write_private(private / name, secrets.token_urlsafe(32).encode("ascii") + b"\n")
        for name, data in EXTERNAL_FILES.items():
            _write_private(private / name, data)
    except FileExistsError as exc:
        raise ConfigError(f"output already exists: {Path(exc.filename or 'file')}; init never overwrites files") from None
    except OSError:
        raise ConfigError("project files could not be created") from None

    print(f"Created a local stack template at {root}")
    print("Generated database and Nakama keys are in private/ with mode 0400; values were not printed.")
    print("Fill .env, the seven GameFleet service fields, and the owner-provided service/SSH files before validation.")
    print("Template generation did not pull images, contact GameFleet, start containers, or deploy anything.")
    return 0


def _looks_placeholder(value: str) -> bool:
    lowered = value.lower()
    return any(token in lowered for token in ("example.com", "replace_me", "change_me", "your_", "<", ">"))


def _private_file(root: Path, env: dict[str, str], name: str, setting: str, *, allow_empty: bool = False,
                  required_owner_uid: int | None = None) -> Path:
    value = env.get(setting, "")
    if not value:
        raise ConfigError(f"{setting} is required")
    candidate = Path(value)
    if not candidate.is_absolute():
        candidate = root / candidate
    try:
        info = candidate.lstat()
    except OSError:
        raise ConfigError(f"{name} file is missing") from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode):
        raise ConfigError(f"{name} must be a non-symlink regular file")
    if stat.S_IMODE(info.st_mode) not in (0o400, 0o600):
        raise ConfigError(f"{name} file must have mode 0400 or 0600")
    if required_owner_uid is not None and info.st_uid != required_owner_uid:
        raise ConfigError(f"{name} file must be owned by UID {required_owner_uid}")
    if not allow_empty and info.st_size == 0:
        raise ConfigError(f"{name} file must not be empty")
    try:
        return candidate.resolve(strict=True)
    except OSError:
        raise ConfigError(f"{name} file is unavailable") from None


def _validate_private_endpoint(url: str, *, port: int) -> None:
    match = re.fullmatch(r"http://127\.0\.0\.1:([0-9]{1,5})", url)
    if not match or int(match.group(1)) != port:
        raise ConfigError(f"GAMEFLEET_SERVICE_URL must be the loopback tunnel origin on port {port}")


def _validate_application_env(root: Path, path_value: str, caddy_ipv4: str) -> None:
    if not path_value:
        raise ConfigError("APPLICATION_ENV_FILE is required; use an empty/nonsecret optional file if the image has no extra env")
    path = Path(path_value)
    if not path.is_absolute():
        path = root / path
    if not path.exists():
        return  # Compose marks this env_file optional.
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        raise ConfigError("application env file is unavailable or not UTF-8") from None
    trusted_values: list[str] = []
    for line in lines:
        stripped = line.strip()
        if not stripped or stripped.startswith("#") or "=" not in stripped:
            continue
        name, value = stripped.split("=", 1)
        if name.strip() == "DM_ACCOUNT_TRUSTED_PROXY_CIDRS":
            trusted_values.append(value.strip().strip("\"'"))
    if trusted_values and trusted_values != [f"{caddy_ipv4}/32"]:
        raise ConfigError("DM_ACCOUNT_TRUSTED_PROXY_CIDRS must contain only the exact Caddy gateway IPv4 /32")


def validate_project(root: Path, *, check_image: bool = True, compose_config: bool = True) -> dict[str, str]:
    try:
        root = root.expanduser().resolve(strict=True)
    except OSError:
        raise ConfigError("project directory does not exist") from None
    env = _read_env(root / ".env")
    service_env_path = Path(env.get("GAMEFLEET_SERVICE_ENV_FILE", "./private/gamefleet-service.env"))
    if not service_env_path.is_absolute():
        service_env_path = root / service_env_path
    service_env = _read_env(service_env_path, allowed={
        "NAKAMA_FLEET_BACKEND", *SERVICE_KEYS, *ARCHIVE_KEYS,
    })
    if service_env.get("NAKAMA_FLEET_BACKEND") != "gamefleet-service":
        raise ConfigError("NAKAMA_FLEET_BACKEND must be gamefleet-service")
    missing = [key for key in SERVICE_KEYS if not service_env.get(key, "").strip()]
    if missing:
        raise ConfigError("all seven GAMEFLEET_SERVICE settings are required; missing: " + ", ".join(missing))
    _validate_private_endpoint(service_env["GAMEFLEET_SERVICE_URL"], port=17682)
    if service_env["GAMEFLEET_SERVICE_KEY_FILE"] != "/run/secrets/gamefleet-service-key":
        raise ConfigError("GAMEFLEET_SERVICE_KEY_FILE must use the read-only Compose secret target")
    for key in SERVICE_KEYS[2:]:
        value = service_env[key]
        if not SAFE_VALUE.fullmatch(value) or _looks_placeholder(value):
            raise ConfigError(f"{key} must be replaced with its exact owner-configured value")

    archive_fields = [service_env.get(key, "") for key in ARCHIVE_KEYS if key != "GAMEFLEET_ARCHIVE_KEY_FILE"]
    archive_enabled = any(archive_fields)
    if archive_enabled and not all(archive_fields):
        raise ConfigError("optional GAMEFLEET_ARCHIVE fields must all be set or all be empty")
    if archive_enabled:
        if env.get("GAMEFLEET_ARCHIVE_KEY_TARGET") != "/run/secrets/gamefleet-archive-key":
            raise ConfigError("set GAMEFLEET_ARCHIVE_KEY_TARGET to /run/secrets/gamefleet-archive-key")
        if service_env.get("GAMEFLEET_ARCHIVE_KEY_FILE") not in ("", "/run/secrets/gamefleet-archive-key"):
            raise ConfigError("archive key path must use the read-only Compose secret target")

    required_env = ("NAKAMA_DOMAIN", "NAKAMA_RUNTIME_IMAGE", "POSTGRES_IMAGE", "CADDY_IMAGE", "ALPINE_IMAGE",
                    "GAMEFLEET_SSH_TARGET", "GAMEFLEET_SSH_PORT", "FRONTEND_SUBNET", "DATABASE_SUBNET",
                    "CADDY_IPV4", "NAKAMA_IPV4")
    missing_env = [key for key in required_env if not env.get(key, "").strip()]
    if missing_env:
        raise ConfigError("missing .env setting(s): " + ", ".join(missing_env))
    for name in ("POSTGRES_IMAGE", "CADDY_IMAGE", "ALPINE_IMAGE"):
        if not IMAGE_DIGEST.fullmatch(env[name]):
            raise ConfigError(f"{name} must use an immutable OCI sha256 digest")
    runtime_image = env["NAKAMA_RUNTIME_IMAGE"]
    if not IMAGE_DIGEST.fullmatch(runtime_image) and not LOCAL_IMAGE_ID.fullmatch(runtime_image):
        raise ConfigError("NAKAMA_RUNTIME_IMAGE must be a registry digest or exact local sha256 image ID")
    domain = env["NAKAMA_DOMAIN"]
    if _looks_placeholder(domain) or not re.fullmatch(r"(?=.{1,253}\Z)(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)(?:\.(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?))+", domain):
        raise ConfigError("NAKAMA_DOMAIN must be a public DNS name without a scheme or path")
    ssh_target = env["GAMEFLEET_SSH_TARGET"]
    if _looks_placeholder(ssh_target) or not re.fullmatch(r"[A-Za-z0-9_.-]+@[A-Za-z0-9.-]+", ssh_target):
        raise ConfigError("GAMEFLEET_SSH_TARGET must be a user@hostname value with key authentication")
    ssh_port = env["GAMEFLEET_SSH_PORT"]
    if not ssh_port.isdigit() or not 1 <= int(ssh_port) <= 65535:
        raise ConfigError("GAMEFLEET_SSH_PORT must be between 1 and 65535")
    for subnet_key in ("FRONTEND_SUBNET", "DATABASE_SUBNET"):
        try:
            ipaddress.ip_network(env[subnet_key], strict=True)
        except ValueError:
            raise ConfigError(f"{subnet_key} must be a valid IPv4 subnet") from None
        if ":" in env[subnet_key]:
            raise ConfigError(f"{subnet_key} must be IPv4 for the configured static addresses")
    frontend = ipaddress.ip_network(env["FRONTEND_SUBNET"], strict=True)
    database = ipaddress.ip_network(env["DATABASE_SUBNET"], strict=True)
    if frontend.overlaps(database):
        raise ConfigError("FRONTEND_SUBNET and DATABASE_SUBNET must not overlap")
    try:
        caddy_ip = ipaddress.ip_address(env["CADDY_IPV4"])
        nakama_ip = ipaddress.ip_address(env["NAKAMA_IPV4"])
    except ValueError:
        raise ConfigError("CADDY_IPV4 and NAKAMA_IPV4 must be IPv4 addresses") from None
    if caddy_ip not in frontend or nakama_ip not in frontend or caddy_ip == nakama_ip:
        raise ConfigError("Caddy and Nakama addresses must be distinct hosts in FRONTEND_SUBNET")
    target = env.get("GAMEFLEET_ARCHIVE_KEY_TARGET", "")
    if target and target != "/run/secrets/gamefleet-archive-key":
        raise ConfigError("GAMEFLEET_ARCHIVE_KEY_TARGET must be /run/secrets/gamefleet-archive-key")

    # Validate host file metadata only. Private contents are never opened.
    for name, setting in (
        ("database password", "POSTGRES_PASSWORD_SOURCE"),
        ("Nakama socket key", "NAKAMA_SOCKET_SERVER_KEY_SOURCE"),
        ("Nakama session key", "NAKAMA_SESSION_KEY_SOURCE"),
        ("Nakama refresh key", "NAKAMA_REFRESH_KEY_SOURCE"),
        ("Nakama runtime key", "NAKAMA_RUNTIME_HTTP_KEY_SOURCE"),
        ("Nakama console password", "NAKAMA_CONSOLE_PASSWORD_SOURCE"),
        ("Nakama console signing key", "NAKAMA_CONSOLE_SIGNING_KEY_SOURCE"),
        ("GameFleet service key", "GAMEFLEET_SERVICE_KEY_SOURCE"),
        ("platform SSH private key", "PLATFORM_SSH_KEY_SOURCE"),
        ("application credential", "APPLICATION_SECRET_FILE"),
    ):
        _private_file(root, env, name, setting, required_owner_uid=0 if setting == "PLATFORM_SSH_KEY_SOURCE" else None)
    archive_source = env.get("GAMEFLEET_ARCHIVE_KEY_SOURCE", "")
    if archive_enabled:
        if not archive_source:
            raise ConfigError("GAMEFLEET_ARCHIVE_KEY_SOURCE is required when the archive reader is enabled")
        _private_file(root, env, "GameFleet archive key", "GAMEFLEET_ARCHIVE_KEY_SOURCE")
    known_hosts = _private_file(root, env, "platform known_hosts", "PLATFORM_KNOWN_HOSTS_SOURCE", required_owner_uid=0)
    if known_hosts.stat().st_size == 0:
        raise ConfigError("platform known_hosts must contain an independently verified host key")
    _validate_application_env(root, env.get("APPLICATION_ENV_FILE", "./private/account.env"), str(caddy_ip))

    modules_value = env.get("APPLICATION_REQUIRED_MODULES", "").strip()
    modules = [part.strip() for part in modules_value.split(",") if part.strip()]
    if len(set(modules)) != len(modules) or any(not SAFE_MODULE.fullmatch(module) for module in modules):
        raise ConfigError("APPLICATION_REQUIRED_MODULES must be a comma-separated list of module filenames")
    env["_APPLICATION_REQUIRED_MODULES"] = ",".join(modules)

    if check_image:
        _check_runtime_image(runtime_image, modules)
    if compose_config:
        try:
            subprocess.run(
                ["docker", "compose", "--env-file", str(root / ".env"), "--project-directory", str(root),
                 "-f", str(root / "compose.yaml"), "config", "--quiet"],
                cwd=root, check=True, capture_output=True, text=True,
            )
        except (OSError, subprocess.SubprocessError):
            raise ConfigError("docker compose config --quiet failed; review paths and nonsecret settings locally") from None
    return env


def _check_runtime_image(image: str, modules: list[str]) -> None:
    try:
        inspected = subprocess.run(["docker", "image", "inspect", "--format", "{{.Id}}|{{json .RepoDigests}}", image],
                                   check=True, capture_output=True, text=True)
    except (OSError, subprocess.SubprocessError):
        raise ConfigError("runtime image is not present locally; build by image ID or pull the exact registry digest first") from None
    line = inspected.stdout.strip()
    image_id, separator, repo_digests = line.partition("|")
    if not separator or not LOCAL_IMAGE_ID.fullmatch(image_id):
        raise ConfigError("Docker returned an unexpected runtime image identity")
    if LOCAL_IMAGE_ID.fullmatch(image):
        if image_id != image:
            raise ConfigError("local runtime image ID does not match the requested image ID")
    elif image not in repo_digests:
        raise ConfigError("runtime image does not have the requested immutable registry digest")
    required = ["agones.so", *modules]
    checker = 'for module do test -s "/nakama/data/modules/$module" || exit 1; done'
    command = ["docker", "run", "--pull=never", "--rm", "--network", "none", "--entrypoint", "/bin/sh",
               image, "-c", checker, "module-check", *required]
    try:
        subprocess.run(command, check=True, capture_output=True, text=True)
    except (OSError, subprocess.SubprocessError):
        raise ConfigError("runtime image is missing agones.so or a configured required module") from None


def command_validate(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        raise ConfigError("run validate with sudo so it can inspect root-owned private files and the Docker image")
    env = validate_project(args.directory, check_image=not args.skip_image_check,
                           compose_config=not args.skip_compose_config)
    print("Local stack configuration is valid: GameFleet service mode, seven service fields, and private-file permissions checked.")
    print("Compose model passed config --quiet; no resolved environment values were printed.")
    if args.skip_image_check:
        print("Runtime image presence and module files were not checked (--skip-image-check).")
    else:
        modules = ["agones.so", *([m for m in env.get("_APPLICATION_REQUIRED_MODULES", "").split(",") if m])]
        print("Runtime image identity and required module files were checked locally: " + ", ".join(modules) + ".")
    print("No remote host, GameFleet service, or running deployment was queried.")
    return 0


def command_plan(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        raise ConfigError("run plan with sudo so it can inspect root-owned private files and the Docker image")
    env = validate_project(args.directory, check_image=not args.skip_image_check,
                           compose_config=not args.skip_compose_config)
    image = env["NAKAMA_RUNTIME_IMAGE"]
    mode = "local Docker image ID" if LOCAL_IMAGE_ID.fullmatch(image) else "registry image digest already pulled locally"
    print("Local deployment plan; this command does not pull images, start services, or contact a remote host.")
    print(f"1. Use the supplied immutable application runtime identity ({mode}).")
    print("2. Build the restricted SSH sidecar image from the pinned Alpine digest.")
    print("3. Start PostgreSQL, Nakama, the loopback-only GameFleet SSH forward, and Caddy HTTPS gateway.")
    print("4. Run Nakama migrations against the named persistent PostgreSQL volume.")
    print("5. Review Compose logs and test the public HTTPS endpoint and GameFleet service scopes separately.")
    print("The plan does not prove remote reachability, grants, account login, match allocation, or production health.")
    return 0


def command_status(args: argparse.Namespace) -> int:
    if os.geteuid() != 0:
        raise ConfigError("run status with sudo to inspect the root-owned project directory")
    root = args.directory.expanduser().resolve()
    local_files = [root / ".env", root / "compose.yaml", root / "private" / "gamefleet-service.env"]
    present = sum(path.is_file() for path in local_files)
    print(f"Local project files: {present}/{len(local_files)} present.")
    print("Deployment status: not checked; this command makes no Docker-daemon or network request.")
    return 0


def make_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    init = commands.add_parser("init", help="create an empty-host stack template and private local keys")
    init.add_argument("--directory", type=Path, default=DEFAULT_DIR)
    init.set_defaults(run=command_init)

    for name, help_text, runner in (
        ("validate", "validate local files and optionally inspect the image/Compose model", command_validate),
        ("plan", "show a local deployment plan without applying it", command_plan),
    ):
        command = commands.add_parser(name, help=help_text)
        command.add_argument("--directory", type=Path, default=DEFAULT_DIR)
        command.add_argument("--skip-image-check", action="store_true", help="skip the local Docker image and module check")
        command.add_argument("--skip-compose-config", action="store_true", help="skip docker compose config --quiet")
        command.set_defaults(run=runner)

    status = commands.add_parser("status", help="count local input files without probing deployment state")
    status.add_argument("--directory", type=Path, default=DEFAULT_DIR)
    status.set_defaults(run=command_status)
    return parser


def main(argv: list[str] | None = None) -> int:
    args = make_parser().parse_args(argv)
    try:
        return args.run(args)
    except ConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("error: local filesystem operation failed", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
