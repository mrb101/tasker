#!/usr/bin/env bash
# Cut a release: move CHANGELOG.md's Unreleased section under the new version, commit, tag and push.
# The tag push runs .github/workflows/release.yml, which builds binaries and publishes the GitHub release.
#
#   scripts/release.sh 0.2.0
set -euo pipefail

v="${1:-}"
v="${v#v}"
if [[ ! "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo "usage: $0 X.Y.Z" >&2
  exit 2
fi
tag="v$v"
cd "$(git rev-parse --show-toplevel)"

[[ "$(git branch --show-current)" == main ]] || { echo "release from main" >&2; exit 1; }
[[ -z "$(git status --porcelain)" ]] || { echo "working tree is not clean" >&2; exit 1; }
git fetch --quiet --tags origin
[[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] || { echo "main differs from origin/main; pull or push first" >&2; exit 1; }
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then echo "$tag already exists" >&2; exit 1; fi

# The Unreleased section must say something.
body="$(awk '/^## \[Unreleased\]/{f=1; next} /^## \[/{f=0} f' CHANGELOG.md | grep -v '^[[:space:]]*$' || true)"
[[ -n "$body" ]] || { echo "CHANGELOG.md has nothing under [Unreleased]" >&2; exit 1; }

prev="$(git describe --tags --abbrev=0 2>/dev/null || true)"
today="$(date +%F)"
repo="https://github.com/mrb101/tasker"

awk -v v="$v" -v d="$today" -v prev="$prev" -v repo="$repo" '
  /^## \[Unreleased\]/ { print; print ""; print "## [" v "] - " d; next }
  /^\[Unreleased\]:/   {
    print "[Unreleased]: " repo "/compare/v" v "...HEAD"
    if (prev != "") print "[" v "]: " repo "/compare/" prev "...v" v
    else            print "[" v "]: " repo "/releases/tag/v" v
    next
  }
  { print }
' CHANGELOG.md > CHANGELOG.md.tmp
mv CHANGELOG.md.tmp CHANGELOG.md

go test ./...

git add CHANGELOG.md
git commit -m "Release $tag"
git tag -a "$tag" -m "$tag"
git push origin main "$tag"
echo "pushed $tag; the release workflow publishes it on GitHub"
