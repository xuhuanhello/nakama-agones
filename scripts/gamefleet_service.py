#!/usr/bin/env python3
"""Prepare a local Nakama gamefleet-service runtime without deploying it."""
from __future__ import annotations

import argparse
from dataclasses import dataclass
import ipaddress
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import subprocess
import sys
import tempfile
from urllib.parse import urlsplit


REPO = Path(__file__).resolve().parent.parent
TEMPLATE = REPO / "deploy" / "gamefleet-service.env.example"
DEFAULT_DIR = Path(".local/gamefleet-service")
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
KNOWN_KEYS = {"NAKAMA_FLEET_BACKEND", *SERVICE_KEYS, *ARCHIVE_KEYS}
IMAGE_DIGEST = re.compile(r"[a-z0-9][a-z0-9._:/-]*@sha256:[a-f0-9]{64}\Z")
IMAGE_TAG = re.compile(r"[A-Za-z0-9][A-Za-z0-9._:/-]*:[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}\Z")
SAFE_VALUE = re.compile(r"[^\s\x00-\x1f\x7f$`\\'\"]+\Z")


class ConfigError(ValueError):
    """A safe diagnostic that never includes a setting's value."""


@dataclass(frozen=True)
class ValidatedConfig:
    values: dict[str, str]
    key_source: Path
    archive_key_source: Path | None


def parse_env_file(path: Path) -> dict[str, str]:
    """Read simple KEY=VALUE data. Never source or evaluate it as shell."""
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except (OSError, UnicodeError):
        raise ConfigError("environment file is unavailable or not UTF-8") from None

    values: dict[str, str] = {}
    for line in lines:
        stripped = line.strip()
        if not stripped or stripped.startswith("#"):
            continue
        if "=" not in stripped:
            raise ConfigError("environment file contains a malformed setting")
        name, value = stripped.split("=", 1)
        name, value = name.strip(), value.strip()
        if not re.fullmatch(r"[A-Z][A-Z0-9_]*", name):
            raise ConfigError("environment file contains an invalid setting name")
        if name not in KNOWN_KEYS:
            raise ConfigError(f"unsupported setting name: {name}")
        if name in values:
            raise ConfigError(f"duplicate setting: {name}")
        if value and not SAFE_VALUE.fullmatch(value):
            raise ConfigError(f"setting contains unsupported quoting or expansion: {name}")
        values[name] = value
    return values


def _validate_non_secret_values(values: dict[str, str]) -> None:
    if values.get("NAKAMA_FLEET_BACKEND") != "gamefleet-service":
        raise ConfigError("NAKAMA_FLEET_BACKEND must be gamefleet-service")

    missing = [name for name in SERVICE_KEYS if not values.get(name, "").strip()]
    if missing:
        raise ConfigError("all seven GAMEFLEET_SERVICE settings are required; missing: " + ", ".join(missing))

    _validate_loopback_origin(values["GAMEFLEET_SERVICE_URL"], "GAMEFLEET_SERVICE_URL")

    key_target = _container_key_path(values["GAMEFLEET_SERVICE_KEY_FILE"], "GAMEFLEET_SERVICE_KEY_FILE")
    values["GAMEFLEET_SERVICE_KEY_FILE"] = str(key_target)
    for name in SERVICE_KEYS[2:]:
        value = values[name]
        if not value or not SAFE_VALUE.fullmatch(value) or _looks_like_placeholder(value):
            raise ConfigError(f"{name} must be replaced with the exact configured value")

    archive_values = [values.get(name, "") for name in ARCHIVE_KEYS]
    configured = [bool(value) for value in archive_values]
    if any(configured) and not all(configured):
        raise ConfigError("optional GAMEFLEET_ARCHIVE settings must be all configured or all empty")
    if all(configured):
        archive_target = _container_key_path(values["GAMEFLEET_ARCHIVE_KEY_FILE"], "GAMEFLEET_ARCHIVE_KEY_FILE")
        values["GAMEFLEET_ARCHIVE_KEY_FILE"] = str(archive_target)
        _validate_loopback_origin(values["GAMEFLEET_ARCHIVE_URL"], "GAMEFLEET_ARCHIVE_URL")
        for name in ("GAMEFLEET_ARCHIVE_APPLICATION_ID", "GAMEFLEET_ARCHIVE_IDENTITY_ISSUER",
                     "GAMEFLEET_ARCHIVE_REGION", "GAMEFLEET_ARCHIVE_COMPATIBILITY",
                     "GAMEFLEET_ARCHIVE_SERVICE_ID"):
            if not SAFE_VALUE.fullmatch(values[name]) or _looks_like_placeholder(values[name]):
                raise ConfigError(f"{name} must be replaced with the exact configured value")


