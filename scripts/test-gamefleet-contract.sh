#!/usr/bin/env bash
# Run against an explicit candidate checkout, without editing either go.mod or
# the GameFleet source tree. No production config or external credentials used.
set -euo pipefail
if [[ $# != 1 ]]; then
  echo "usage: $0 /path/to/selfhosted-gamefleet-candidate" >&2
  exit 2
fi
bridge_root="$(cd "$(dirname "$0")/.." && pwd)"
platform_root="$(cd "$1" && pwd)"
test -f "$platform_root/internal/platform/business_current_reservation_http_test.go"
contract_tmp="$(mktemp -d "${TMPDIR:-/tmp}/nakama-gamefleet-contract.XXXXXX")"
trap 'rm -rf "$contract_tmp"' EXIT
cp "$platform_root/go.mod" "$contract_tmp/contract.mod"
cp "$platform_root/go.sum" "$contract_tmp/contract.sum"
python3 - "$bridge_root" "$platform_root" "$contract_tmp/overlay.json" <<'PY'
import json, pathlib, sys
bridge, platform, target = map(pathlib.Path, sys.argv[1:])
target.write_text(json.dumps({"Replace": {
    str(platform / "internal/platform/nakama_bridge_contract_test.go"):
        str(bridge / "tests/contracts/gamefleet_business_test.go.txt")
}}))
PY
go mod edit -modfile="$contract_tmp/contract.mod" -go=1.27.1 \
  -require=github.com/xuhuanhello/nakama-agones@v0.0.0 \
  "-replace=github.com/xuhuanhello/nakama-agones=$bridge_root"
cd "$platform_root"
GOTOOLCHAIN=go1.27.1 go test -mod=mod -modfile="$contract_tmp/contract.mod" \
  -overlay="$contract_tmp/overlay.json" -race ./internal/platform \
  -run '^TestNakamaGameFleetClientContract$' -count=1 -timeout=180s
