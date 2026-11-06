# Benchmarks

Record here, for every published number:
- Hardware (CPU, RAM, disk), OS, Go version
- Message size, number of producers/consumers, batch size, replication factor
- Throughput (msg/s), latency p50/p99, failover time
- Comparison baseline (Kafka / NATS) with the same configuration

Run: `make bench`

## Results

### Storage: reading with and without the sparse offset index
`BenchmarkRead` in `internal/storage/index_test.go`: one segment of 200,000 records
(100-byte values, about 27 MB on disk), reading a 10-message window at a random
offset (fixed RNG seed). Command: `go test -run '^$' -bench BenchmarkRead -benchtime 2s ./internal/storage/`

| Index interval | Time per read | Speedup vs no index |
|---|---|---|
| none (scan from segment start) | 59,793,770 ns (about 59.8 ms) | 1x |
| 64 KiB | 180,688 ns (about 181 us) | about 331x |
| 4 KiB | 30,381 ns (about 30 us) | about 1,968x |

Machine: Intel Core i7-12700H, 40 GB RAM, Windows 11, Go 1.27.3. The file is small
enough that reads are mostly served from the OS page cache. Single run, so treat the ratios as an order of
magnitude, not a precise figure.
