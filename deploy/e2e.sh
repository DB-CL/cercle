#!/usr/bin/env bash
# Test de bout en bout : 5 conteneurs, adhésion en chaîne, téléchargement
# croisé vérifié par somme de contrôle, révocation, taille de l'image et
# mémoire au repos. Nécessite docker, curl, python3 et sha256sum.
#
#   deploy/e2e.sh                       construit cercle:e2e puis teste
#   CERCLE_IMAGE=ghcr.io/… deploy/e2e.sh  teste une image existante
#   CERCLE_KEEP=1 deploy/e2e.sh         laisse les conteneurs tourner à la fin
set -euo pipefail
cd "$(dirname "$0")"
DATA="$PWD/e2e-data"
COMPOSE=(docker compose -p cercle-e2e -f compose.test.yml)
export CERCLE_IMAGE="${CERCLE_IMAGE:-cercle:e2e}"

say() { printf '\n== %s\n' "$*"; }
fail() { printf 'ÉCHEC : %s\n' "$*" >&2; "${COMPOSE[@]}" logs --tail 30 >&2 || true; exit 1; }
ui() { # ui <n> <méthode> <chemin> [corps]
  local n=$1 method=$2 path=$3 body=${4:-}
  local args=(-sS --max-time "${UI_TIMEOUT:-300}" -u x:e2e -H 'X-Cercle-UI: 1' -X "$method")
  if [[ -n "$body" ]]; then args+=(-H 'Content-Type: application/json' -d "$body"); fi
  curl "${args[@]}" "http://127.0.0.1:1818$n/ui/api$path"
}
enc() { python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=''))" "$1"; }
json() { python3 -c "import json,sys; d=json.load(sys.stdin); print($1)"; }
wait_for() { # wait_for <secondes> <description> <commande…>
  local limit=$1 what=$2; shift 2
  local start=$SECONDS
  until "$@" >/dev/null 2>&1; do
    (( SECONDS - start < limit )) || fail "délai de ${limit} s dépassé : $what"
    sleep 1
  done
  echo "  $what : $(( SECONDS - start )) s"
}

