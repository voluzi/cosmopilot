#!/usr/bin/env bash
# Usage: is-newest-release.sh <tag prefix> <version>
#
# Prints "true" when <version> is the highest stable release among the tags "<prefix>v<X.Y.Z>",
# and "false" otherwise. Prereleases are never candidates, so neither they nor a backport to an
# older line move `latest`.
# The cosmopilot tags have an empty prefix ("v3.2.0"), the component ones a path prefix
# ("node-utils/v3.0.0").
set -euo pipefail

prefix="$1"
version="$2"

newest=""
while IFS= read -r tag; do
  candidate="${tag#"${prefix}v"}"
  [[ "$candidate" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || continue
  if [ -z "$newest" ] || [ "$(printf '%s\n%s\n' "$newest" "$candidate" | sort -V | tail -n1)" = "$candidate" ]; then
    newest="$candidate"
  fi
done < <(git tag -l "${prefix}v[0-9]*")

if [ "$version" = "$newest" ]; then
  echo true
else
  echo false
fi
