# Notes on this result set

- Machine: Intel Core i7-12700H (20 threads), 40 GB RAM, NVMe SSD, Windows 11, Go 1.27.3.
  Three `raftlogd` processes and the client on the same machine, loopback network.
- `produce-*.json` come from `bench/run-matrix.sh`, run once. They were made with the
  benchmark tool before the failover check was changed; the write path of the tool did not change.
- `failover-default.json` and `failover-fast.json` were **made again** with the same commands
  after the failover check was fixed. The first run of the default-timings scenario reported 345
  acknowledged writes missing (and two reruns while diagnosing it, 265 and 236): that was a defect
  in the check (it read a node that had not applied the newest entries), not data loss. The file
  with the 345 was deleted; the reruns were not saved. See bench/README.md, "What was wrong with
  the first failover run".
- `commands.txt` lists the command lines (`-raftlogd` points to the binary built from the same tree).