cleanup() {
  if [[ -z "${CERCLE_KEEP:-}" ]]; then
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

if [[ "$CERCLE_IMAGE" == cercle:e2e ]]; then
  say "Construction de l'image"
  docker build -q --network host -f Dockerfile --build-arg VERSION=e2e --build-arg COMMIT="$(git -C .. rev-parse --short HEAD 2>/dev/null || echo inconnu)" -t cercle:e2e .. >/dev/null
fi
size=$(docker image inspect "$CERCLE_IMAGE" --format '{{.Size}}')
echo "  image : $(( size / 1024 / 1024 )) Mio"
(( size < 30 * 1024 * 1024 )) || fail "image de plus de 30 Mo"

say "Préparation des données"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
rm -rf "${DATA:?}"
for n in 1 2 3 4 5; do
  mkdir -p "$DATA/node$n"/{share/docs,share/media,data,downloads}
  head -c $(( 3 * 1024 * 1024 + n )) /dev/urandom > "$DATA/node$n/share/media/video-$n.bin"
  for i in $(seq 1 20); do head -c $(( 1000 * i + n )) /dev/urandom > "$DATA/node$n/share/docs/doc-$i.txt"; done
  echo "fichier caché" > "$DATA/node$n/share/.cache"
done
if [[ $(id -u) == 0 ]]; then
  export CERCLE_UID=65532 CERCLE_GID=65532
  chown -R 65532:65532 "$DATA"
else
  export CERCLE_UID=$(id -u) CERCLE_GID=$(id -g)
fi

say "Démarrage des 5 nœuds"
"${COMPOSE[@]}" up -d >/dev/null
for n in 1 2 3 4 5; do
  wait_for 30 "node$n prêt" curl -sf "http://127.0.0.1:1818$n/healthz"
done

say "Création du cercle et adhésions en chaîne (1→2→3→4, 1→5)"
ip=$(ui 1 POST /circle | json 'd["self"]["tunnel_ip"]')
[[ "$ip" == 10.42.0.1 ]] || fail "fondateur en $ip"
echo "  node1 : $ip"
join() { # join <invitant> <invité>
  local code
  code=$(ui "$1" POST /invites | json 'd["code"]')
  ui "$2" POST /join "{\"code\":\"$code\"}" | json 'd.get("self",{}).get("tunnel_ip") or d.get("error")'
}
for pair in "1 2" "2 3" "3 4" "1 5"; do
  set -- $pair
  echo "  node$2 invité par node$1 : $(join "$1" "$2")"
done
code=$(ui 1 POST /invites | json 'd["code"]')
ui 2 POST /join "{\"code\":\"$code\"}" | grep -q "appartient déjà" || fail "un membre a pu rejoindre deux fois"

online_count() { ui "$1" GET /members | json 'sum(1 for m in d if m["online"] and not m["self"])'; }
all_online() { [[ $(online_count "$1") == 4 ]]; }
for n in 1 2 3 4 5; do wait_for 120 "node$n voit les 4 autres en ligne" all_online "$n"; done

id_of() { ui 1 GET /members | json "[m['node_id'] for m in d if m['name']=='node$1'][0]"; }
declare -A ID
for n in 1 2 3 4 5; do ID[$n]=$(id_of "$n"); done

say "Téléchargement croisé : chaque nœud prend tout le partage du suivant"
for n in 1 2 3 4 5; do
  src=$(( n % 5 + 1 ))
  added=$(ui "$n" POST /queue "{\"peer\":\"${ID[$src]}\",\"paths\":[\"\"]}" | json 'd["added"]')
  [[ "$added" == 21 ]] || fail "node$n : $added éléments mis en file, 21 attendus (le fichier caché est exclu)"
done
done_count() { ui "$1" GET '/queue?status=done&limit=1' | json 'd["summary"]["count"].get("done",0)'; }
all_done() { [[ $(done_count "$1") == 21 ]]; }
for n in 1 2 3 4 5; do wait_for 180 "node$n a tout reçu" all_done "$n"; done

say "Sommes de contrôle"
for n in 1 2 3 4 5; do
  src=$(( n % 5 + 1 ))
  (cd "$DATA/node$src/share" && find . -type f ! -name '.*' -print0 | sort -z | xargs -0 sha256sum) > "$DATA/want"
  (cd "$DATA/node$n/downloads/node$src" && find . -type f -print0 | sort -z | xargs -0 sha256sum) > "$DATA/got"
  cmp -s "$DATA/want" "$DATA/got" || { diff "$DATA/want" "$DATA/got" | head; fail "node$n : fichiers reçus de node$src différents"; }
  echo "  node$n ← node$src : $(wc -l < "$DATA/got") fichiers identiques"
done

say "Mémoire au repos (après 40 s sans transfert)"
sleep 40
for n in 1 2 3 4 5; do
  mem=$(docker stats --no-stream --format '{{.MemUsage}}' "cercle-e2e-node$n-1" | cut -d/ -f1 | tr -d ' ')
  echo "  node$n : $mem"
  python3 - "$mem" <<'EOF' || fail "node$n consomme plus de 80 Mo au repos"
import re, sys
v, u = re.match(r"([\d.]+)(\w+)", sys.argv[1]).groups()
mib = float(v) * {"B": 1 / 2**20, "KiB": 1 / 1024, "MiB": 1, "GiB": 1024}[u]
sys.exit(0 if mib < 80 else 1)
EOF
done

say "Révocation de node5 par node2"
ui 2 POST "/members/$(enc "${ID[5]}")/revoke" >/dev/null
revoked_everywhere() {
  for n in 1 2 3 4; do
    [[ $(ui "$n" GET /members | json "[m['revoked'] for m in d if m['name']=='node5'][0]") == True ]] || return 1
  done
}
wait_for 120 "node5 révoqué chez tous" revoked_everywhere
node5_cut() { # les 4 essais en parallèle, 5 s chacun : un partage obtenu = pas encore coupé
  local n pids=() ok=0
  for n in 1 2 3 4; do
    ( UI_TIMEOUT=5 ui 5 GET "/members/$(enc "${ID[$n]}")/files?path=" 2>/dev/null | grep -q '"entries"' ) &
    pids+=($!)
  done
  for p in "${pids[@]}"; do if wait "$p"; then ok=1; fi; done
  (( ok == 0 ))
}
wait_for 120 "node5 ne peut plus parcourir aucun partage" node5_cut

say "Redémarrage de node1 : la révocation tient, node2 reste joignable"
"${COMPOSE[@]}" restart node1 >/dev/null
wait_for 30 "node1 prêt" curl -sf http://127.0.0.1:18181/healthz
node1_sees_node2() { ui 1 GET /members | json "[m['online'] for m in d if m['name']=='node2'][0]" | grep -q True; }
wait_for 120 "node1 joint node2" node1_sees_node2
[[ $(ui 1 GET /members | json "[m['revoked'] for m in d if m['name']=='node5'][0]") == True ]] || fail "révocation perdue au redémarrage"
sleep 3
if ui 5 GET "/members/$(enc "${ID[1]}")/files?path=" | grep -q '"entries"'; then
  fail "node5 parcourt node1 après son redémarrage"
fi

say "Succès : tous les scénarios sont passés"
