#!/usr/bin/env bash
# Lint and render charts/opsward with each value set in ci/ (validated by kubeconform, and
# its configuration by opsward validate),
# check what each renders against the assertions in tests/<set>.expect, and check that
# each tests/fail-*.yaml is refused with the message on its first line ("# expect: ...").
set -euo pipefail
chart=charts/opsward
cd "$(dirname "$0")/.."
fail=0
for values in "$chart"/ci/*.yaml; do
  name=$(basename "$values" .yaml)
  # Lint reports a template's fail only as INFO: its output is checked too.
  if ! lint=$(helm lint --strict "$chart" --namespace opsward -f "$values" 2>&1) || grep -q "Fail:" <<<"$lint"; then
    echo "$name: helm lint:"; echo "$lint"; fail=1; continue
  fi
  out=$(helm template opsward "$chart" --namespace opsward -f "$values")
  if ! kc=$(printf '%s\n' "$out" | kubeconform -strict -summary -output text \
    -schema-location default -kubernetes-version 1.31.0 - 2>&1); then
    echo "$name: kubeconform:"; echo "$kc"; fail=1
  fi
  # The configuration the chart writes must be one Opsward accepts.
  tmp=$(mktemp -d)
  helm template opsward "$chart" --namespace opsward -f "$values" --show-only templates/configmap.yaml |
    awk '/^  opsward.yaml: \|$/ {on=1; next} on && /^  [^ ]/ {on=0} on {sub(/^    /, ""); print}' >"$tmp/opsward.yaml"
  if ! go run ./cmd/opsward validate --config "$tmp/opsward.yaml" >"$tmp/out" 2>&1; then
    echo "$name: the rendered configuration is not valid:"; cat "$tmp/out"; fail=1
  fi
  rm -rf "$tmp"
  expect="$chart/tests/$name.expect"
  [[ -f $expect ]] || continue
  # Each line of an .expect file is a fixed string the output must contain, or, after
  # "! ", one it must not.
  while IFS= read -r line; do
    [[ -z $line || $line == \#* ]] && continue
    if [[ $line == '! '* ]]; then
      if grep -qF -- "${line#! }" <<<"$out"; then echo "$name: renders '${line#! }'"; fail=1; fi
    elif ! grep -qF -- "$line" <<<"$out"; then
      echo "$name: does not render '$line'"; fail=1
    fi
  done <"$expect"
done
for values in "$chart"/tests/fail-*.yaml; do
  want=$(head -1 "$values" | sed 's/^# expect: //')
  if out=$(helm template opsward "$chart" --namespace opsward -f "$values" 2>&1); then
    echo "$(basename "$values"): rendered; want it refused with '$want'"; fail=1
  elif ! grep -qF -- "$want" <<<"$out"; then
    echo "$(basename "$values"): refused with '$out'; want '$want'"; fail=1
  fi
done
# A policy edit reloads in place; an edit to a section that needs a restart rolls the pod.
restart_hash() {
  helm template opsward "$chart" --namespace opsward -f "$chart/ci/default.yaml" "$@" --show-only templates/deployment.yaml |
    grep 'checksum/restart:'
}
base=$(restart_hash)
if [[ $(restart_hash --set 'config.policy.roles.viewer.risk[0]=write') != "$base" ]]; then
  echo "a policy edit changes checksum/restart: the pod would restart for a reloadable change"; fail=1
fi
if [[ $(restart_hash --set 'config.identity.groups.team-a[0]=bob@example.com') != "$base" ]]; then
  echo "an internal groups edit changes checksum/restart"; fail=1
fi
if [[ $(restart_hash --set 'config.audit.timeout=5s') == "$base" ]]; then
  echo "an audit edit leaves checksum/restart: the pod would keep the old audit settings"; fail=1
fi
exit $fail
