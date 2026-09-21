#!/usr/bin/env python3
"""Read the documented Fleet API through an SSH tunnel without exposing its token."""
import argparse
import ipaddress
import json
import os
from pathlib import Path
import re
import stat
import sys
import urllib.error
import urllib.parse
import urllib.request

FILTERS = {
    "overview": ("region",),
    "rooms": ("region", "state", "worker_id", "room_id", "user_id", "offset", "limit"),
    "instances": ("region", "state", "worker_id", "node", "offset", "limit"),
    "nodes": ("region", "name", "role", "ready", "offset", "limit"),
    "events": ("region", "namespace", "object_name", "type", "reason", "offset", "limit"),
    "logs": ("region", "namespace", "pod", "container", "mode", "minutes", "limit", "search", "before"),
}
MAX_RESPONSE = 8 * 1024 * 1024


class QueryError(Exception):
    pass


def read_credentials(path):
    try:
        before = path.lstat()
        if not stat.S_ISREG(before.st_mode) or before.st_size > 16384:
            raise QueryError("credentials must be a small regular file")
        if os.name != "nt" and stat.S_IMODE(before.st_mode) != 0o600:
            raise QueryError("credentials file must have mode 0600")
        with path.open("rb") as handle:
            after = os.fstat(handle.fileno())
            if (before.st_dev, before.st_ino) != (after.st_dev, after.st_ino):
                raise QueryError("credentials file changed while opening")
            data = json.loads(handle.read(16385))
        if not isinstance(data, dict) or set(data) != {"base_url", "token"}:
            raise QueryError("credentials require exactly base_url and token")
        base, token = data["base_url"], data["token"]
        if not isinstance(base, str) or not isinstance(token, str):
            raise QueryError("invalid credential fields")
        if not re.fullmatch(r"fcro_[A-Za-z0-9_-]{43}", token):
            raise QueryError("expected an independent read-only API token")
        uri = urllib.parse.urlsplit(base)
        loopback = uri.hostname == "localhost"
        if not loopback:
            try:
                loopback = ipaddress.ip_address(uri.hostname).is_loopback
            except ValueError:
                loopback = False
        if (uri.scheme not in ("http", "https") or not loopback or uri.username is not None
                or uri.password is not None or uri.path != "/fleet-admin/" or uri.query or uri.fragment
                or not uri.port or any(c in base for c in "\r\n\t ")):
            raise QueryError("base_url must be the SSH loopback /fleet-admin/ URL with a port")
        return base, token
    except QueryError:
        raise
    except (OSError, ValueError, TypeError, KeyError):
        raise QueryError("cannot read valid private credentials") from None


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def query(base, token, resource, params):
    if resource not in FILTERS or any(k not in FILTERS[resource] for k in params):
        raise QueryError("unsupported resource or query filter")
    target = base + "api/v1/" + resource
    if params:
        target += "?" + urllib.parse.urlencode(params)
    request = urllib.request.Request(target, headers={"Authorization": "Bearer " + token,
                                                    "Accept": "application/json"}, method="GET")
    # Never forward credentials to an ambient HTTP proxy or redirected host.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=15) as response:
            body = response.read(MAX_RESPONSE + 1)
            if len(body) > MAX_RESPONSE:
                raise QueryError("API response exceeded limit")
            if token.encode() in body:
                raise QueryError("API response unexpectedly contained a credential")
            value = json.loads(body)
            if not isinstance(value, dict) or value.get("api_version") != "v1" or value.get("read_only") is not True:
                raise QueryError("unexpected API response contract")
            return value
    except urllib.error.HTTPError as error:
        # Stable status is enough; never echo an arbitrary proxy error body.
        raise QueryError("API returned HTTP " + str(error.code)) from None
    except (urllib.error.URLError, OSError, ValueError):
        raise QueryError("API unavailable or response invalid; check the SSH tunnel and console") from None


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--credentials", required=True, type=Path,
                        help="private JSON file containing base_url and token (0600 on POSIX)")
    commands = parser.add_subparsers(dest="resource", required=True)
    for resource, filters in FILTERS.items():
        command = commands.add_parser(resource)
        for key in filters:
            command.add_argument("--" + key.replace("_", "-"), dest=key)
    args = parser.parse_args(argv)
    try:
        base, token = read_credentials(args.credentials)
        params = {k: getattr(args, k) for k in FILTERS[args.resource] if getattr(args, k) is not None}
        result = query(base, token, args.resource, params)
    except QueryError as error:
        print("fleet-console-query: " + str(error), file=sys.stderr)
        return 1
    json.dump(result, sys.stdout, ensure_ascii=False, indent=2)
    print()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
