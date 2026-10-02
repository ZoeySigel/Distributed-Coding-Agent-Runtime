#!/bin/sh
set -eu
cd "$(dirname "$0")/.."
if [ -e .env ] || [ -e secrets ]; then echo 'Existing configuration; refusing to overwrite.' >&2; exit 1; fi
umask 077
mkdir secrets
token() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
printf 'POSTGRES_PASSWORD=%s\nS3_SECRET_KEY=%s\n' "$(token)" "$(token)" > .env
printf '{"local":"%s"}\n' "$(token)" > secrets/api-tokens.json
token > secrets/worker-token
printf '{}' > secrets/repositories.json
: > secrets/model-key
mkdir secrets/publication
printf '{}' > secrets/publication/repositories.json
chmod 755 secrets/publication
chmod 644 secrets/publication/repositories.json
# Non-root API/gateway containers need read access to bind-mounted files.
chmod 700 secrets
chmod 644 secrets/*
chmod 755 secrets/publication
echo 'Created configuration. Add the model key to secrets/model-key. Protect this directory with host ACLs.'
