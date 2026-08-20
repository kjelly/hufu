#!/usr/bin/env bash
set -euo pipefail

# Materialise a bounded review plan. The model receives one batch at a time,
# never the repository-wide path list.
since_spec="${1:-2.days.ago}"
repo_root="$(git rev-parse --show-toplevel)"
review_root="$repo_root/workspace/hufu-code-review/coverage"
batch_root="$review_root/batches"
reviewed_root="$review_root/reviewed"

# A file-count-only partition is not a bounded review unit: one generated or
# heavily edited source file can make a four-file diff exceed the model/tool
# context even though the path count is small.  Pack complete zero-context
# hunks by bytes and never split a hunk.  A single unusually large hunk is
# retained as one unit (and is still below view's 512 KiB artifact ceiling),
# so no changed line silently disappears from the review plan.
max_diff_bytes="${HUFU_REVIEW_MAX_DIFF_BYTES:-24000}"
if ! [[ "$max_diff_bytes" =~ ^[1-9][0-9]*$ ]]; then
	echo "HUFU_REVIEW_MAX_DIFF_BYTES must be a positive integer" >&2
	exit 2
fi
max_diff_lines="${HUFU_REVIEW_MAX_DIFF_LINES:-600}"
if ! [[ "$max_diff_lines" =~ ^[1-9][0-9]*$ ]]; then
	echo "HUFU_REVIEW_MAX_DIFF_LINES must be a positive integer" >&2
	exit 2
fi
max_paths_per_batch="${HUFU_REVIEW_MAX_PATHS:-16}"
if ! [[ "$max_paths_per_batch" =~ ^[1-9][0-9]*$ ]]; then
	echo "HUFU_REVIEW_MAX_PATHS must be a positive integer" >&2
	exit 2
fi

rm -rf "$batch_root" "$reviewed_root"
mkdir -p "$batch_root" "$reviewed_root"

end_sha="$(git rev-parse HEAD)"
commit_count="$(git rev-list --count --since="$since_spec" HEAD)"
start_sha=""
if [[ "$commit_count" -gt 0 ]]; then
	oldest_sha="$(git log --since="$since_spec" --format=%H --skip=$((commit_count - 1)) -n 1)"
	if [[ -z "$oldest_sha" ]]; then
		echo "unable to resolve oldest commit for since=$since_spec" >&2
		exit 2
	fi
	start_sha="$(git rev-parse "$oldest_sha^")"
	checked_count="$(git rev-list --count "$start_sha..$end_sha")"
	if [[ "$checked_count" != "$commit_count" ]]; then
		echo "commit range check failed: expected $commit_count, got $checked_count" >&2
		exit 3
	fi
else
	checked_count=0
fi

paths_file="$review_root/changed-paths.txt"
: > "$paths_file"
if [[ "$commit_count" -gt 0 ]]; then
	git diff --no-renames --name-only "$start_sha..$end_sha" | sort -u > "$paths_file"
fi

changed_files="$(awk 'NF { count++ } END { print count + 0 }' "$paths_file")"
shortstat=0
if [[ "$commit_count" -gt 0 ]]; then
	shortstat="$(git diff --no-renames --shortstat "$start_sha..$end_sha" | sed -n 's/^[[:space:]]*\([0-9][0-9]*\) files changed.*/\1/p')"
	shortstat="${shortstat:-0}"
fi
if [[ "$changed_files" != "$shortstat" ]]; then
	echo "changed-file count check failed: name-only=$changed_files shortstat=$shortstat" >&2
	exit 4
fi

