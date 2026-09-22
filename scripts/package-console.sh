#!/bin/sh
# Build the optional console without pulling/rebuilding the Nakama runtime.
set -eu
repo_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$repo_dir"
requested_version=${PLUGIN_VERSION:-}
. ./deploy/compatibility.env
plugin_version=${requested_version:-$PLUGIN_VERSION}
case "$plugin_version" in
  *[!a-zA-Z0-9._-]*|'') printf 'Invalid PLUGIN_VERSION.\n' >&2; exit 1 ;;
esac
console_go=${GO:-go}
test "$("$console_go" env GOVERSION)" = "go$GO_VERSION" || { echo "Use Go $GO_VERSION from deploy/compatibility.env." >&2; exit 1; }
artifact_name="nakama-agones-console-$plugin_version-linux-amd64"
mkdir -p "$repo_dir/dist"
package_stage=$(mktemp -d "$repo_dir/dist/.console-package.XXXXXX")
trap 'rm -rf "$package_stage"' EXIT HUP INT TERM
artifact_dir="$package_stage/$artifact_name"
mkdir -p "$artifact_dir/bin" "$artifact_dir/deploy" "$artifact_dir/scripts" "$artifact_dir/docs"
for tool in fleet-console fleet-console-control; do
  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 "$console_go" build -trimpath -mod=readonly -o "$artifact_dir/bin/$tool" "./cmd/$tool"
done
# Explicit manifest: never bundle local configs, tokens or Python caches.
python3 - "$artifact_dir" <<'PUBLIC_FILES'
import shutil, sys
from pathlib import Path
root = Path(sys.argv[1])
manifest = {
    'console': ['config.example.json', 'control.example.json', 'operations.example.json',
                'fleet-console.service', 'fleet-console-control.service',
                'fleet-console-export.service', 'fleet-console-export.timer',
                'observer-rbac.yaml'],
    'enrollment': ['00-namespaces.yaml', '10-crds.yaml', '20-rbac.yaml',
                   '25-secret-admission.yaml', '30-retirement-admission.yaml',
                   '31-retirement-rbac.yaml', 'Dockerfile', 'base-image.txt',
                   'config.example.json', 'submitter-token-sync.env.example'],
    'observability': ['00-namespace.yaml', '10-loki.yaml', '20-alloy.yaml',
                      '30-console-log-reader.yaml', '40-network-policy.yaml', 'validate.py'],
}
for folder, names in manifest.items():
    target = root/'deploy'/folder
    target.mkdir()
    for name in names:
        source = Path('deploy')/folder/name
        if source.is_symlink() or not source.is_file():
            raise SystemExit('Missing regular public package file: '+str(source))
        shutil.copy2(source, target/name)
PUBLIC_FILES
cp scripts/node_enrollment_controller.py scripts/render_node_enrollment.py scripts/node_onboarding.py scripts/k3s_node.py scripts/console_query.py scripts/export_console_status.py scripts/issue_nakama_token.py scripts/sync_nakama_token.py "$artifact_dir/scripts/"
cp docs/console*.md docs/node-enrollment*.md docs/nodes.md docs/credentials.md docs/token-sync.md "$artifact_dir/docs/"
cp LICENSE "$artifact_dir/"
python3 - "$artifact_dir" <<'CHECKSUMS'
import hashlib, sys
from pathlib import Path
root = Path(sys.argv[1])
files = sorted(p for p in root.rglob('*') if p.is_file() and p.name != 'SHA256SUMS')
(root/'SHA256SUMS').write_text(''.join(hashlib.sha256(p.read_bytes()).hexdigest()+'  '+str(p.relative_to(root))+'\n' for p in files))
CHECKSUMS
# Only public source files and fresh binaries enter the archive.
COPYFILE_DISABLE=1 tar -czf "$package_stage/package.tar.gz" -C "$package_stage" "$artifact_name"
mv "$package_stage/package.tar.gz" "$repo_dir/dist/$artifact_name.tar.gz"
printf 'Created %s/dist/%s.tar.gz\n' "$repo_dir" "$artifact_name"
