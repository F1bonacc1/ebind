#!/usr/bin/env bash
# Smoke-runs every example: each must exit 0 within its timeout. Examples are
# otherwise only compiled, so one that deadlocks or log.Fatals after an API
# change would go unnoticed.
#
# Usage: examples/run-all.sh [dir...]   (default: every examples/*/ directory)
# Env:   EXAMPLE_TIMEOUT (default 60s) per example; GO (default go).
set -euo pipefail

cd "$(dirname "$0")/.."
GO=${GO:-go}
EXAMPLE_TIMEOUT=${EXAMPLE_TIMEOUT:-60s}

if [ "$#" -gt 0 ]; then
	dirs=("$@")
else
	dirs=(examples/*/)
fi

bin_dir=$(mktemp -d)
trap 'rm -rf "$bin_dir"' EXIT

failed=()
for d in "${dirs[@]}"; do
	d=${d%/}
	name=$(basename "$d")
	# Build first so compile time does not count against the run timeout.
	if ! "$GO" build -o "$bin_dir/$name" "./$d"; then
		echo "FAIL $name (build)"
		failed+=("$name")
		continue
	fi
	start=$SECONDS
	log="$bin_dir/$name.log"
	if timeout "$EXAMPLE_TIMEOUT" "$bin_dir/$name" >"$log" 2>&1; then
		echo "ok   $name ($((SECONDS - start))s)"
	else
		rc=$?
		reason="exit $rc"
		[ "$rc" -eq 124 ] && reason="timeout after $EXAMPLE_TIMEOUT"
		echo "FAIL $name ($reason), last output:"
		tail -n 20 "$log" | sed 's/^/    /'
		failed+=("$name")
	fi
done

if [ "${#failed[@]}" -gt 0 ]; then
	echo "${#failed[@]} example(s) failed: ${failed[*]}"
	exit 1
fi
echo "all ${#dirs[@]} examples passed"
