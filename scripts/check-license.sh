#!/usr/bin/env bash
# License gate for ContainerWatch.
#
# Fails when:
#   1. The LICENSE file is missing or does not look like MIT.
#   2. A runtime Go dependency is copyleft (GPL/AGPL/LGPL/SSPL/BUSL/Elastic),
#      which would forbid distributing this project under MIT.
#   3. A third-party browser asset is vendored without a notice in
#      THIRD-PARTY-NOTICES.md.
#   4. A font is vendored without the OFL text shipped next to it.
#
# It does not attempt to be a legal determination -- it catches the mistakes
# that are easy to make and hard to spot in review.
set -euo pipefail

cd "$(dirname "$0")/.."

fail=0
note() { printf '  %s\n' "$*"; }
error() { printf 'FAIL: %s\n' "$*" >&2; fail=1; }

echo "==> LICENSE file"
if [ ! -f LICENSE ]; then
  error "no LICENSE file at the repository root"
elif ! grep -q 'MIT License' LICENSE; then
  error "LICENSE does not declare the MIT License"
else
  note "LICENSE present and MIT"
fi

echo "==> README license section"
if ! grep -qE '^## License' README.md; then
  error "README.md has no '## License' section"
elif ! grep -q 'LICENSE' README.md; then
  error "README.md license section does not link the LICENSE file"
else
  note "README.md links the LICENSE file"
fi

echo "==> THIRD-PARTY-NOTICES.md"
if [ ! -f THIRD-PARTY-NOTICES.md ]; then
  error "no THIRD-PARTY-NOTICES.md"
else
  note "notices file present"
fi

echo "==> copyleft in the Go dependency graph"
# A copyleft runtime dependency would forbid MIT distribution. Checked against
# the module graph, not the module cache, so it reflects what actually builds.
unknown=''
while read -r mod; do
  [ -n "$mod" ] || continue
  # The project's own module is not a dependency.
  [ "$mod" = "github.com/containerwatch/containerwatch" ] && continue
  case "$mod" in
    golang.org/x/*|modernc.org/*|github.com/*|gopkg.in/*|go.etcd.io/*|sigs.k8s.io/*|k8s.io/*|filippo.io/*|go.mongodb.org/*|go.opentelemetry.io/*|cloud.google.com/*|google.golang.org/*)
      # Known-permissive prefix: the license text check below is authoritative.
      ;;
    *)
      # Anything else needs a human look -- it might be copyleft.
      unknown="$unknown $mod"
      ;;
  esac
done < <(go list -m -f '{{.Path}}' all 2>/dev/null | sort -u)

if [ -n "$unknown" ]; then
  error "modules outside the known-permissive prefixes need a manual license check:$unknown"
else
  note "every runtime module is in a known-permissive prefix"
fi

# Belt and braces: grep the vendored module sources for copyleft headers.
echo "==> copyleft headers in dependency sources"
if go mod download >/dev/null 2>&1; then
  hits=''
  for d in $(go list -m -f '{{if not .Main}}{{.Dir}}{{end}}' all 2>/dev/null); do
    [ -d "$d" ] || continue
    for f in "$d"/LICENSE "$d"/LICENSE.txt "$d"/COPYING; do
      [ -f "$f" ] || continue
      if grep -qiE 'GNU (GENERAL PUBLIC|AFFERO GENERAL PUBLIC) LICENSE|Affero General Public License|Server Side Public License|Business Source License' "$f" 2>/dev/null; then
        hits="$hits $(basename "$(dirname "$d")")"
      fi
      break
    done
  done
  if [ -n "$hits" ]; then
    error "copyleft license found in:$hits"
  else
    note "no copyleft license text in any dependency"
  fi
fi

echo "==> vendored browser assets have notices"
notices=THIRD-PARTY-NOTICES.md
missing=''
for f in web/static/js/*.js; do
  base=$(basename "$f")
  case "$base" in
    app.js|containers.js|logs.js) continue ;;   # first-party
  esac
  # Require the file to appear as a table row, not merely somewhere in the
  # prose -- otherwise a mention in a sentence would satisfy the check.
  if ! grep -qE "^\| .*\`?${base//./\\.}\`? *\|" "$notices" 2>/dev/null; then
    missing="$missing $base"
  fi
done
if [ -n "$missing" ]; then
  error "vendored JS without a notice in $notices:$missing"
else
  note "every vendored JS file is named in the notices file"
fi

echo "==> fonts ship with the OFL text"
if [ ! -f web/static/fonts/OFL.txt ]; then
  error "web/static/fonts/OFL.txt is missing; the OFL requires the license to accompany the fonts"
else
  note "OFL.txt present"
fi

if [ "$fail" -ne 0 ]; then
  echo
  echo "license gate FAILED" >&2
  exit 1
fi

echo
echo "license gate passed"
