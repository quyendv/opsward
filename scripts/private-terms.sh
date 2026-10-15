#!/usr/bin/env bash
# Keeps maintainer-listed private terms (organization names, hostnames, addresses,
# personal email) out of a public repository: file content, commit messages and
# commit identities.
#
# The terms are not in this repository. They live in .local/private-terms.txt, which is
# git-ignored: one Perl-compatible regex per line, matched case-insensitively; blank
# lines and lines starting with # are ignored.
#
# Without that file the check passes, so contributors need nothing. A maintainer turns
# a missing or empty file into an error with:
#   git config opsward.privateTermsRequired true
#
# Usage: private-terms.sh staged | message FILE | push | history
set -euo pipefail

root=$(git rev-parse --show-toplevel)
list="$root/.local/private-terms.txt"

pattern=""
if [[ -f "$list" ]]; then
  pattern=$(grep -vE '^[[:space:]]*(#|$)' "$list" | paste -sd'|' || true)
fi
if [[ -z "$pattern" ]]; then
  if [[ "$(git config --bool opsward.privateTermsRequired || true)" == "true" ]]; then
    echo "private-terms: $list is missing or empty, but opsward.privateTermsRequired is set" >&2
    exit 1
  fi
  exit 0
fi

# grep exits 1 for "no match" and 2 for an invalid pattern; scan treats 1 as clean, so an
# invalid pattern must be caught here or it would pass everything.
rc=0
grep -qP -- "(?:$pattern)" </dev/null || rc=$?
if [[ $rc -eq 2 ]]; then
  echo "private-terms: $list contains an invalid regex" >&2
  exit 1
fi

# scan WHAT: reads text on stdin, prints matching lines, fails if there are any.
scan() {
  local hits
  hits=$(grep -inP -- "(?:$pattern)" | cut -c1-200 || true)
  if [[ -n "$hits" ]]; then
    echo "private-terms: $1 contains a listed term:" >&2
    echo "$hits" >&2
    return 1
  fi
}

# commits REV...: scans the identities, messages and added lines of those commits.
commits() {
  git log --format='commit %H%nAuthor: %an <%ae>%nCommitter: %cn <%ce>%n%n%B' "$@" | scan "commit metadata"
  git log -p --no-color --format='' "$@" | { grep -E '^\+' || true; } | scan "added lines"
}

case "${1:-}" in
  staged)
    git var GIT_AUTHOR_IDENT | scan "author identity"
    git var GIT_COMMITTER_IDENT | scan "committer identity"
    git diff --cached --no-color -U0 | { grep -E '^\+' || true; } | scan "staged changes"
    ;;
  message)
    { grep -v '^#' "${2:?message file required}" || true; } | scan "commit message"
    ;;
  push)
    # Every local commit the remote does not have, as in gitleaks-pre-push.sh.
    commits --branches --tags --not "--remotes=${PRE_COMMIT_REMOTE_NAME:?must run as a pre-commit pre-push hook}"
    ;;
  history)
    commits --all
    ;;
  *)
    echo "usage: $0 staged | message FILE | push | history" >&2
    exit 2
    ;;
esac
