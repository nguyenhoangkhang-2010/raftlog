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

# One line per run: scenario, build, round, messages/s, p50 and p99 of a request in ms.
: >"$out/runs.csv"
for file in "$out"/*.json; do
  base="$(basename "$file" .json)"
  scenario="${base%%-old-*}"
  scenario="${scenario%%-new-*}"
  which=old
  case "$base" in *-new-*) which=new ;; esac
  round="${base##*-r}"
  value() { grep -m1 "\"$1\"" "$file" | sed -E 's/.*: *([0-9.eE+-]+).*/\1/'; }
  msgs="$(value messages_per_sec)"
  p50="$(value p50_ns)"
  p99="$(value p99_ns)"
  awk -v s="$scenario" -v w="$which" -v r="$round" -v m="$msgs" -v a="$p50" -v b="$p99" \
    'BEGIN { printf "%s,%s,%s,%.0f,%.1f,%.1f\n", s, w, r, m, a / 1e6, b / 1e6 }' >>"$out/runs.csv"
done

{
  echo "scenario   build  runs  messages/s: median (min - max)    p50 ms: median    p99 ms: median"
  for name in $(cut -d, -f1 "$out/runs.csv" | sort -u); do
    for which in old new; do
      grep "^$name,$which," "$out/runs.csv" | sort -t, -k4 -n | awk -F, -v n="$name" -v w="$which" '
        { m[NR] = $4; a[NR] = $5; b[NR] = $6 }
        END {
          if (NR == 0) exit
          # medians of the three columns, each sorted on its own
          for (c = 0; c < 3; c++) {
            for (i = 1; i <= NR; i++) v[i] = (c == 0 ? m[i] : (c == 1 ? a[i] : b[i]))
            for (i = 1; i <= NR; i++) for (j = i + 1; j <= NR; j++) if (v[j] < v[i]) { t = v[i]; v[i] = v[j]; v[j] = t }
            med[c] = (NR % 2) ? v[(NR + 1) / 2] : (v[NR / 2] + v[NR / 2 + 1]) / 2
            if (c == 0) { lo = v[1]; hi = v[NR] }
          }
          printf "%-10s %-5s %4d  %10.0f (%.0f - %.0f)  %14.1f  %16.1f\n", n, w, NR, med[0], lo, hi, med[1], med[2]
        }'
    done
  done
} | tee "$out/summary.txt" >&2
echo "done: $out" >&2
