#!/usr/bin/env bash
# Prints the CHANGELOG.md section for one version, without its heading.
# Usage: scripts/release-notes.sh 0.1.0
set -euo pipefail

version=${1:?usage: $0 <version>}
notes=$(awk -v v="$version" '
  /^## \[/ { if (found) exit; found = index($0, "## [" v "]") == 1; next }
  /^\[[^]]+\]: / { next } # link reference definitions
  found
' CHANGELOG.md | sed '/./,$!d') # $(…) drops the trailing blank lines

if [[ -z ${notes//[[:space:]]/} ]]; then
  echo "CHANGELOG.md has no section for $version" >&2
  exit 1
fi
printf '%s\n' "$notes"
