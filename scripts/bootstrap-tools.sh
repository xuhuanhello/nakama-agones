#!/bin/sh
set -eu
cd "$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
mkdir -p .local/tools
os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in arm64|aarch64) arch=arm64;; x86_64|amd64) arch=amd64;; *) echo 'Unsupported host architecture' >&2; exit 1;; esac
case "$os" in darwin|linux) ;; *) echo 'Use a Linux shell or WSL2' >&2; exit 1;; esac
cd .local/tools
curl -fsSL "https://github.com/k3d-io/k3d/releases/download/v5.9.0/k3d-$os-$arch" -o "k3d-$os-$arch"
curl -fsSL https://github.com/k3d-io/k3d/releases/download/v5.9.0/checksums.txt -o k3d-checksums.txt
curl -fsSL "https://get.helm.sh/helm-v3.22.0-$os-$arch.tar.gz" -o helm.tar.gz
curl -fsSL "https://get.helm.sh/helm-v3.22.0-$os-$arch.tar.gz.sha256sum" -o helm.sha256sum
python3 - "$os" "$arch" <<'CHECK'
import hashlib,pathlib,sys
os,arch=sys.argv[1:]; name=f'k3d-{os}-{arch}'
checks={line.split()[1].lstrip('*'):line.split()[0] for line in pathlib.Path('k3d-checksums.txt').read_text().splitlines() if len(line.split())==2}
for filename,expected in [(name,checks[name]),('helm.tar.gz',pathlib.Path('helm.sha256sum').read_text().split()[0])]:
    if hashlib.sha256(pathlib.Path(filename).read_bytes()).hexdigest()!=expected:
        raise SystemExit('Tool checksum mismatch')
print('Official tool checksums verified')
CHECK
cp "k3d-$os-$arch" k3d
# Verified official archive; extract only the required executable.
tar -xzf helm.tar.gz "$os-$arch/helm"
cp "$os-$arch/helm" helm
chmod 755 k3d helm
