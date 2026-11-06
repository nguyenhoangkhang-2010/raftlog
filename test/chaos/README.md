# Chaos tests

Fault-injection scenarios run against a real multi-node cluster (build tag `chaos`).

Planned scenarios:
- Kill the leader mid-write; verify no acknowledged message is lost
- Network partition (minority / majority); verify at most one leader per term
- Add latency / drop packets (Toxiproxy); verify ordering and convergence
- Restart nodes repeatedly; verify catch-up

Planned checks: no acknowledged-message loss, one leader per term, per-partition ordering, linearizability (Porcupine).

Run: `make chaos`
