#!/usr/bin/env bash
# Scans every local commit that the remote does not have yet, on all branches and tags.
#
# Not the pushed range: pre-commit hands a hook only the first ref of a push and leaves
# PRE_COMMIT_TO_REF unset on a first push, so a range built from it skips refs or fails.
# The superset can block a push over a secret on another unpushed branch; that is intended.
#
# The range is resolved by git under `set -e` before gitleaks sees it, because gitleaks
# reports a git error in --log-opts as "0 commits scanned" and exits 0.
set -euo pipefail

remote="${PRE_COMMIT_REMOTE_NAME:?must run as a pre-commit pre-push hook}"
range=(--branches --tags --not "--remotes=$remote")

count=$(git rev-list --count "${range[@]}")
if [[ "$count" -eq 0 ]]; then
  exit 0
fi

echo "gitleaks: scanning $count commit(s) not on '$remote'"
exec mise exec -- gitleaks git --redact --no-banner --log-opts="${range[*]}"
