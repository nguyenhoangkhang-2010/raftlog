# raftlog design document

> Status: draft. Fill this in as you build; it is one of the strongest signals in the repo.

## 1. Problem and motivation
TODO

## 2. Goals and non-goals
**Goals**
- Replicated, durable, ordered log per partition
- Survive loss of a minority of nodes without losing acknowledged messages

**Non-goals (for now)**
- Exactly-once semantics, transactions
- Security (TLS/auth), dynamic membership, multi-datacenter

## 3. Architecture
TODO: diagram + component description (storage, raft, broker, server).

## 4. Data model
- Message: `offset, timestamp, key, value, headers`
- On-disk record: `[length][crc32][offset][timestamp][keyLen][key][valLen][value]`
- Raft: `LogEntry{term, index, command}`, `PersistentState{currentTerm, votedFor}`

## 5. Write path / read path
TODO

## 6. Failure model and guarantees
TODO: crash, restart, partition, slow node. What is guaranteed?

## 7. Testing strategy
Unit, integration, chaos (kill leader, network partition, delay), linearizability (Porcupine).

## 8. Benchmarks
TODO: methodology, hardware, results, comparison with Kafka/NATS.

## 9. Open questions and future work
TODO
