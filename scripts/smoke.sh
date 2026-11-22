#!/bin/sh
# smoke.sh — the README's quick start, executed.
#
# Unit tests prove functions; this proves the WIRING: an operator following the documentation gets a
# board that accepts a node, places an item, claims it, reports it, and can be read back. It runs the
# shipped binary over real HTTP with no test hooks, which is the only way to notice that a route
# moved, a flag was renamed, or the docs describe a previous version.
#
# Every wait is bounded and every failure prints what it saw. A smoke test that hangs is worse than
# no smoke test: it turns a red build into a stuck one.
set -eu

root=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/nsm-smoke.XXXXXX")
# $$ rather than $RANDOM: /bin/sh is dash on Debian and Ubuntu, and dash has no RANDOM. The PID is
# unique per run, which is all these three need.
admin=smoke-admin-$$
enroll=smoke-enroll-$$
port=$((18000 + $$ % 2000))
board="http://127.0.0.1:$port"
fail() { printf '\nFAIL: %s\n' "$1" >&2; printf -- '--- manager log ---\n' >&2; cat "$work/manager.log" >&2 || true; exit 1; }
say() { printf '== %s ==\n' "$1"; }

cleanup() {
	[ -f "$work/pids" ] || return 0
	while read -r pid; do kill "$pid" 2>/dev/null || true; done < "$work/pids"
	wait 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

say "build"
(cd "$root" && go build -o "$work/nsm" ./cmd/nsm) || fail "build"

say "a provider the manager can probe (the board places only through healthy endpoints)"
# The fake provider binds port 0 and writes its port, so two smoke runs cannot collide.
cat > "$work/fake2.py" <<'PY'
import json, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def _ok(self):
        b = json.dumps({"object": "list", "data": [{"id": "smoke-1", "object": "model"}]}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    do_GET = _ok
    do_POST = _ok
    def log_message(self, *a): pass
srv = HTTPServer(("127.0.0.1", 0), H)
open(sys.argv[1], "w").write(str(srv.server_port))
srv.serve_forever()
PY
python3 "$work/fake2.py" "$work/provider.port" &
echo $! >> "$work/pids"
i=0
while [ ! -s "$work/provider.port" ]; do
	i=$((i + 1)); [ "$i" -lt 100 ] || fail "the fake provider did not start"
	sleep 0.05
done
provider="http://127.0.0.1:$(cat "$work/provider.port")/v1"
printf 'provider=%s\n' "$provider"

say "manager (the README's first command)"
NSM_ADMIN_TOKEN=$admin NSM_ENROLL_TOKEN=$enroll "$work/nsm" manager serve \
	--db "$work/nsm.db" --addr "127.0.0.1:$port" > "$work/manager.log" 2>&1 &
echo $! >> "$work/pids"
i=0
while ! curl -sf "$board/version" > /dev/null 2>&1; do
	i=$((i + 1)); [ "$i" -lt 100 ] || fail "the manager did not answer $board/version"
	sleep 0.1
done
curl -sf "$board/version" > "$work/version.json" || fail "version"
say "version: $(cat "$work/version.json")"

say "endpoint, model, probe (the README's placement paragraph)"
endpoint=$(curl -sf -X POST "$board/admin/endpoints" -H "Authorization: Bearer $admin" \
	-d "{\"name\":\"smoke\",\"provider\":\"openai\",\"base_url\":\"$provider\",\"reach\":\"host\"}" \
	| sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$endpoint" ] || fail "create endpoint"
curl -sf -X POST "$board/admin/models" -H "Authorization: Bearer $admin" \
	-d "{\"endpoint_id\":\"$endpoint\",\"model_id\":\"smoke-1\",\"provides\":[\"agentic\"],\"features\":[\"tools\"],\"context_window\":8192,\"cost_in_per_mtok\":1,\"cost_out_per_mtok\":1}" > /dev/null \
	|| fail "create model"
curl -sf -X POST "$board/admin/endpoints/$endpoint/probe" -H "Authorization: Bearer $admin" > /dev/null \
	|| fail "probe endpoint"

say "a node enrolls (the README's second command)"
reg=$(curl -sf -X POST "$board/nodes/register" -H "Authorization: Bearer $enroll" \
	-d '{"name":"smoke-node","role":"executor","provides":["agentic"],"reach":["host"],"concurrency":1}') \
	|| fail "register node"
node=$(printf '%s' "$reg" | sed -n 's/.*"node_id":"\([^"]*\)".*/\1/p')
token=$(printf '%s' "$reg" | sed -n 's/.*"token": *"\([^"]*\)".*/\1/p')
[ -n "$node" ] && [ -n "$token" ] || fail "the register reply did not carry node_id and token: $reg"
curl -sf -X POST "$board/nodes/heartbeat" -H "Authorization: Bearer $token" \
	-d '{"provides_live":["agentic"],"reach_classes":["host"],"protocol_version":2}' > /dev/null \
	|| fail "heartbeat"

say "an item is placed, claimed, started and reported"
item=$(curl -sf -X POST "$board/admin/items" -H "Authorization: Bearer $admin" \
	-d '{"title":"the smoke item","brief":"do nothing, successfully","needs":["agentic"]}' \
	| sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$item" ] || fail "create item"
claim=$(curl -sf -X POST "$board/claim" -H "Authorization: Bearer $token" -d '{"slots":1}') || fail "claim"
run=$(printf '%s' "$claim" | sed -n 's/.*"run_id":"\([^"]*\)".*/\1/p')
[ -n "$run" ] || fail "the claim did not place the item: $claim"
curl -sf -X POST "$board/runs/$run/start" -H "Authorization: Bearer $token" -d '{}' > /dev/null || fail "start"
curl -sf -X POST "$board/runs/$run/report" -H "Authorization: Bearer $token" \
	-d '{"status":"ok","summary":"smoke","tokens_in":10,"tokens_out":20}' > /dev/null || fail "report"

say "the board agrees"
why=$(curl -sf -H "Authorization: Bearer $admin" "$board/admin/why/$item") || fail "why"
printf '%s' "$why" | grep -q '"status":"done"' || fail "the item is not done: $why"
spend=$(curl -sf -H "Authorization: Bearer $admin" "$board/admin/spend/today") || fail "spend"
printf '%s' "$spend" | grep -q '"runs":1' || fail "the ledger does not show the run: $spend"
page=$(curl -sf -H "Authorization: Bearer $admin" "$board/admin") || fail "the board page"
printf '%s' "$page" | grep -q 'the smoke item' || fail "the board page does not list the item"
printf '%s' "$page" | grep -q '"done":1' || fail "the board page does not count the done item"

say "an unauthenticated caller is refused"
code=$(curl -s -o /dev/null -w '%{http_code}' "$board/admin")
[ "$code" = "401" ] || fail "the board answered $code without a token"

printf '\nsmoke: ok\n'