def _looks_like_placeholder(value: str) -> bool:
    normalized = value.casefold()
    return any(marker in normalized for marker in ("replace_me", "change_me", "your_", "<", ">", "todo"))


def _validate_loopback_origin(value: str, name: str) -> None:
    try:
        parsed = urlsplit(value)
        host = parsed.hostname
        port = parsed.port
        loopback = ipaddress.ip_address(host).is_loopback if host else False
    except (ValueError, TypeError):
        raise ConfigError(f"{name} must be an explicit loopback HTTP origin") from None
    if (parsed.scheme != "http" or not loopback or port is None or not 1 <= port <= 65535 or
            parsed.username is not None or parsed.password is not None or
            parsed.path not in ("", "/") or parsed.query or parsed.fragment or "?" in value or "#" in value):
        raise ConfigError(f"{name} must be an explicit loopback HTTP origin")


def _container_key_path(value: str, setting: str) -> PurePosixPath:
    target = PurePosixPath(value)
    if not target.is_absolute() or target == PurePosixPath("/") or ".." in target.parts:
        raise ConfigError(f"{setting} must be an absolute container file path without parent traversal")
    return target


def _private_key_source(path: Path, name: str) -> Path:
    """Check only path metadata. Deliberately never open/read the key file."""
    try:
        info = path.lstat()
    except OSError:
        raise ConfigError(f"{name} must point to a local private key file") from None
    if stat.S_ISLNK(info.st_mode) or not stat.S_ISREG(info.st_mode) or stat.S_IMODE(info.st_mode) not in (0o400, 0o600):
        raise ConfigError(f"{name} must be a non-symlink regular file with mode 0400 or 0600")
    try:
        return path.resolve(strict=True)
    except OSError:
        raise ConfigError(f"{name} must point to a local private key file") from None


def validate_config(env_path: Path, key_source: Path, archive_key_source: Path | None = None) -> ValidatedConfig:
    values = parse_env_file(env_path)
    _validate_non_secret_values(values)
    key = _private_key_source(key_source, "--key-source")
    archive_enabled = bool(values.get("GAMEFLEET_ARCHIVE_URL"))
    if archive_enabled and archive_key_source is None:
        raise ConfigError("configured archive requires --archive-key-source")
    if not archive_enabled and archive_key_source is not None:
        raise ConfigError("--archive-key-source was supplied while archive settings are disabled")
    archive_key = _private_key_source(archive_key_source, "--archive-key-source") if archive_key_source else None
    if archive_key and archive_key == key:
        raise ConfigError("service and archive key files must be separate files")
    return ValidatedConfig(values=values, key_source=key, archive_key_source=archive_key)


def _json_scalar(value: str) -> str:
    # JSON string scalars are valid YAML and make paths/identifiers unambiguous.
    return json.dumps(value, ensure_ascii=False)


def compose_override(config: ValidatedConfig, runtime_image: str) -> str:
    if not IMAGE_DIGEST.fullmatch(runtime_image):
        raise ConfigError("runtime image must be pinned by an OCI sha256 digest")
    lines = [
        "# Generated locally by scripts/gamefleet_service.py render.",
        "# Merge into the existing Fixed/application Compose project; this is not a deployment.",
        "services:",
        "  nakama:",
        f"    image: {_json_scalar(runtime_image)}",
        "    environment:",
        f"      NAKAMA_FLEET_BACKEND: {_json_scalar('gamefleet-service')}",
    ]
    for name in SERVICE_KEYS:
        lines.append(f"      {name}: {_json_scalar(config.values[name])}")
    for name in ARCHIVE_KEYS:
        value = config.values.get(name, "")
        if value:
            lines.append(f"      {name}: {_json_scalar(value)}")
    lines.extend([
        "    volumes:",
        "      - type: bind",
        f"        source: {_json_scalar(str(config.key_source))}",
        f"        target: {_json_scalar(config.values['GAMEFLEET_SERVICE_KEY_FILE'])}",
        "        read_only: true",
    ])
    if config.archive_key_source:
        lines.extend([
            "      - type: bind",
            f"        source: {_json_scalar(str(config.archive_key_source))}",
            f"        target: {_json_scalar(config.values['GAMEFLEET_ARCHIVE_KEY_FILE'])}",
            "        read_only: true",
        ])
    return "\n".join(lines) + "\n"


