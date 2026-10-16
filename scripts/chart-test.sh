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
  helm lint --strict "$chart" -f "$values" >/dev/null
  out=$(helm template opsward "$chart" --namespace opsward -f "$values")
  printf '%s\n' "$out" | kubeconform -strict -summary -output text \
    -schema-location default -kubernetes-version 1.31.0 - >/dev/null
  # The configuration the chart writes must be one Opsward accepts.
  tmp=$(mktemp -d)
  helm template opsward "$chart" --namespace opsward -f "$values" --show-only templates/configmap.yaml |
    sed -n '/^  opsward.yaml: |$/,$p' | tail -n +2 | sed 's/^    //' >"$tmp/opsward.yaml"
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
exit $fail
