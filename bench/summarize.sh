#!/usr/bin/env bash
# Summarizes a directory of runs made by bench/compare.sh (files named <scenario>-old-r<N>.json
# and <scenario>-new-r<N>.json): writes runs.csv, one line per run, and summary.txt, the median
# and the range of messages/s and the median request latency for each scenario and build.
#
#   bench/summarize.sh bench/results/my-comparison
set -euo pipefail

out="${1:?usage: bench/summarize.sh <directory of runs>}"

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

