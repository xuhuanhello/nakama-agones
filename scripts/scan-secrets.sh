#!/bin/sh
set -eu
case "$(uname -s)-$(uname -m)" in Linux-x86_64) ;; *) echo 'CI scanner targets Linux amd64; use your installed gitleaks elsewhere.' >&2; exit 1;; esac
scanner_dir=$(mktemp -d)
trap 'rm -rf "$scanner_dir"' EXIT
curl -fsSL https://github.com/gitleaks/gitleaks/releases/download/v8.30.1/gitleaks_8.30.1_linux_x64.tar.gz -o "$scanner_dir/gitleaks.tar.gz"
printf '%s  %s\n' '551f6fc83ea457d62a0d98237cbad105af8d557003051f41f3e7ca7b3f2470eb' "$scanner_dir/gitleaks.tar.gz" | sha256sum --check -
tar -xzf "$scanner_dir/gitleaks.tar.gz" -C "$scanner_dir" gitleaks
"$scanner_dir/gitleaks" git --redact=100 --no-banner --log-opts='--all' .
