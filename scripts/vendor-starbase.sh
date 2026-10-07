#!/bin/sh
# Vendors the Starbase components this app uses, one file each (Starbase's
# cmd/dist writes the same bytes starbase.zweiundeins.gmbh serves), and
# Starbase's patched Datastar build, from a Starbase checkout at a given ref.
#   scripts/vendor-starbase.sh ../starbase origin/main
set -eu
src=${1:-../starbase}
ref=${2:-origin/main}
dst=web/static/vendor
components="kanban-board inline-edit modal toast"
commit=$(git -C "$src" rev-parse "$ref")
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
git clone -q --no-checkout "$src" "$tmp/starbase"
git -C "$tmp/starbase" checkout -q "$commit"
(cd "$tmp/starbase" && go run ./cmd/dist -out "$tmp/dist" $components) > "$tmp/dist.txt"
rm -rf "$dst"
mkdir -p "$dst/starbase"
cp -R "$tmp/dist/." "$dst/starbase/"
for c in $components; do
	if git -C "$src" cat-file -e "$commit:components/$c/LICENSE-pd-rockets.txt" 2>/dev/null; then
		git -C "$src" show "$commit:components/$c/LICENSE-pd-rockets.txt" > "$dst/starbase/$c.LICENSE-pd-rockets.txt"
	fi
done
git -C "$src" show "$commit:static/vendor/datastar-rocket.js" > "$dst/datastar-rocket.js"
git -C "$src" show "$commit:LICENSE" > "$dst/starbase/LICENSE"
{
	cat <<EOT
Starbase components, one file each (Starbase's cmd/dist), and its patched Datastar build
(Datastar v1.0.4 with patches/rocket), from github.com/zweiundeins/starbase at commit $commit.
Update: task starbase REF=<ref>.
The PD rockets components (kanban-board, inline-edit) are Beer-Ware (their *.LICENSE-pd-rockets.txt); the rest is MIT (starbase/LICENSE).
Datastar: MIT, github.com/starfederation/datastar.

Files and their integrity, as cmd/dist printed them:
EOT
	sed "s#$tmp/dist/##" "$tmp/dist.txt"
} > "$dst/VERSION"
echo "vendored Starbase $commit"
