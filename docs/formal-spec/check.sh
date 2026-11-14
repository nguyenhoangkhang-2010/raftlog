#!/bin/sh
# Checks every model in this directory with TLC, and says whether each ended as it must:
# the real models must pass, and the models with a deliberate mistake must fail.
#
#   TLA2TOOLS=/path/to/tla2tools.jar docs/formal-spec/check.sh          # all of them
#   TLA2TOOLS=/path/to/tla2tools.jar docs/formal-spec/check.sh quick    # without Figure8 and BugNoUpToDateCheck, which take the longest
#
# tla2tools.jar is the model checker, from https://github.com/tlaplus/tlaplus/releases. Needs Java 11 or later.
# TLC keeps its states on disk, in $TLC_STATES (a new temporary directory by default); a model can use a few GB.
set -u
cd "$(dirname "$0")"
: "${TLA2TOOLS:?set TLA2TOOLS to the path of tla2tools.jar}"
states=${TLC_STATES:-$(mktemp -d)}
quick=0
[ "${1:-}" = "quick" ] && quick=1

status=0
check() { # spec cfg expectation (pass|fail)
  spec=$1 cfg=$2 want=$3
  out=$(java -XX:+UseParallelGC -cp "$TLA2TOOLS" tlc2.TLC -workers auto -metadir "$states/$cfg" -config "$cfg" "$spec.tla" 2>&1)
  if printf '%s' "$out" | grep -q "No error has been found"; then got=pass
  elif printf '%s' "$out" | grep -q "is violated"; then got=fail
  else got=error; fi
  size=$(printf '%s' "$out" | grep -E "states generated" | tail -1 | sed 's/^ *//')
  if [ "$got" = "$want" ]; then verdict=ok; else verdict="WRONG (it ${got}ed)"; status=1; fi
  printf '%-26s must %-4s %-8s %s\n' "$cfg" "$want" "$verdict" "$size"
  if [ "$got" = error ]; then printf '%s\n' "$out" | tail -15; fi
}

check Raftlog Raftlog.cfg pass
check Raftlog BugVoteTwice.cfg fail
[ "$quick" = 1 ] || check Raftlog BugNoUpToDateCheck.cfg fail
check Raftlog BugCommitOldTerms.cfg fail
[ "$quick" = 1 ] || check Raftlog Figure8.cfg pass
check Points Points.cfg pass
check Points PointsNoRefunds.cfg pass
check Points BugNoAvailabilityCheck.cfg fail

[ -z "${TLC_STATES:-}" ] && rm -rf "$states"
exit $status
