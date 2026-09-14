#!/usr/bin/env bash

set -euo pipefail

usage() {
	echo "usage: scripts/operator-usability-measure.sh <baseline|candidate> [output|-]" >&2
	exit 2
}

if (($# < 1 || $# > 2)); then
	usage
fi

mode=$1
case "$mode" in
	baseline|candidate) ;;
	*) usage ;;
esac

repo_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
corpus="$repo_root/docs/reference/operator-usability-corpus.tsv"
output=${2:--}

render() {
	echo "# Operator usability measurement"
	echo
	echo "Mode: $mode"
	echo
	echo "Record one row per participant and scenario. Score State, What, and Next against the answer key after the session; do not reveal it during the task."
	echo
	echo "| Participant | Scenario | Order | State correct | What correct | Next correct | Seconds | Actions | Safety incidents | Notes |"
	echo "|---|---|---:|---:|---:|---:|---:|---:|---:|---|"
	while IFS=$'\t' read -r id journey _scenario _state _what _next _safety; do
		[[ "$id" == "id" ]] && continue
		printf '|  | %s (%s) |  |  |  |  |  |  |  |  |\n' "$journey" "$id"
	done <"$corpus"
	echo
	echo "## Facilitator answer key"
	echo
	echo "| Scenario | Expected State | Expected What | Expected Next | Safety gate |"
	echo "|---|---|---|---|---|"
	while IFS=$'\t' read -r id journey _scenario state what next safety; do
		[[ "$id" == "id" ]] && continue
		printf '| %s (%s) | %s | %s | %s | %s |\n' "$journey" "$id" "$state" "$what" "$next" "$safety"
	done <"$corpus"
}

if [[ "$output" == "-" ]]; then
	render
else
	render >"$output"
	echo "wrote $output"
fi
