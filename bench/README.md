# Benchmarks

Record here, for every published number:
- Hardware (CPU, RAM, disk), OS, Go version
- Message size, number of producers/consumers, batch size, replication factor
- Throughput (msg/s), latency p50/p99, failover time
- Comparison baseline (Kafka / NATS) with the same configuration

Run: `make bench`
