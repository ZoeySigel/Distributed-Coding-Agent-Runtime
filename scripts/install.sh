#!/bin/sh
set -eu
project_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
install_dir=${DCAR_INSTALL_DIR:-"$HOME/.local/bin"}
api_url=${DCAR_URL:-http://localhost:8080}
mkdir -p "$install_dir"
cd "$project_root"
go build -o "$install_dir/dcar" ./cmd/dcar
"$install_dir/dcar" configure --url "$api_url" --token-file "$project_root/secrets/api-tokens.json"
printf 'Installed %s/dcar. Ensure %s is in PATH, then run dcar in your working directory.\n' "$install_dir" "$install_dir"
