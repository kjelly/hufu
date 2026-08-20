#!/usr/bin/env bash
set -euo pipefail

repo_root="$(git rev-parse --show-toplevel)"
coverage_root="$repo_root/workspace/hufu-code-review/coverage"
manifest="$coverage_root/review-manifest.tsv"
batches="$coverage_root/batches.tsv"
reviewed="$coverage_root/reviewed"
if [[ ! -s "$manifest" || ! -s "$batches" ]]; then
	echo "review manifest is missing; inventory must run first" >&2
	exit 2
fi
expected_files="$(awk -F '\t' '$1 == "changed_files" { print $2 }' "$manifest")"
expected_batches="$(awk -F '\t' '$1 == "batch_count" { print $2 }' "$manifest")"
actual_files="$(awk -F '\t' '
 $1 == "path" { next }
 NF == 3 { seen[$1] = 1 }
 END { for (path in seen) count++; print count + 0 }
' "$manifest")"
if [[ "$actual_files" != "$expected_files" ]]; then
	echo "manifest path count mismatch: expected $expected_files, got $actual_files" >&2
	exit 3
fi

# Read-only reviewers return their complete Markdown handoff as final text.
# Promote only a handoff from this invocation, identified by the latest run ID
# in the append-only task journal, and only when it contains every required
# evidence section. A stale transcript from an older session must never
# satisfy the current coverage gate.
task_journal="$repo_root/workspace/hufu-code-review/logs/task_journal.jsonl"
task_output_dir="$repo_root/workspace/hufu-code-review/logs/task-output"
# The range is no longer typed into the dispatch prompt or matched as free
# text: reviewers view review-manifest.tsv directly for range_start/range_end,
# the same evidence-ledger check used for diff/source/caller/test (see
# evidence_present below). That removes the one recurring failure mode where
# the coordinator's own retyping of a 40-hex SHA pair into many similar batch
# dispatches dropped or merged a character, which every reviewer in the run
# then faithfully reproduced. The run_id-scoped file glob below remains the
# actual staleness guard.
run_id=""
if [[ -s "$task_journal" ]]; then
	run_id="$(grep -o '"run_id":"run-[^"]*"' "$task_journal" | tail -1 | cut -d '"' -f4)"
fi
if [[ -z "$run_id" ]]; then
	echo "current run id is missing from task journal" >&2
	exit 4
fi

# candidate_satisfies_batch checks ONE task-output file against every
# requirement for ONE batch: coarse markers (status/range/section prose) plus
# the precise per-evidence-file view-call ledger. A bare `grep -Fq batch_id`
# only means the string appears somewhere in the file — a different batch's
# reviewer can mention another batch_id in passing (e.g. while discussing
# related context) and still pass that coarse check. Keeping the full
# gauntlet in one function lets the caller try the NEXT most-recent candidate
# instead of committing to (and failing on) the first coarse match.
candidate_satisfies_batch() {
	local path="$1" batch_id="$2" manifest_file="$3" diff_file="$4" source_file="$5" caller_file="$6" test_file="$7"
	# The task-output transcript is NDJSON: each record's "input" field holds
	# the model's raw tool-call-argument text as a JSON *string*, so every
	# quote inside that argument JSON (status, details, ...) is itself
	# backslash-escaped once by the outer encoder. Match that escaped form —
	# an unescaped '"status":"..."' pattern can never occur in this file and
	# would silently fail every batch regardless of review completeness.
	# Section headings are free markdown prose the reviewer writes at its own
	# nesting depth (some batches use "## Findings", others "### Findings");
	# accept either depth since only the section's presence is evidentiary,
	# not its heading level.
	grep -Fq '"tool":"submit_result"' "$path" \
		&& grep -Eq '\\"status\\":\\"(success|completed_with_gaps)\\"' "$path" \
		&& grep -Fq "$batch_id" "$path" \
		&& grep -Fq '\"details\"' "$path" \
		&& grep -Fq '## Review scope' "$path" \
		&& grep -Eq '#{2,3} Findings' "$path" \
		&& grep -Eq '#{2,3} Tests and evidence' "$path" \
		&& grep -Eq '#{2,3} Coverage gaps' "$path" \
		&& grep -Fqi 'untruncated' "$path" \
		&& ! grep -Fq 'provider returned no complete Markdown handoff' "$path" \
		&& grep -Fq "$diff_file" "$path" \
		&& evidence_present "$path" manifest "$manifest_file" \
		&& evidence_present "$path" diff "$diff_file" \
		&& evidence_present "$path" source "$source_file" \
		&& evidence_present "$path" caller "$caller_file" \
		&& evidence_present "$path" test "$test_file"
}

# A displayed workspace path is not an opaque runtime artifact ID. Require
# both the direct file_path call and the matching successful view response;
# a failed artifact_ref attempt or prose-only ledger cannot satisfy this.
#
# Count matches with `grep -c` rather than testing with `grep -q` at the end
# of a pipe. `-q` exits the instant it sees the first match, which under
# `set -o pipefail` can race the upstream grep: the upstream is still writing
# when its stdout pipe closes, it is killed by SIGPIPE, and pipefail reports
# that non-zero (141) status for the whole pipeline even though the match was
# genuinely found. `-c` always reads its input to EOF, so no stage exits
# early and there is nothing for SIGPIPE to race.
evidence_present() {
	local path="$1" role="$2" file="$3"
	local call_count result_count
	call_count="$(grep -F '"event":"tool_call"' "$path" | grep -F '"tool":"view"' | grep -Fc "\\\"file_path\\\":\\\"$file\\\"" || true)"
	result_count="$(grep -F '"event":"tool_result"' "$path" | grep -F '"tool":"view"' | grep -Fc "\\u003c$file\\u003e" || true)"
	(( call_count > 0 )) \
		&& (( result_count > 0 )) \
		&& grep -Fq "\\\"path\\\":\\\"$file\\\"" "$path" \
		&& grep -Fq "\\\"purpose\\\":\\\"$role\\\"" "$path"
}

