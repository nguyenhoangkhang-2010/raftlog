#!/usr/bin/env bash
# Runs the measurements published in bench/README.md and keeps the raw JSON of each.
#
#   make build && bench/run-matrix.sh bench/results/my-machine
#
# One run of each scenario, on this machine, with the nodes on the same machine as the
# client. Close other programs first: the figures are only as quiet as the machine.
# Environment: BIN_DIR (where raftlogbench and raftlogd were built, default ./bin),
# DURATION and WARMUP (the measured window and the warm-up of each write run).
set -euo pipefail

out="${1:?usage: bench/run-matrix.sh <output directory>}"
bin_dir="${BIN_DIR:-bin}"
duration="${DURATION:-10s}"
warmup="${WARMUP:-3s}"
exe=""
case "$(uname -s)" in MINGW* | MSYS* | CYGWIN*) exe=".exe" ;; esac
bench="$bin_dir/raftlogbench$exe"
daemon="$bin_dir/raftlogd$exe"

if [ -e "$out" ] && [ -n "$(ls -A "$out" 2>/dev/null)" ]; then
  echo "$out is not empty; choose a new directory so that results are never mixed" >&2
  exit 1
fi
mkdir -p "$out"
: >"$out/commands.txt"

run() { # name, then the arguments of raftlogbench
  local name="$1"
  shift
  echo "$bench $* -raftlogd $daemon -json" >>"$out/commands.txt"
  echo "== $name" >&2
  "$bench" "$@" -raftlogd "$daemon" -json >"$out/$name.json"
}

w=(-warmup "$warmup" -duration "$duration")

# Write throughput and latency. The last argument of each name says what changed.
run produce-1node-p1-b1 produce -spawn 1 -producers 1 -batch 1 "${w[@]}"
run produce-3node-p1-b1 produce -spawn 3 -producers 1 -batch 1 "${w[@]}"
run produce-3node-p16-b1 produce -spawn 3 -producers 16 -batch 1 "${w[@]}"
run produce-3node-p16-b10 produce -spawn 3 -producers 16 -batch 10 "${w[@]}"
run produce-3node-p16-b100 produce -spawn 3 -producers 16 -batch 100 "${w[@]}"
run produce-3node-p16-b100-1kb produce -spawn 3 -producers 16 -batch 100 -message-size 1024 "${w[@]}"
run produce-1node-p16-b100 produce -spawn 1 -producers 16 -batch 100 "${w[@]}"

# Failover: the leader killed ten times, with the default Raft timings (100 ms tick,
# election timeout 1 to 2 s) and with timings five times shorter.
run failover-default failover -rounds 10
run failover-fast failover -rounds 10 -node-args "-tick-interval 20ms"

echo "done: $out" >&2