def _write_file(path: Path, data: str, *, force: bool = False) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    if path.exists() and not force:
        raise ConfigError(f"output already exists: {path}; choose another path or use --force")
    fd, temp_name = tempfile.mkstemp(prefix="." + path.name + ".", dir=path.parent)
    temp = Path(temp_name)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temp, path)
        os.chmod(path, 0o600)
    finally:
        try:
            temp.unlink(missing_ok=True)
        except OSError:
            pass


def _source_revision() -> tuple[str, bool]:
    try:
        revision = subprocess.run(["git", "rev-parse", "HEAD"], cwd=REPO, check=True,
                                  capture_output=True, text=True).stdout.strip()
        dirty = bool(subprocess.run(["git", "status", "--porcelain"], cwd=REPO, check=True,
                                    capture_output=True, text=True).stdout.strip())
        return revision, dirty
    except (OSError, subprocess.SubprocessError):
        return "unknown", True


def _compatibility_inputs() -> dict[str, str]:
    result: dict[str, str] = {}
    try:
        for line in (REPO / "deploy" / "compatibility.env").read_text(encoding="utf-8").splitlines():
            stripped = line.strip()
            if not stripped or stripped.startswith("#") or "=" not in stripped:
                continue
            key, value = stripped.split("=", 1)
            result[key] = value
    except (OSError, UnicodeError):
        raise ConfigError("deploy/compatibility.env is unavailable") from None
    needed = ("NAKAMA_VERSION", "NAKAMA_IMAGE", "PLUGIN_BUILDER_IMAGE", "NAKAMA_COMMON_VERSION",
              "GO_VERSION", "TARGET_PLATFORM", "PLUGIN_VERSION")
    if any(not result.get(name) for name in needed):
        raise ConfigError("deploy/compatibility.env is missing a pinned build input")
    if not IMAGE_DIGEST.fullmatch(result["NAKAMA_IMAGE"]) or not IMAGE_DIGEST.fullmatch(result["PLUGIN_BUILDER_IMAGE"]):
        raise ConfigError("Nakama and plugin-builder base images must be pinned by sha256 digest")
    if result["TARGET_PLATFORM"] != "linux/amd64":
        raise ConfigError("service runtime plugin ABI is validated only for linux/amd64")
    return result


def _validate_application_image(value: str) -> None:
    if not IMAGE_DIGEST.fullmatch(value):
        raise ConfigError("application runtime image must be pinned by an OCI sha256 digest")


def _validate_output_tag(value: str) -> None:
    if not IMAGE_TAG.fullmatch(value) or "@sha256:" in value:
        raise ConfigError("build output must be a local image tag; publish it and render with the resulting digest")


def command_init(args: argparse.Namespace) -> int:
    try:
        template = TEMPLATE.read_text(encoding="utf-8")
        _write_file(args.output, template)
    except OSError:
        raise ConfigError("service environment template could not be written") from None
    print(f"Created secret-free configuration template: {args.output}")
    print("Template generation did not deploy or contact GameFleet.")
    return 0


def _validated(args: argparse.Namespace) -> ValidatedConfig:
    return validate_config(args.env, args.key_source, getattr(args, "archive_key_source", None))


def command_validate(args: argparse.Namespace) -> int:
    _validated(args)
    print("Configuration valid: gamefleet-service and all seven required settings are present.")
    print("Key-file permissions were checked from metadata only; file contents were not read.")
    print("The loopback URL is checked as a literal address; actual reachability is tested by Nakama startup preflight in its own network namespace.")
    return 0


