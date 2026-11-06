# 1. Record architecture decisions

Date: 2026-11-06

## Status
Accepted

## Context
We need to record the significant design decisions of raftlog and the reasons behind them, so they can be explained and revisited later.

## Decision
We use lightweight Architecture Decision Records (ADRs), one numbered Markdown file per decision in `docs/adr/`. Each has: Context, Decision, Consequences.

## Consequences
Every non-trivial choice (language, Raft implementation, storage format, RPC layer, ...) gets an ADR, e.g. `0002-implement-raft-from-scratch.md`.
