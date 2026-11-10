#!/bin/sh
# Vendors the Starbase components this app uses, one file each (Starbase's
# cmd/dist writes the same bytes starbase.zweiundeins.gmbh serves), from a
# Starbase checkout at a given ref. Datastar comes from lith
# (template.Datastar()): take the components from a Starbase commit whose
# static/vendor/datastar-rocket.js is that same build, or the two copies of
# Rocket break each other (TestStarbaseMatchesDatastar).
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
git -C "$src" show "$commit:LICENSE" > "$dst/starbase/LICENSE"
datastar=$(git -C "$src" show "$commit:static/vendor/datastar-rocket.js" | sha256sum | cut -d' ' -f1)
{
	cat <<EOT
Starbase components, one file each (Starbase's cmd/dist), from
github.com/zweiundeins/starbase at commit $commit.
Datastar is lith's (template.Datastar()); this commit's
static/vendor/datastar-rocket.js, sha256 $datastar,
must be the same build (TestStarbaseMatchesDatastar).
Update: task starbase REF=<ref>.
The PD rockets components (kanban-board, inline-edit) are Beer-Ware (their *.LICENSE-pd-rockets.txt); the rest is MIT (starbase/LICENSE).

Files and their integrity, as cmd/dist printed them:
EOT
	sed "s#$tmp/dist/##" "$tmp/dist.txt"
} > "$dst/VERSION"
echo "vendored Starbase $commit"