def command_plan(args: argparse.Namespace) -> int:
    config = _validated(args)
    compatibility = _compatibility_inputs()
    _validate_application_image(args.application_image)
    print("Local plan only; no remote service is contacted or changed.")
    print(f"1. Build agones.so for {compatibility['TARGET_PLATFORM']} using Nakama {compatibility['NAKAMA_VERSION']} / Go {compatibility['GO_VERSION']} and the pinned plugin-builder digest.")
    print("2. Extend the supplied full application runtime image; existing Fixed modules such as account.so remain in that image.")
    print("3. Publish the result, record its immutable digest, then render a Compose override for the existing nakama service.")
    print("4. Bind the private key file read-only; do not put the credential value in env, arguments, or chat.")
    print("5. Confirm the loopback forward inside Nakama's network namespace; service/history scope preflights run during Nakama startup.")
    print("6. GameFleet service identity, search/match grants, History routes, and platform installation remain owner-managed.")
    print(f"Service fields validated: {len(SERVICE_KEYS)}; archive reader: {'enabled' if config.archive_key_source else 'disabled'}.")
    print(f"Application runtime input: {args.application_image}")
    return 0


def command_build(args: argparse.Namespace) -> int:
    config = _validated(args)
    del config  # Metadata checks only; the key path and contents never enter the image build.
    compatibility = _compatibility_inputs()
    _validate_application_image(args.application_image)
    _validate_output_tag(args.output_image)
    revision, dirty = _source_revision()
    output_dir = args.output_dir
    output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(output_dir, 0o700)
    metadata_path = output_dir / "buildx-metadata.json"
    build_inputs = {
        "kind": "local-gamefleet-service-runtime-build",
        "application_runtime_image": args.application_image,
        "output_tag": args.output_image,
        "nakama_image": compatibility["NAKAMA_IMAGE"],
        "plugin_builder_image": compatibility["PLUGIN_BUILDER_IMAGE"],
        "nakama_version": compatibility["NAKAMA_VERSION"],
        "nakama_common_version": compatibility["NAKAMA_COMMON_VERSION"],
        "go_version": compatibility["GO_VERSION"],
        "platform": compatibility["TARGET_PLATFORM"],
        "plugin_version": args.plugin_version or compatibility["PLUGIN_VERSION"],
        "source_revision": revision,
        "source_dirty": dirty,
        "local_build_completed": False,
        "push_or_deploy_performed": False,
    }
    plugin_version = build_inputs["plugin_version"]
    if not re.fullmatch(r"[A-Za-z0-9._-]+", plugin_version):
        raise ConfigError("plugin version contains unsupported characters")
    command = [
        "docker", "buildx", "build", "--load",
        "--platform", compatibility["TARGET_PLATFORM"],
        "--file", "deploy/Dockerfile",
        "--target", "service-runtime",
        "--build-arg", f"NAKAMA_IMAGE={compatibility['NAKAMA_IMAGE']}",
        "--build-arg", f"PLUGIN_BUILDER_IMAGE={compatibility['PLUGIN_BUILDER_IMAGE']}",
        "--build-arg", f"APPLICATION_RUNTIME_IMAGE={args.application_image}",
        "--build-arg", f"PLUGIN_VERSION={plugin_version}",
        "--build-arg", f"VCS_REF={revision}",
        "--tag", args.output_image,
        "--metadata-file", str(metadata_path),
        ".",
    ]
    try:
        subprocess.run(command, cwd=REPO, check=True)
    except (OSError, subprocess.SubprocessError):
        raise ConfigError("local Docker Buildx failed; inspect the local builder diagnostics") from None
    build_inputs["local_build_completed"] = True
    _write_file(output_dir / "build-inputs.json", json.dumps(build_inputs, indent=2) + "\n", force=True)
    print(f"Built local image {args.output_image} with the fixed Nakama/Go ABI.")
    print(f"Recorded immutable inputs in {output_dir / 'build-inputs.json'}.")
    print("The application runtime image was extended in place, preserving its existing plugin modules.")
    print("This image is only built locally; it has not been pushed, deployed, or checked against GameFleet.")
    return 0


