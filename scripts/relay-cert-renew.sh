#!/usr/bin/env bash
# relay-cert-renew.sh — renew the end-to-end relay certificate for THIS server
# (Let's Encrypt via NullBore's DNS-01 hook) and drop it into data/tls, where
# the running server picks it up without a restart (certReloader).
#
# THE KEY IS REUSED (--reuse-key): the pin IS the key, and a renewal must never
# make a paired phone re-pair. Rotate the key only deliberately, and say why in
# the UI. Runs weekly from cron; lego only renews inside the last 30 days.
# Interim for PJ's own server; the product path is ACME inside the Go server.
set -euo pipefail
SERVER="/home/pj/projects/jarvis/abookify/engineering/server"
cd "$SERVER"
set -a; . ../relay/.env; set +a
HOST="$(curl -s --max-time 5 http://localhost:7654/api/info | grep -oP '(?<="tls_url":"https://)[^"]*' || true)"
[[ -n "$HOST" ]] || { echo "renew: no tls_url from the server — skipped"; exit 0; }
docker image inspect lego-nb >/dev/null 2>&1 || { echo "renew: lego-nb image missing (scratch build: goacme/lego + nullbore)"; exit 1; }
docker run --rm -e NULLBORE_API_KEY="$NULLBORE_API_KEY" -e NULLBORE_SERVER="${NULLBORE_SERVER:-https://tunnel.nullbore.com}" \
  -e EXEC_PATH=/usr/local/bin/nullbore-acme-hook -v legoprod:/data --entrypoint /lego lego-nb \
  renew --path /data --dns exec --domains "$HOST" --days 30 --reuse-key 2>&1 | tail -3
docker run --rm -v legoprod:/data -v "$SERVER/data/tls:/tls" alpine sh -c '
  cd /data/certificates; f=$(ls *.crt | grep -v issuer | head -1); k=${f%.crt}.key
  if ! cmp -s "$k" /tls/server.key; then echo "renew: KEY CHANGED — refusing to install (pin would move); investigate"; exit 3; fi
  cat "$f" "${f%.crt}.issuer.crt" > /tls/server.crt.new && mv /tls/server.crt.new /tls/server.crt && echo "renew: certificate installed, key unchanged"'
