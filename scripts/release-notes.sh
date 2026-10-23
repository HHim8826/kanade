#!/usr/bin/env bash
# release-notes.sh TAG: the notes of the release TAG in Markdown, from the commits since the version
# tag before it (review #260): each commit's title with the issues it fixes, newest first, then the
# whole comparison. Run in a clone that has the tags (actions/checkout with fetch-depth: 0).
set -euo pipefail

tag="${1:?usage: release-notes.sh TAG}"
repo="${GITHUB_REPOSITORY:-HHim8826/kanade}"
prev=$(git describe --tags --abbrev=0 --match 'v*' "$tag^" 2>/dev/null || true)
range="$tag"
if [ -n "$prev" ]; then
  range="$prev..$tag"
fi

git log --no-merges --format='%H' "$range" | while read -r c; do
  subject=$(git log -1 --format='%s' "$c")
  fixes=$(git log -1 --format='%b' "$c" | sed -nE 's/^(Fixes|Closes|Resolves) #([0-9]+).*/#\2/ip' | paste -sd, - | sed 's/,/, /g')
  printf -- '- %s%s\n' "$subject" "${fixes:+ ($fixes)}"
done
if [ -n "$prev" ]; then
  printf '\n**Full Changelog**: https://github.com/%s/compare/%s...%s\n' "$repo" "$prev" "$tag"
fi
