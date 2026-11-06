#!/usr/bin/env bash
# End-to-end test: 5 containers, chained joins, cross downloads checked by
# checksum, revocation, image size and idle memory. Needs docker, curl,
# python3 and sha256sum.
#
#   deploy/e2e.sh                         builds cercle:e2e, then tests it
#   CERCLE_IMAGE=ghcr.io/… deploy/e2e.sh  tests an existing image
#   CERCLE_KEEP=1 deploy/e2e.sh           leaves the containers running
set -euo pipefail
cd "$(dirname "$0")"
DATA="$PWD/e2e-data"
COMPOSE=(docker compose -p cercle-e2e -f compose.test.yml)
export CERCLE_IMAGE="${CERCLE_IMAGE:-cercle:e2e}"

say() { printf '\n== %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; "${COMPOSE[@]}" logs --tail 30 >&2 || true; exit 1; }
ui() { # ui <n> <method> <path> [body]
  local n=$1 method=$2 path=$3 body=${4:-}
  local args=(-sS --max-time "${UI_TIMEOUT:-300}" -u x:e2e -H 'X-Cercle-UI: 1' -X "$method")
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  curl "${args[@]}" "http://127.0.0.1:1818$n/ui/api$path"
}
enc() { python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=''))" "$1"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_for() { # wait_for <seconds> <description> <command…>
  local limit=$1 what=$2; shift 2
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    (( SECONDS - start < limit )) || fail "${limit} s timeout exceeded: $what"
    sleep 1
  done
  echo "  $what: $(( SECONDS - start )) s"
}

cleanup() {
  if [[ -z "${CERCLE_KEEP:-}" ]]; then
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ "$CERCLE_IMAGE" == cercle:e2e ]]; then
  say "Building the image"
  docker build -q --network host -f Dockerfile --build-arg VERSION=e2e --build-arg COMMIT="$(git -C .. rev-parse --short HEAD 2>/dev/null || echo unknown)" -t cercle:e2e .. >/dev/null
fi
size=$(docker image inspect "$CERCLE_IMAGE" --format '{{.Size}}')
echo "  image: $(( size / 1024 / 1024 )) MiB"
(( size < 30 * 1024 * 1024 )) || fail "image larger than 30 MB"

say "Preparing data"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
rm -rf "${DATA:?}"
for n in 1 2 3 4 5; do
  mkdir -p "$DATA/node$n"/{share/docs,share/media,data,downloads}
  head -c $(( 3 * 1024 * 1024 + n )) /dev/urandom > "$DATA/node$n/share/media/video-$n.bin"
  for i in $(seq 1 20); do head -c $(( 1000 * i + n )) /dev/urandom > "$DATA/node$n/share/docs/doc-$i.txt"; done
  echo "hidden file" > "$DATA/node$n/share/.cache"
done
if [[ $(id -u) == 0 ]]; then
  export CERCLE_UID=65532 CERCLE_GID=65532
  chown -R 65532:65532 "$DATA"
else
  export CERCLE_UID=$(id -u) CERCLE_GID=$(id -g)
fi

say "Starting the 5 nodes"
"${COMPOSE[@]}" up -d >/dev/null
for n in 1 2 3 4 5; do
  wait_for 30 "node$n ready" curl -sf "http://127.0.0.1:1818$n/healthz"
done

say "Creating the circle and chained joins (1→2→3→4, 1→5)"
ip=$(ui 1 POST /circle | json 'd["self"]["tunnel_ip"]')
[[ "$ip" == 10.42.0.1 ]] || fail "founder at $ip"
echo "  node1: $ip"
join() { # join <inviter> <invitee>
  local code
  code=$(ui "$1" POST /invites | json 'd["code"]')
  ui "$2" POST /join "{\"code\":\"$code\"}" | json 'd.get("self",{}).get("tunnel_ip") or d.get("error")'
}
for pair in "1 2" "2 3" "3 4" "1 5"; do
  set -- $pair
  echo "  node$2 invited by node$1: $(join "$1" "$2")"
done
code=$(ui 1 POST /invites | json 'd["code"]')
ui 2 POST /join "{\"code\":\"$code\"}" | grep -q "already belongs" || fail "a member could join twice"