promote_handoff() {
	local batch_id="$1"
	local marker="$reviewed/$batch_id.ok"
	local diff_file="$coverage_root/batches/$batch_id/diff.patch"
	local source_file="$coverage_root/batches/$batch_id/source-context.txt"
	local caller_file="$coverage_root/batches/$batch_id/caller-context.txt"
	local test_file="$coverage_root/batches/$batch_id/test-context.txt"
	[[ -f "$diff_file" && -f "$source_file" && -f "$caller_file" && -f "$test_file" ]] || {
		echo "missing deterministic review evidence file: $batch_id" >&2
		return 1
	}
	local candidate="" stamp path
	while read -r stamp path; do
		[[ -n "$path" ]] || continue
		if candidate_satisfies_batch "$path" "$batch_id" "$manifest" "$diff_file" "$source_file" "$caller_file" "$test_file"; then
			candidate="$path"
			break
		fi
	done < <(find "$task_output_dir" -maxdepth 1 -type f -name "*-$run_id-attempt-*.jsonl" -printf '%T@ %p\n' | sort -nr)
	if [[ -z "$candidate" ]]; then
		echo "no complete current-run Markdown handoff for $batch_id" >&2
		return 1
	fi
	local sha
	sha="$(sha256sum "$diff_file" | awk '{print $1}')"
	mkdir -p "$reviewed"
	printf '%s\t%s\n' "$batch_id" "$sha" > "$marker"
}

mkdir -p "$reviewed"
while IFS=$'\t' read -r batch_id batch_files diff_file source_file caller_file test_file status; do
	[[ -n "$batch_id" ]] || continue
	[[ "$batch_id" == \#* ]] && continue
	if [[ ! -s "$reviewed/$batch_id.ok" ]]; then
		promote_handoff "$batch_id" || exit 5
	fi
done < "$batches"
reviewed_count="$(find "$reviewed" -maxdepth 1 -type f -name 'batch-*.ok' | wc -l | tr -d ' ')"
if [[ "$reviewed_count" != "$expected_batches" ]]; then
	echo "coverage incomplete: $reviewed_count/$expected_batches batches marked reviewed" >&2
	exit 6
fi
while IFS=$'\t' read -r batch_id batch_files diff_file source_file caller_file test_file status; do
	[[ -n "$batch_id" ]] || continue
	[[ "$batch_id" == \#* ]] && continue
	[[ -f "$reviewed/$batch_id.ok" ]] || { echo "missing reviewed marker: $batch_id" >&2; exit 7; }
	[[ -s "$diff_file" || "$batch_files" == "0" ]] || { echo "missing diff evidence: $batch_id" >&2; exit 8; }
	[[ -s "$source_file" && -s "$caller_file" && -s "$test_file" ]] || { echo "missing source/caller/test evidence: $batch_id" >&2; exit 8; }
	expected_sha="$(sha256sum "$diff_file" | awk '{print $1}')"
	actual_sha="$(awk -F '\t' 'NF >= 2 { print $2; exit }' "$reviewed/$batch_id.ok")"
	[[ "$actual_sha" == "$expected_sha" ]] || { echo "stale reviewed marker: $batch_id" >&2; exit 9; }
done < "$batches"
printf 'coverage complete: %s files across %s batches\n' "$expected_files" "$expected_batches"