def command_render(args: argparse.Namespace) -> int:
    config = _validated(args)
    body = compose_override(config, args.runtime_image)
    args.output_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(args.output_dir, 0o700)
    manifest = {
        "kind": "local-gamefleet-service-compose-render",
        "runtime_image": args.runtime_image,
        "compose_file": str(args.output_dir / "compose.override.yaml"),
        "required_service_settings": len(SERVICE_KEYS),
        "archive_reader_enabled": config.archive_key_source is not None,
        "key_contents_read": False,
        "remote_status_checked": False,
        "deployed": False,
    }
    _write_file(args.output_dir / "compose.override.yaml", body, force=args.force)
    _write_file(args.output_dir / "render-manifest.json", json.dumps(manifest, indent=2) + "\n", force=args.force)
    print(f"Rendered local Compose override: {args.output_dir / 'compose.override.yaml'}")
    print("It only patches the existing nakama service, adds a read-only key-file bind, and adds no ports or network-mode changes.")
    print("The fragment preserves the supplied full runtime image and its existing Fixed plugin combination.")
    print("Rendered configuration is not deployed; merge and review it in the existing application Compose project.")
    return 0


def command_status(args: argparse.Namespace) -> int:
    manifest_path = args.directory / "render-manifest.json"
    if not manifest_path.is_file():
        print("Local status: no rendered service configuration found.")
    else:
        try:
            manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError, ValueError):
            raise ConfigError("local render manifest is unavailable or invalid") from None
        if manifest.get("kind") != "local-gamefleet-service-compose-render":
            raise ConfigError("local render manifest has an unexpected format")
        print("Local status: Compose override prepared.")
        print(f"Runtime image: {manifest.get('runtime_image', 'unknown')}")
    build_path = args.directory / "build-inputs.json"
    if build_path.is_file():
        try:
            build = json.loads(build_path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError, ValueError):
            raise ConfigError("local build manifest is unavailable or invalid") from None
        if build.get("kind") == "local-gamefleet-service-runtime-build":
            print(f"Local build: {'completed' if build.get('local_build_completed') else 'not completed'}; output {build.get('output_tag', 'unknown')}.")
    print("Deployment status: not checked; this command makes no network or Docker-daemon request.")
    return 0


def make_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)

    init = commands.add_parser("init", help="create a secret-free env template")
    init.add_argument("--output", type=Path, default=DEFAULT_DIR / "gamefleet-service.env")
    init.set_defaults(run=command_init)

    def add_config_options(command: argparse.ArgumentParser, *, archive: bool = True) -> None:
        command.add_argument("--env", type=Path, required=True, help="plain KEY=VALUE file; never evaluated as shell")
        command.add_argument("--key-source", type=Path, required=True, help="local key file path; only POSIX metadata is checked")
        if archive:
            command.add_argument("--archive-key-source", type=Path, help="separate local archive key file when archive settings are enabled")

    validate = commands.add_parser("validate", help="check service config and local key-file metadata")
    add_config_options(validate)
    validate.set_defaults(run=command_validate)

    plan = commands.add_parser("plan", help="show the local build/render plan without applying it")
    add_config_options(plan)
    plan.add_argument("--application-image", required=True, help="full Fixed/application Nakama image pinned by sha256 digest")
    plan.set_defaults(run=command_plan)

    build = commands.add_parser("build", help="build a local service image while retaining application modules")
    add_config_options(build)
    build.add_argument("--application-image", required=True, help="full Fixed/application Nakama image pinned by sha256 digest")
    build.add_argument("--output-image", required=True, help="local output image tag; do not use --push")
    build.add_argument("--plugin-version")
    build.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    build.set_defaults(run=command_build)

    render = commands.add_parser("render", help="render a local Compose override; does not deploy")
    add_config_options(render)
    render.add_argument("--runtime-image", required=True, help="published full runtime image pinned by sha256 digest")
    render.add_argument("--output-dir", type=Path, default=DEFAULT_DIR)
    render.add_argument("--force", action="store_true", help="replace only the generated files in the output directory")
    render.set_defaults(run=command_render)

    status = commands.add_parser("status", help="report local prepared files without checking deployment")
    status.add_argument("--directory", type=Path, default=DEFAULT_DIR)
    status.set_defaults(run=command_status)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = make_parser()
    args = parser.parse_args(argv)
    try:
        return args.run(args)
    except ConfigError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    except OSError:
        print("error: local file operation failed", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
