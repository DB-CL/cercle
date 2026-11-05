#!/bin/sh
# Régénère THIRD_PARTY_LICENSES.md : texte complet de la licence de chaque
# module lié au binaire (obligatoire pour distribuer l'image).
#   deploy/licenses.sh
set -e
cd "$(dirname "$0")/.."
out=THIRD_PARTY_LICENSES.md
mods=$(deploy/go.sh list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' ./cmd/cercle | sort -u | grep -v '^github.com/DB-CL/cercle ')
{
  echo "# Licences des composants tiers"
  echo
  echo "Le binaire \`cercle\` et l'image Docker incluent les modules suivants. Leurs licences sont reproduites ci-dessous, comme elles l'exigent. Le compilateur et la bibliothèque standard de Go sont sous licence BSD (https://go.dev/LICENSE)."
  echo
  echo "$mods" | while read -r path version dir; do echo "- \`$path\` $version"; done
  echo "$mods" | while read -r path version dir; do
    host_dir=$(echo "$dir" | sed "s|^/cache/mod|$HOME/.cache/cercle-go/mod|")
    echo
    echo "## $path"
    echo
    for f in "$host_dir"/LICENSE* "$host_dir"/COPYING* "$host_dir"/NOTICE*; do
      [ -f "$f" ] || continue
      echo '```'
      cat "$f"
      echo '```'
    done
  done
} > "$out"
echo "$out : $(echo "$mods" | wc -l) modules"
