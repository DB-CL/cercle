#!/bin/sh
# Regenerates THIRD_PARTY_LICENSES.md: full license text of every module
# linked into the binary (required to distribute the image).
#   deploy/licenses.sh
set -e
cd "$(dirname "$0")/.."
out=THIRD_PARTY_LICENSES.md
mods=$(deploy/go.sh list -deps -f '{{with .Module}}{{.Path}} {{.Version}} {{.Dir}}{{end}}' ./cmd/cercle | sort -u | grep -v '^github.com/DB-CL/cercle ')
{
  echo "# Third-party licenses"
  echo
  echo "The \`cercle\` binary and the Docker image include the following modules. Their licenses are reproduced below, as they require. The Go compiler and standard library are under the BSD license (https://go.dev/LICENSE)."
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
echo "$out: $(echo "$mods" | wc -l) modules"
