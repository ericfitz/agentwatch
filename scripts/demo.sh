#!/usr/bin/env bash
# Builds agentwatch, mints both tokens, makes a self-signed certificate for
# localhost, starts a TLS relay, and wraps an interactive bash in it. Open the
# printed URL, accept the certificate warning, and paste the viewer token.
# Exit the shell to stop the demo. Set AGENTWATCH_DEMO_ADDR to change the
# listen address (default 127.0.0.1:8443).
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
addr=${AGENTWATCH_DEMO_ADDR:-127.0.0.1:8443}
work=$(mktemp -d "${TMPDIR:-/tmp}/agentwatch-demo.XXXXXX")

cd "$root"
go build -o "$work/agentwatch" ./cmd/agentwatch
go run ./scripts/gencert "$work"

AGENTWATCH_WRAPPER_TOKEN=$("$work/agentwatch" token)
AGENTWATCH_VIEWER_TOKEN=$("$work/agentwatch" token)
export AGENTWATCH_WRAPPER_TOKEN AGENTWATCH_VIEWER_TOKEN

"$work/agentwatch" relay --listen "$addr" --tls-cert "$work/cert.pem" --tls-key "$work/key.pem" 2>"$work/relay.log" &
relay=$!
trap 'kill "$relay" 2>/dev/null || true; wait "$relay" 2>/dev/null || true' EXIT

for _ in $(seq 50); do
	if curl -fsk "https://$addr/healthz" >/dev/null 2>&1; then
		break
	fi
	if ! kill -0 "$relay" 2>/dev/null; then
		echo "The relay did not start:" >&2
		cat "$work/relay.log" >&2
		exit 1
	fi
	sleep 0.1
done

cat <<EOF
Open:          https://$addr/
Viewer token:  $AGENTWATCH_VIEWER_TOKEN
Recordings:    $work/sessions/
Relay log:     $work/relay.log

Starting bash under agentwatch. Exit the shell to stop the demo.

EOF

"$work/agentwatch" run --relay "https://$addr" --ca-file "$work/cert.pem" --name demo \
	--record "$work/sessions/" -- bash --noprofile --norc
