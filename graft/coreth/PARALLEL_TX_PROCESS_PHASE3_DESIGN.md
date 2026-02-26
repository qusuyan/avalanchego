# Parallel Tx Processing: Phase 3 Design

Last updated: 2026-02-26

## Goal

Implement true parallel transaction execution in `state_processor` while preserving deterministic, serial-equivalent block output.

## Current Baseline

1. `StateDBLastWriterBlockState` exists and is wired into the `cfg.ParallelExecutionEnabled` path.
2. `ValidateReadSet(...)` is implemented (existence + object version checks).
3. Tx processing in `state_processor.Process(...)` is still serial even in parallel mode.
4. Scheduler and true parallel execution path are not implemented yet.

## Selected Phase 3 Design

The first concrete Phase 3 design is:

1. Single mixed worker pool (no dedicated executor vs validator roles).
2. Validation is always sequential by tx index and is prioritized over execution.
3. Task selection uses atomic cursors:
   - `nextExecToIssue`: incremented when a worker claims a new execution slot.
   - `nextValidateToCommit`: incremented only after tx is committed.
4. Worker fetch policy:
   - first try validation for `nextValidateToCommit` if execution result is available and not claimed.
   - otherwise claim next execution index.
5. On validation failure at tx `i`:
   - re-execute tx `i` immediately by the same worker against the latest canonical image.
   - commit rerun result directly (no re-enqueue, no attempt counter, no second validate).
6. Determinism rule:
   - canonical state is mutated only on sequential validation/commit path for `nextValidateToCommit`.

## Why direct rerun-commit is valid

Because validation is strictly sequential:

1. Before validating tx `i`, all `0..i-1` are already committed.
2. No tx `> i` can commit before tx `i`.
3. If tx `i` fails validation, rerunning immediately observes final canonical prefix and cannot be invalidated by later txs.
4. Therefore rerun result can be committed immediately under the same validation critical section.

## Data Model

1. Per-tx immutable inputs:
   - tx pointer/hash
   - precomputed `Message` (or deterministic reconstruction inputs)
2. Per-tx execution slot:
   - `ready` flag
   - `claimedForValidation` flag
   - execution result payload (`TxnState`, receipt template, gas used, err)
3. Shared atomics:
   - `nextExecToIssue`
   - `nextValidateToCommit`
4. Shared synchronization:
   - one validation/commit mutex (or equivalent exclusive claim on `nextValidateToCommit`)

## Worker Loop

1. Read `v := nextValidateToCommit`.
2. If result for `v` is ready and can be claimed for validation:
   - enter validation/commit critical section
   - confirm `v` is still current
   - validate read set
   - if success: commit result and increment `nextValidateToCommit`
   - if failure: rerun tx `v` immediately and commit rerun result; increment `nextValidateToCommit`
   - continue
3. Otherwise claim `i := fetch_add(nextExecToIssue, 1)`.
4. If `i < len(txs)`, execute tx `i` speculatively and publish result as ready.
5. Stop when `nextValidateToCommit == len(txs)`.

## Validation Rules

1. For each address existence read in `TxReadSet`:
   - compare observed version to current canonical existence version.
2. For each object read in `TxReadSet`:
   - compare observed version to current canonical object version.
3. Any mismatch => conflict => rerun.
4. Exact match for all reads => eligible to commit when cursor reaches tx index.

## Integration Into Block Processing (`state_processor`)

Current `Process(...)` parallel branch executes and commits txs in a single loop. Integration should:

1. Split current logic into:
   - serial path (unchanged)
   - new `processParallelSequentialValidate(...)`
2. Build shared block-scoped context once:
   - `blockState := parallel.NewStateDBLastWriterBlockState(statedb, txHashes)`
   - signer/message precompute for all txs
3. Replace shared mutable `GasPool` and shared `vm.EVM` usage for speculative workers:
   - each execution must use its own local `GasPool`/EVM instance to avoid races
   - canonical gas-accounting/receipt cumulative gas is finalized only at commit
4. Store per-tx execution outputs in slots (not directly appended to final receipts/logs during execute).
5. During sequential commit:
   - apply `TxnState.CommitTxn()` (or equivalent extracted commit payload)
   - compute final `CumulativeGasUsed`
   - place receipt at index `i`
   - when deriving cumulative gas from `blockGasLimit - remainingGas`, commit order must remain deterministic by tx index
6. After all commits:
   - `blockState.WriteBack()`
   - continue existing `engine.Finalize(...)` path

## Remaining Implementation Tasks

1. Add worker loop and task claiming primitives in `state_processor.go`.
2. Refactor `applyTransactionSpeculative(...)` to return commit-time-friendly execution payload.
3. Remove shared mutable state from speculative execution path (`GasPool`, `vm.EVM`).
4. Ensure receipt fields requiring canonical order are assigned at commit.
5. Add deterministic concurrency tests for conflict-free and conflict-heavy blocks.

## Test Plan

1. Unit tests:
   - task-claim correctness (validation prioritized, no duplicate claims)
   - commit cursor monotonicity and single-commit per index
   - validation-fail direct rerun-commit behavior
2. Integration tests:
   - conflict-free block: parallel output equals serial
   - conflict-heavy hotspot block: deterministic output equals serial
   - same-sender nonce chain: deterministic output equals serial
3. Determinism tests:
   - repeated runs with same input produce identical receipts/logs/state root.

## First Implementation Order

1. Introduce `processParallelSequentialValidate(...)` behind current parallel flag.
2. Add per-tx result slots + atomic cursors + validation priority fetch logic.
3. Implement sequential validation critical section and direct rerun-commit on fail.
4. Move receipt cumulative gas finalization to commit.
5. Add focused deterministic tests and baseline metrics.