# Each audit task receives four direct, ordinary filesystem paths. They are
# deliberately not runtime artifact references: `view.artifact_ref` accepts
# only opaque IDs issued by Hufu, while these are team-owned workspace files.
printf '#batch_id\tfile_count\tdiff_path\tsource_context_path\tcaller_context_path\ttest_context_path\tstatus\n' > "$review_root/batches.tsv"
batch_count=0
if [[ "$changed_files" -gt 0 ]]; then
	tmp_root="$(mktemp -d "$review_root/.partition.XXXXXX")"
	trap 'rm -rf "$tmp_root"' EXIT
	current_batch=""
	current_diff=""
	current_paths=""
	current_bytes=0
	current_lines=0
	current_files=0
	current_path_count=0
	declare -A current_path_seen=()

	truncate_context() {
		local path="$1"
		local max_bytes=400000
		local bytes
		bytes="$(wc -c < "$path" | tr -d ' ')"
		if [[ "$bytes" -gt "$max_bytes" ]]; then
			head -c "$max_bytes" "$path" > "$path.tmp"
			printf '\n[context truncated at %s bytes; record this as a coverage gap]\n' "$max_bytes" >> "$path.tmp"
			mv "$path.tmp" "$path"
		fi
	}

	write_context_files() {
		local batch_dir="$1"
		local paths="$batch_dir/paths.txt"
		local source="$batch_dir/source-context.txt"
		local callers="$batch_dir/caller-context.txt"
		local tests="$batch_dir/test-context.txt"
		local path symbol test dir

		: > "$source"
		while IFS= read -r path; do
			[[ -n "$path" ]] || continue
			printf '=== CURRENT SOURCE: %s ===\n' "$path" >> "$source"
			if git cat-file -e "HEAD:$path" 2>/dev/null; then
				git show "HEAD:$path" | sed -n '1,180p' >> "$source"
			else
				printf '[path is deleted or unavailable at HEAD]\n' >> "$source"
			fi
			printf '\n' >> "$source"
		done < "$paths"
		truncate_context "$source"

		{
			printf '# Caller/callee search context for this batch\n'
			printf '# Matches are candidates; absence is explicit evidence, not a clean pass.\n\n'
			awk '/^\\+func[[:space:]]+/ { line=$0; sub(/^\\+func[[:space:]]+/, "", line); sub(/^\\([^)]*\\)[[:space:]]*/, "", line); split(line, parts, /[[:space:](]/); if (parts[1] != "") print parts[1] }' "$batch_dir/diff.patch" | sort -u | while IFS= read -r symbol; do
				printf '=== CALLER CANDIDATES: %s ===\n' "$symbol"
				git grep -n -w -- "$symbol" || true
				printf '\n'
			done
		} > "$callers"
		if [[ ! -s "$callers" ]] || [[ "$(wc -l < "$callers" | tr -d ' ')" -le 2 ]]; then
			printf '[No statically identifiable changed Go function symbol; caller/callee evidence is unavailable.]\n' >> "$callers"
		fi
		truncate_context "$callers"

		: > "$tests"
		printf '# Focused current test context for directories changed in this batch\n\n' >> "$tests"
		declare -A seen_tests=()
		while IFS= read -r path; do
			[[ -n "$path" ]] || continue
			dir="${path%/*}"
			[[ "$dir" == "$path" ]] && dir="."
			while IFS= read -r test; do
				[[ -n "$test" && -z "${seen_tests[$test]+present}" ]] || continue
				seen_tests["$test"]=1
				printf '=== FOCUSED TEST: %s ===\n' "$test" >> "$tests"
				git show "HEAD:$test" | sed -n '1,220p' >> "$tests"
				printf '\n' >> "$tests"
			done < <(git ls-files '*_test.go' | awk -v dir="$dir" 'index($0, dir "/") == 1')
		done < "$paths"
		if [[ "$(wc -l < "$tests" | tr -d ' ')" -le 2 ]]; then
			printf '[No focused tracked test file is available for this batch.]\n' >> "$tests"
		fi
		truncate_context "$tests"
	}

	flush_batch() {
		[[ -n "$current_batch" ]] || return 0
		mkdir -p "$batch_root/$current_batch"
		mv "$current_diff" "$batch_root/$current_batch/diff.patch"
		awk '!seen[$0]++' "$current_paths" > "$batch_root/$current_batch/paths.txt"
		rm -f "$current_paths"
		write_context_files "$batch_root/$current_batch"
		printf '%s\t%s\t%s\t%s\t%s\t%s\tunreviewed\n' \
			"$current_batch" "$current_files" "$batch_root/$current_batch/diff.patch" \
			"$batch_root/$current_batch/source-context.txt" \
			"$batch_root/$current_batch/caller-context.txt" \
			"$batch_root/$current_batch/test-context.txt" >> "$review_root/batches.tsv"
		batch_count=$((batch_count + 1))
		current_batch=""
		current_diff=""
		current_paths=""
		current_bytes=0
		current_lines=0
		current_files=0
		current_path_count=0
		current_path_seen=()
	}

	start_batch() {
		current_batch="batch-$(printf '%04d' "$batch_count")"
		current_diff="$tmp_root/$current_batch.diff"
		current_paths="$tmp_root/$current_batch.paths"
		: > "$current_diff"
		: > "$current_paths"
		current_bytes=0
		current_lines=0
		current_files=0
		current_path_count=0
		current_path_seen=()
	}

	append_unit() {
		local unit="$1"
		local path="$2"
		local unit_bytes
		local unit_lines
		local new_path=0
		unit_bytes="$(wc -c < "$unit" | tr -d ' ')"
		unit_lines="$(wc -l < "$unit" | tr -d ' ')"
		if [[ -z "${current_path_seen[$path]+present}" ]]; then
			new_path=1
		fi
		if [[ -z "$current_batch" ]]; then
			start_batch
		elif [[ "$current_bytes" -gt 0 && ( $((current_bytes + unit_bytes)) -gt "$max_diff_bytes" || $((current_lines + unit_lines)) -gt "$max_diff_lines" || ( "$new_path" -eq 1 && "$current_path_count" -ge "$max_paths_per_batch" ) ) ]]; then
			flush_batch
			start_batch
			new_path=1
		fi
		cat "$unit" >> "$current_diff"
		printf '%s\n' "$path" >> "$current_paths"
		current_bytes=$((current_bytes + unit_bytes))
		current_lines=$((current_lines + unit_lines))
		if [[ "$new_path" -eq 1 ]]; then
			current_path_seen["$path"]=1
			current_path_count=$((current_path_count + 1))
			current_files=$((current_files + 1))
		fi
	}

	while IFS= read -r path; do
		[[ -n "$path" ]] || continue
		diff_file="$tmp_root/current.diff"
		git diff --no-renames --unified=0 "$start_sha..$end_sha" -- "$path" > "$diff_file"
		if [[ ! -s "$diff_file" ]]; then
			# Keep an explicit empty unit for a path whose diff disappeared
			# between inventory and materialisation; the count check above still
			# makes this visible to the verifier instead of dropping the path.
			printf 'diff --git a/%s b/%s\n' "$path" "$path" > "$diff_file"
		fi

		# Split only at hunk boundaries.  Every emitted unit has the complete
		# file header plus one or more whole hunks, so a reviewer never receives
		# an ambiguous fragment and the union of units covers every hunk.
		unit_dir="$tmp_root/units"
		mkdir -p "$unit_dir"
		unit_prefix="$unit_dir/$(printf '%s' "$path" | tr '/:' '__')"
		awk -v dir="$unit_dir" -v prefix="$unit_prefix" '
			function close_unit() {
				if (out != "") {
					close(out)
				}
			}
			function open_unit() {
				idx++
				out = sprintf("%s-%04d", prefix, idx)
				printf "%s", header > out
			}
			BEGIN { header=""; out=""; idx=0; saw_hunk=0 }
			/^@@ / {
				close_unit()
				open_unit()
				printf "%s\n", $0 > out
				saw_hunk=1
				next
			}
			{
				if (!saw_hunk) {
					header = header $0 "\n"
				} else {
					print $0 > out
				}
			}
			END {
				close_unit()
				if (!saw_hunk) {
					idx++
					out = sprintf("%s-%04d", prefix, idx)
					printf "%s", header > out
					close(out)
				}
			}' "$diff_file"
		for unit in "$unit_dir"/$(basename "$unit_prefix")-[0-9][0-9][0-9][0-9]; do
			[[ -f "$unit" ]] || continue
			append_unit "$unit" "$path"
		done
	done < "$paths_file"
	flush_batch
	# Derive path-to-batch rows from the durable, de-duplicated unit manifests.
	: > "$review_root/review-manifest.rows"
	for batch in "$batch_root"/batch-[0-9][0-9][0-9][0-9]; do
		[[ -d "$batch" ]] || continue
		batch_id="$(basename "$batch")"
		while IFS= read -r path; do
			[[ -n "$path" ]] || continue
			printf '%s\t%s\tunreviewed\n' "$path" "$batch_id" >> "$review_root/review-manifest.rows"
		done < "$batch/paths.txt"
	done
	{
		printf 'range_start\t%s\n' "$start_sha"
		printf 'range_end\t%s\n' "$end_sha"
		printf 'since\t%s\n' "$since_spec"
		printf 'commit_count\t%s\n' "$commit_count"
		printf 'changed_files\t%s\n' "$changed_files"
		printf 'batch_count\t%s\n' "$batch_count"
		printf 'batch_size\t%s\n' "$max_diff_bytes"
		printf 'batch_max_lines\t%s\n' "$max_diff_lines"
		printf 'batch_max_paths\t%s\n' "$max_paths_per_batch"
		printf 'path\tbatch\tstatus\n'
		cat "$review_root/review-manifest.rows"
	} > "$review_root/review-manifest.tsv"
fi

manifest="$review_root/review-manifest.tsv"
if [[ "$changed_files" -eq 0 ]]; then
	: > "$review_root/review-manifest.rows"
fi
{
	printf 'range_start\t%s\n' "$start_sha"
	printf 'range_end\t%s\n' "$end_sha"
	printf 'since\t%s\n' "$since_spec"
	printf 'commit_count\t%s\n' "$commit_count"
	printf 'changed_files\t%s\n' "$changed_files"
	printf 'batch_count\t%s\n' "$batch_count"
	printf 'batch_size\t%s\n' "$max_diff_bytes"
	printf 'batch_max_lines\t%s\n' "$max_diff_lines"
	printf 'batch_max_paths\t%s\n' "$max_paths_per_batch"
	printf 'path\tbatch\tstatus\n'
	cat "$review_root/review-manifest.rows"
} > "$manifest"

printf 'manifest=%s\nrange=%s..%s\ncommit_count=%s\nchanged_files=%s\nbatches=%s\nbatch_size=%s\n' \
	"$manifest" "${start_sha:-empty}" "$end_sha" "$commit_count" "$changed_files" "$batch_count" "$max_diff_bytes"
