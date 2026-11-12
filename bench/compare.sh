#!/usr/bin/env bash
# Compares two builds of raftlogd on the write benchmark, alternating them so that both see
# the same machine conditions, and keeps the raw JSON of every run.
#
#   # build the daemon before the change from a clean copy of the parent commit:
#   mkdir /tmp/old && git archive HEAD~1 | tar -x -C /tmp/old && (cd /tmp/old && go build -o /tmp/raftlogd-old ./cmd/raftlogd)
#   make build
#   ROUNDS=5 bench/compare.sh /tmp/raftlogd-old bin/raftlogd bench/results/my-comparison
#
# A machine's speed changes from hour to hour (a disk, other programs), so two result sets made
# at different times are not a comparison, and neither is one run of each. Here each scenario is
# run ROUNDS times for each build; the order of the two builds is swapped from one round to the
# next so that a slow stretch does not always fall on the same build; and the summary gives the
# median and the range, not one number.
# Environment: BIN_DIR (where raftlogbench is, default ./bin), ROUNDS (default 5), ONLY (a regular
# expression: run only the scenarios whose name matches, e.g. ONLY=p1-b1), DURATION and
# WARMUP of each run.
set -euo pipefail

old="${1:?usage: bench/compare.sh <old raftlogd> <new raftlogd> <output directory>}"
new="${2:?usage: bench/compare.sh <old raftlogd> <new raftlogd> <output directory>}"
out="${3:?usage: bench/compare.sh <old raftlogd> <new raftlogd> <output directory>}"
bin_dir="${BIN_DIR:-bin}"
rounds="${ROUNDS:-5}"
duration="${DURATION:-8s}"
warmup="${WARMUP:-3s}"
exe=""
case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) exe=".exe" ;; esac
bench="$bin_dir/raftlogbench$exe"

if [ -e "$out" ] && [ -n "$(ls -A "$out" 2>/dev/null)" ]; then
  echo "$out is not empty; choose a new directory so that results are never mixed" >&2
  exit 1
fi
mkdir -p "$out"
: >"$out/commands.txt"

scenarios=(
  "p1-b1:-producers 1 -batch 1"
  "p16-b1:-producers 16 -batch 1"
  "p16-b10:-producers 16 -batch 10"
  "p16-b100:-producers 16 -batch 100"
)

for round in $(seq 1 "$rounds"); do
  order="old new"
  [ $((round % 2)) -eq 0 ] && order="new old"
  for sc in "${scenarios[@]}"; do
    name="${sc%%:*}"
    args="${sc#*:}"
    if [ -n "${ONLY:-}" ] && ! [[ "$name" =~ $ONLY ]]; then continue; fi
    for which in $order; do
      daemon="$old"
      [ "$which" = new ] && daemon="$new"
      file="$out/$name-$which-r$round.json"
      echo "$bench produce -spawn 3 $args -warmup $warmup -duration $duration -fetch=false -raftlogd $daemon -json" >>"$out/commands.txt"
      echo "== round $round $name $which" >&2
      # shellcheck disable=SC2086 # the scenario arguments are meant to be split
      "$bench" produce -spawn 3 $args -warmup "$warmup" -duration "$duration" -fetch=false -raftlogd "$daemon" -json >"$file"
    done
  done
done


"$(dirname "$0")/summarize.sh" "$out" >&2
echo "done: $out" >&2
