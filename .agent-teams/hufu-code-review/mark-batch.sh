#!/usr/bin/env bash
set -euo pipefail

batch_id="${1:-}"
if [[ ! "$batch_id" =~ ^batch-[0-9]{4}$ ]]; then
	echo "invalid batch id: expected batch-0000" >&2
	exit 2
fi
repo_root="$(git rev-parse --show-toplevel)"
coverage_root="$repo_root/workspace/hufu-code-review/coverage"
batch_file="$coverage_root/batches/$batch_id/paths.txt"
diff_file="$coverage_root/batches/$batch_id/diff.patch"
if [[ ! -s "$batch_file" || ! -s "$diff_file" ]]; then
	echo "batch evidence is missing for $batch_id" >&2
	exit 3
fi
mkdir -p "$coverage_root/reviewed"
sha="$(sha256sum "$diff_file" | awk '{print $1}')"
printf '%s\t%s\n' "$batch_id" "$sha" > "$coverage_root/reviewed/$batch_id.ok"
printf 'reviewed=%s\ndiff_sha256=%s\n' "$batch_id" "$sha"