online_count() { ui "$1" GET /members | json 'sum(1 for m in d if m["online"] and not m["self"])'; }
all_online() { [[ $(online_count "$1") == 4 ]]; }
for n in 1 2 3 4 5; do wait_for 120 "node$n sees the 4 others online" all_online "$n"; done

id_of() { ui 1 GET /members | json "[m['node_id'] for m in d if m['name']=='node$1'][0]"; }
declare -A ID
for n in 1 2 3 4 5; do ID[$n]=$(id_of "$n"); done

say "Cross download: each node takes the whole share of the next one"
for n in 1 2 3 4 5; do
  src=$(( n % 5 + 1 ))
  added=$(ui "$n" POST /queue "{\"peer\":\"${ID[$src]}\",\"paths\":[\"\"]}" | json 'd["added"]')
  [[ "$added" == 21 ]] || fail "node$n: $added items queued, want 21 (the hidden file is excluded)"
done
done_count() { ui "$1" GET '/queue?status=done&limit=1' | json 'd["summary"]["count"].get("done",0)'; }
all_done() { [[ $(done_count "$1") == 21 ]]; }
for n in 1 2 3 4 5; do wait_for 180 "node$n received everything" all_done "$n"; done

say "Checksums"
for n in 1 2 3 4 5; do
  src=$(( n % 5 + 1 ))
  (cd "$DATA/node$src/share" && find . -type f ! -name '.*' -print0 | sort -z | xargs -0 sha256sum) > "$DATA/want"
  (cd "$DATA/node$n/downloads/node$src" && find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$DATA/got"
  cmp -s "$DATA/want" "$DATA/got" || { diff "$DATA/want" "$DATA/got" | head; fail "node$n: files received from node$src differ"; }
  echo "  node$n ← node$src: $(wc -l < "$DATA/got") identical files"
done

say "Idle memory (after 40 s without transfers)"
sleep 40
for n in 1 2 3 4 5; do
  mem=$(docker stats --no-stream --format '{{.MemUsage}}' "cercle-e2e-node$n-1" | cut -d/ -f1 | tr -d ' ')
  echo "  node$n: $mem"
  python3 - "$mem" <<'EOF' || fail "node$n uses more than 80 MB when idle"
import re, sys
v, u = re.match(r"([\d.]+)(\w+)", sys.argv[1]).groups()
mib = float(v) * {"B": 1 / 2**20, "KiB": 1 / 1024, "MiB": 1, "GiB": 1024}[u]
sys.exit(0 if mib < 80 else 1)
EOF
done

say "node2 revokes node5"
ui 2 POST "/members/$(enc "${ID[5]}")/revoke" >/dev/null
revoked_everywhere() {
  for n in 1 2 3 4; do
    [[ $(ui "$n" GET /members | json "[m['revoked'] for m in d if m['name']=='node5'][0]") == True ]] || return 1
  done
}
wait_for 120 "node5 revoked everywhere" revoked_everywhere
node5_cut() { # the 4 attempts in parallel, 5 s each: a share received = not cut off yet
  local n pids=() ok=0
  for n in 1 2 3 4; do
    ( UI_TIMEOUT=5 ui 5 GET "/members/$(enc "${ID[$n]}")/files?path=" 2>/dev/null | grep -q '"entries"' ) &
    pids+=($!)
  done
  for p in "${pids[@]}"; do if wait "$p"; then ok=1; fi; done
  (( ok == 0 ))
}
wait_for 120 "node5 can no longer browse any share" node5_cut

say "Restarting node1: the revocation holds, node2 stays reachable"
"${COMPOSE[@]}" restart node1 >/dev/null
wait_for 30 "node1 ready" curl -sf http://127.0.0.1:18181/healthz
node1_sees_node2() { ui 1 GET /members | json "[m['online'] for m in d if m['name']=='node2'][0]" | grep -q True; }
wait_for 120 "node1 reaches node2" node1_sees_node2
[[ $(ui 1 GET /members | json "[m['revoked'] for m in d if m['name']=='node5'][0]") == True ]] || fail "revocation lost on restart"
sleep 3
if ui 5 GET "/members/$(enc "${ID[1]}")/files?path=" | grep -q '"entries"'; then
  fail "node5 browses node1 after its restart"
fi

say "Success: all scenarios passed"
