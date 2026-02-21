# Parallel Transaction Execution Notes

Last updated: 2026-02-20

## Scope

This document summarizes:

1. The current serial execution path in this workspace.
2. The agreed target direction for parallel transaction execution.
3. Key constraints and open decisions to pick up later.

## Progress Update (2026-02-20)

### Completed Since Last Update

1. Integrated block-scoped `StateDBLastWriterBlockState` into `StateProcessor` parallel path:
- pre-collect tx hashes
- create one block state for all txs
- run `TxnState.CommitTxn()` per tx into block state
- call `blockState.WriteBack()` before `engine.Finalize(...)`

2. Extended `BlockState` contract with explicit `WriteBack() error` phase.

3. Implemented `StateDBLastWriterBlockState` with:
- last-writer-wins object/existence tracking (`objectStates`, `existsStates`)
- read path that checks account liveness and returns empty value for deleted accounts
- tx-indexed logs/preimages buffers

4. Switched internal write-path storage to atomic-pointer based structures:
- object and existence entries use per-key atomic pointer CAS updates
- logs/preimages use pre-sized tx-index arrays of atomic pointers

5. Fixed log replay context in `WriteBack()`:
- `SetTxContext(txHash, txIndex)` is set before replaying logs for each tx slot.

6. Updated lifecycle conflict handling:
- `ApplyWriteSet` now treats account creation as a fresh state epoch by clearing stale object entries for that address (while preserving balance semantics used by current `TxWriteSet.CreateAccount` path).

7. Updated tests and test harness:
- `TxnState` tests now use a concrete test `BlockState` stub instead of `nil`.
- Parallel/core test targets pass with current wiring.

### Current Status

1. Parallel processing path now writes into block-scoped LWW state and writes back to `StateDB` before finalize.
2. `ValidateReadSet(...)` is still a placeholder (`true`).
3. Conflict retries/scheduler path is not yet implemented; execution is still in-order.
4. Lifecycle semantics are improved but still need broader parity validation against `StateDB` edge cases.

### Next Work Items

1. Implement real `ValidateReadSet(...)` against current canonical versions in `StateDBLastWriterBlockState`.
2. Add focused tests for destroy/recreate/write interactions across tx boundaries.
3. Add tests for log/preimage replay determinism and tx-context correctness.
4. Validate parity for `SelfDestruct`/`Selfdestruct6780` and account existence behavior under mixed tx sequences.

## Progress Update (2026-02-18)

### Completed Since Last Update

1. Added a concrete `BlockState` contract and `SequentialBlockState` adapter in `graft/coreth/core/parallel/block_state.go`.
2. `TxnState` now depends on `BlockState` (not `*state.StateDB`) and commits through:
- `ApplyWriteSet(txIndex, *TxWriteSet)`
- `Commit(...)` delegation for test-only paths
3. Added versioned read plumbing and typed tracking structures in `graft/coreth/core/parallel_state_types.go`:
- `ObjectVersion`, `VersionedValue`
- `TxReadSet` (`accountExistsVersion`, `objectVersions`)
- `TxWriteSet` split into account lifecycle changes + object writes
4. Added explicit object key for code hash (`StateObjectCodeHash` / `CodeHashKey`) and typed value accessors (`CodeHash()`).
5. `state_processor` parallel-enabled path now instantiates:
- `NewTxnState(NewSequentialBlockState(statedb), tx.Hash(), i, nonce)`
6. `TxnState` was aligned closer to libevm semantics for tx-local bookkeeping:
- `state.AccessList` and `state.TransientStorage` are used directly
- `Prepare(...)` now applies Berlin/Shanghai warming rules and resets transient storage
- `GetLogs(...)` is tx-local only (non-matching tx hash returns `nil`)
7. `TxnState` read path now captures observed versions from `BlockState.Read(...)` and stores them in `TxReadSet` for later validation.
8. Added `StateDBLastWriterBlockState` (`graft/coreth/core/parallel/statedb_last_writer_block_state.go`) as the new phase-2 direction:
- wraps a single `*state.StateDB` baseline
- tracks in-block canonical last-writer-wins state (`objectStates`, `existsStates`)
- reserves `logsByTx` / `preimagesByTx` per transaction index
- defers materialization to `StateDB` until `BlockState.Commit(...)`

### Current Status

1. Wiring is in place for `TxnState -> BlockState.Read/ApplyWriteSet/Exists/GetCommittedState`.
2. `StateDBBlockState` currently uses placeholder versions (`COMMITTED_VERSION` / `ERROR_VERSION`) and `ValidateReadSet(...)` is still a stub that returns `true`.
3. Account lifecycle is now represented in `TxWriteSet.accountLifecycleChanges`.
4. `StateDBLastWriterBlockState.ApplyWriteSet(...)` is now the canonical merge point for lifecycle/object conflicts:
- resolve existence/lifecycle first
- destruct clears overlay object writes for that address and keeps zero-balance canonicalization
- apply object writes only if not superseded by newer lifecycle/object versions
5. `TxnState.Validate()` remains a phase placeholder (`true`) and is not yet connected to coordinator-driven read-set validation.

### Next Work Items

1. Complete wiring from `state_processor` to a single block-scoped `StateDBLastWriterBlockState`.
2. Implement `ValidateReadSet(*TxReadSet)` against canonical current versions.
3. Complete lifecycle semantics parity (`SelfDestruct`, EIP-6780 behavior, `Empty`/`Exist` edge cases) under tx-local overlay + canonical apply.
4. Add focused tests for:
- read version consistency capture and mismatch behavior
- account existence version tracking
- ordered `ApplyWriteSet` lifecycle + value writes
- conflict detection via `ValidateReadSet`
5. Move from serial-in-order speculative wiring to scheduler-managed parallel run/validate/commit.

## Current Serial Execution Path


1. Block insertion entry point:
- `graft/coreth/core/blockchain.go:1329` (`insertBlock`)
- Creates `StateDB` from parent root via `state.New(parent.Root, bc.stateCache, bc.snaps)`.
- Starts prefetcher, then calls processor.

2. Block transaction processing (current WIP integration):
- `graft/coreth/core/state_processor.go:71` (`Process`)
- Creates one EVM context for the block.
- Iterates txs strictly in order.
- Per tx: `TransactionToMessage` -> `NewTxnState(NewStateDBBlockState(statedb), txHash, txIndex)` ->
  `applyTransaction(..., txState, ...)` -> `txState.finalise()`.

3. Transaction application (current WIP integration):
- `graft/coreth/core/state_processor.go` (`applyTransaction`)
- Calls `ApplyMessage(...)`.
- `evm.Reset(...)` uses the passed tx-local state (`TxnState`) as `vm.StateDB`.
- Builds receipt with cumulative gas and logs; per-tx `PostState` is intentionally empty.

4. State transition core:
- `ApplyMessage(...)` reaches `graft/coreth/core/state_transition.go`.

5. Post-processing in `insertBlock`:
- `ValidateState(...)`
- later state commit path (through `StateDB.Commit(...)` in write path helpers).

## StateDB Location and Ownership

- `StateDB` type and constructor are in `../libevm/core/state/statedb.go`.
- Constructor: `New(root common.Hash, db Database, snaps SnapshotTree) (*StateDB, error)`.
- Current workspace uses module-versioned `github.com/ava-labs/libevm`.
- Local workspace wiring is through `go.mod` replace (`github.com/ava-labs/libevm => ../libevm`).

## Agreed Parallel Direction (No Sequential Fallback)

User requirement: do not switch to sequential path when conflicts are high.

### Target model

1. Keep one canonical block-level state (`blockState`) for validation/commit.
2. Execute txs speculatively in parallel with tx-local read/write tracking.
3. Pipeline per tx: `run` -> `validate` -> `commit`.
4. Validation checks tx read-set against current canonical block state.
5. Commit applies tx write-set to canonical block state in canonical tx order.
6. Validation timing is scheduler-controlled and may happen immediately or later.
7. On validation conflict, tx is submitted for another `run` attempt (same operation as first run).

### Determinism rule

- Commit cursor must advance by tx index order (`0..n-1`), regardless of execution completion order.

### Receipt root change

- In parallel mode, per-tx `receipt.PostState` can be left empty.

## Expected Components

1. Tx-local execution artifacts:
- `TxReadSet`
- `TxWriteSet`
- `TxExecResult` (status, logs, gas, error, rw-sets)

2. Coordinator/scheduler:
- Fixed worker count for speculative `run`.
- Ordered validate/commit coordinator.
- Conflict handling by scheduling additional `run` attempts.

3. State versioning for conflict checks:
- Per-key version tracking in canonical state view.

Note: current implementation keeps `SequentialBlockState` for compatibility wiring and
introduces `StateDBLastWriterBlockState` for phase-2 last-writer-wins semantics. Version validation is still placeholder.

## State Object Versioning Rules (Planned for BlockState Phase)

When `BlockState` is introduced, track versions at fine granularity.

1. Version meaning:
- Version value is the tx index of the last committed transaction that changed the object.
- If untouched in the block so far, version is the baseline pre-block value.

2. Object granularity for conflict detection:
- `Balance(addr)`
- `Nonce(addr)`
- `CodeHash(addr)`
- `Code(addr)`
- `Storage(addr, slot)` for each distinct storage key
- `Extra(addr)` for account extra metadata
- account existence version for `Exist(addr)`/`Empty(addr)` checks

3. Derived object coupling:
- `CodeSize(addr)` is derived as `len(Code(addr))`, so it does not need a separate tracked object key.
- `StateRoot(addr)` is coupled to storage and changes whenever any `Storage(addr, slot)` changes, but it is not tracked as a separate tx-local object key in the current EVM execution path.

4. Validation behavior (future phase):
- Read-set entries must carry object versions observed during `run`.
- Validation compares observed versions to canonical current versions at commit time.
- Any mismatch is a conflict and requires another `run` attempt.

## Open Design Decisions

1. Granularity of read/write keys:
- exact state key schema for the versioned objects listed above

2. Interaction points with libevm:
- where to capture rw-sets cleanly
- how to apply write-sets efficiently to canonical state

3. Gas and receipt finalization in ordered commit:
- maintain canonical cumulative gas and log indices only at commit time

## Known Risks

1. `StateDB` is not designed for concurrent shared mutation.
2. High-conflict blocks can increase repeated `run` attempts.
3. Same-sender nonce chains likely conflict frequently.
4. Memory overhead from tx-local state + rw-sets.
5. Must preserve exact consensus behavior despite speculative execution.

## Current Policy Choices

1. No sequential fallback.
2. No max-retry abort policy for now.
3. Progress relies on deterministic ordered commit and scheduler behavior.

## Next Session Starting Point

1. Add/adjust tests for tx-local receipt/log behavior under `TxnState` execution path.
2. Decide exact `TxnState` commit timing model for upcoming scheduler integration
   (still ordered, deterministic).
3. Complete feature flag/config gate wiring and test toggling behavior.
4. Enter Phase 2 (`BlockState`) before starting scheduler and validation work planned for Phase 3.

## Staged Migration Plan

### Phase 0: Feature Gate

Goal: add rollout controls before behavior changes.

1. Add feature flags:
- `ParallelExecutionEnabled`
- `ParallelExecutionWorkers`

2. Keep existing serial path unchanged and default.

Acceptance criteria:
- Build compiles with new flags.
- No runtime behavior change when flag is disabled.

### Phase 1: TxnState Overlay on Existing StateDB

Goal: introduce per-tx read/write tracking without replacing canonical storage yet.

1. Implement `TxnState`:
- Wraps canonical state through `BlockState` (currently `StateDBBlockState` adapter over `*state.StateDB`).
- Maintains tx-local read set and write set.
- Read path: `write-set first`, then canonical read.
- Write/delete path: update write set only.

2. Implement `Commit()`:
- Applies write set into canonical state in deterministic commit order.
- Uses immutable tx context (`txHash`, `txIndex`) created with `TxnState`.

3. Keep receipt `PostState` empty in parallel mode.

Acceptance criteria:
- Deterministic ordered commit with parallel run attempts.
- `TxnState` methods are compatible with current EVM `vm.StateDB` usage.

### Phase 2: Introduce BlockState (StateDB-Backed Overlay Canonical State)

Goal: keep canonical ownership in `state.StateDB` while moving tx-merge/read-version
logic into `BlockState` overlay structures.

1. Implement `StateDBLastWriterBlockState` around a single block `*state.StateDB`:
- canonical read interfaces (`overlay-first`, `StateDB` fallback)
- in-memory canonical last-writer-wins state:
  - `objectStates map[StateObjectKey]VersionedValue`
  - `existsStates map[common.Address]ExistsState`
  - `logsByTx [][]*types.Log`
  - `preimagesByTx []map[common.Hash][]byte`
- block-level `Commit()`:
  - apply merged last-writer-wins state to `StateDB`
  - call `StateDB.Commit(...)`

2. Keep compatibility adapter (`SequentialBlockState`) until full processor wiring migration completes.
3. Route `TxnCommit` writes into `BlockState` overlay rather than mutating canonical `StateDB` directly during tx execution.

Acceptance criteria:
- Parallel tx execution path no longer mutates canonical `StateDB` directly during tx run/finalize.
- Overlay materialization preserves `StateDB` semantics and commit correctness.

### Phase 3: Parallel Processor Path

Goal: run txs in parallel using scheduler while preserving deterministic commit.

1. Add `ProcessParallel(...)` in `state_processor.go`.
2. Scheduler creates `TxnState` per run attempt.
3. Move `TxnState.Validate()` from phase-1 placeholder behavior to coordinator-driven conflict checks.
4. Commit coordinator validates read sets and commits by tx index order.
5. On conflict, schedule another run attempt (no sequential fallback, no max-retry abort for now).

Acceptance criteria:
- Same block output as serial path on conflict-free and conflict-heavy test sets.
- Deterministic results across repeated runs.

### Phase 4: Semantics Parity and Hardening

Goal: ensure consensus-equivalent behavior and production safety.

1. Validate parity for:
- refunds
- logs and index ordering
- access list / warm-cold semantics
- transient storage
- self-destruct behavior and account lifecycle

2. Add stress and determinism tests:
- same-sender nonce chains
- conflicting storage hotspots
- contract create/self-destruct patterns

Acceptance criteria:
- Consensus-sensitive behavior matches serial reference.
- Stable deterministic outputs under load.

### Phase 5: Rollout

Goal: safe activation and observability.

1. Add metrics:
- run attempts per tx
- validation conflicts
- commit wait/latency
- parallel worker utilization

2. Gate by config/network rollout policy.
3. Keep serial processor path available as operational fallback until confidence target is met.

## API Contract (Current Design)

### Shared Types

```go
type StateObjectKind uint8

const (
    StateObjectBalance StateObjectKind = iota + 1
    StateObjectNonce
    StateObjectCodeHash
    StateObjectCode
    StateObjectStorage
    StateObjectExtra
)

type StateObjectKey struct {
    Kind    StateObjectKind
    Address common.Address
    Slot    common.Hash // only used for storage
}

type ObjectVersion = uint64

const (
    COMMITTED_VERSION ObjectVersion = 0
    ERROR_VERSION     ObjectVersion = ^uint64(0)
)

type VersionedValue struct {
    Value   StateObjectValue
    Version ObjectVersion
}

type TxReadSet struct {
    accountExistsVersion map[common.Address]ObjectVersion
    objectVersions       map[StateObjectKey]ObjectVersion
}

type TxWriteSet struct {
    accountLifecycleChanges map[common.Address]AccountLifecycle
    writes                  map[StateObjectKey]StateObjectValue
}
```

### BlockState

```go
type BlockState interface {
    Exists(addr common.Address) (bool, ObjectVersion, error)
    Read(key StateObjectKey, txIndex uint64) (VersionedValue, error)
    GetCommittedState(key StateObjectKey) (common.Hash, error)
    ApplyWriteSet(txIndex uint64, ws *TxWriteSet) error
    ValidateReadSet(rs *TxReadSet) bool
    Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error)
}
```

### TxnState

`TxnState` remains `vm.StateDB` compatible, with tx-local overlay behavior:

1. Read path: check tx-local writes first, then `BlockState.Read(...)`.
2. Write path: mutate only tx-local `TxWriteSet` until finalize/commit.
3. Finalize path: call `BlockState.ApplyWriteSet(txIndex, writeSet)` in deterministic tx order.
4. Validation path: currently placeholder (`Validate() == true`), with read versions already collected in `TxReadSet`.

Notes:
1. Access list and transient storage are transaction-local and reset in `Prepare(...)`.
2. Logs and preimages are tracked tx-locally.
3. `GetCommittedState(...)` goes through `BlockState.GetCommittedState(...)`.
4. `Snapshot()`/`RevertToSnapshot()` remain no-op/discard semantics in this model.

## Progress Log

### 2026-02-18

Implemented:

1. Introduced `BlockState` + `StateDBBlockState` adapter in `graft/coreth/core/block_state.go` and switched `TxnState` constructor to consume `BlockState`.
2. Split tx-local state tracking into `TxWriteSet` (lifecycle + writes) and `TxReadSet` (account existence + object version tracking) in `graft/coreth/core/parallel_state_types.go`.
3. Added `StateObjectCodeHash`/`CodeHashKey` support and versioned value transport (`VersionedValue`) for typed reads.
4. Updated `TxnState` read path to use `BlockState.Read(...)` and record observed versions for future validation integration.
5. Switched tx-local access-list/transient handling to `state.AccessList` and `state.TransientStorage` with `Prepare(...)` reset semantics.
6. Updated processor wiring to create tx overlays via `NewTxnState(NewStateDBBlockState(statedb), txHash, txIndex)` before speculative apply/finalize.
7. Added and adjusted tests for write-first read behavior and read-set structure assumptions in `graft/coreth/core/txn_state_test.go`.

Notes:

1. `StateDBBlockState.ValidateReadSet(...)` is still a stub and object versions are currently placeholder constants.
2. Full-suite verification was not rerun in this session.


### 2026-02-10

Implemented:

1. Phase-1 `TxnState` overlay scaffolding in `graft/coreth/core/txn_state.go`:
- wraps canonical `*state.StateDB` directly
- immutable tx context (`txHash`, `txIndex`) at construction
- read path: local writes first, then wrapped state
- write path: tx-local buffering, canonical apply at `Commit()`
- `Validate()` currently returns `true` as phase-1 placeholder

2. Shared tx-local tracking types in `graft/coreth/core/parallel_state_types.go`:
- object keys for account fields and storage slot keys
- typed write-set values for balance/nonce/code/codehash
- removed redundant code-size value storage (derive from code length)

3. Storage tracking model uses tx-local object keys:
- storage writes tracked as `WriteSet[StorageKey(addr, slot)] = value`
- keeps `TxnState` as an overlay and avoids mirroring `stateObject` internals
- supports multiple independent storage entries per account

4. Initial tests in `graft/coreth/core/txn_state_test.go`:
- read-own-write behavior for balance/nonce/code/state
- multiple storage-slot tracking on same account
- phase-1 validate behavior

5. Simplified derived-key tracking and lifecycle modeling:
- `CodeHash` and `CodeSize` are derived from `Code` (no separate tx-local object keys)
- `StateRoot` is not tracked as a separate tx-local object key
- account lifecycle uses one per-address state map with last-op-wins semantics
  (instead of separate `created` and `selfDestruct` maps)

6. Initial state-processor integration in `graft/coreth/core/state_processor.go`:
- creates one `TxnState` per tx in block order
- passes `TxnState` into `applyTransaction(...)` so `evm.Reset(...)` executes on tx-local overlay
- commits tx-local writes to canonical `statedb` via `txState.Commit()`
- removed per-tx `IntermediateRoot()`/`Finalise()` calls from `applyTransaction(...)`
- keeps block-level finalization and validation paths on canonical `statedb`

Removed/replaced:

1. Removed `VersionedStateDB` path and related files from active implementation.
2. Replaced earlier generic storage scalar tracking with storage-keyed write-set entries.

Deferred to next phase:

1. Block-level versioned validation in `BlockState`.
2. Parallel scheduler integration in `ProcessParallel(...)`.

Environment note:

1. Local `go test` remains blocked by unrelated dependency/toolchain issues
(`zstd`, `blst`, `libevm/crypto` mismatch), so compile/test verification is incomplete.

### 2026-02-12

Implemented:

1. Cross-repo account-extra plumbing to make `GetExtra`/`SetExtra` trackable through
   tx-local overlays:
- `libevm/core/state/state.libevm.go`: introduced `StateDBExt`-based extra accessors.
- `libevm/core/types/rlp_payload.libevm.go`: `StateAccount` accessor target now uses
  `**StateAccountExtra` so nil extras can be safely materialized.
- `libevm/core/state/statedb.go` + `libevm/core/state/state_object.go`: `SetExtra`
  now goes through object creation + journaled mutation path.

2. Coreth extstate backend contract generalized:
- `graft/coreth/core/extstate/statedb.go`: new `Backend` interface
  (`vm.StateDB` + `state.StateDBExt` + `Logs/Commit/Finalise`), allowing extstate to
  wrap either canonical `*state.StateDB` or tx-local overlay implementations that
  satisfy the same contract.

3. TxnState extra tracking integrated:
- `graft/coreth/core/parallel_state_types.go`: added `StateObjectExtra` and `ExtraKey`.
- `graft/coreth/core/txn_state.go`: added `GetExtra`/`SetExtra` overlay behavior and
  canonical apply path for `StateObjectExtra` during tx-local finalization.

4. EVM wrapping hardening:
- `graft/coreth/core/evm.go`: `wrapStateDB(...)` now checks whether `vm.StateDB`
  satisfies `extstate.Backend` before wrapping, avoiding a hard panic on unsupported
  implementations.

5. Processor integration detail:
- `graft/coreth/core/state_processor.go`: per-tx path applies tx-local writes via
  `txState.finalise()` (overlay apply only), while block-level finalization remains on
  canonical `statedb`.

Validation status (targeted):

1. `libevm`:
- `go test ./core/state -run 'TestGetSetExtra|TestStateObjectEmpty' -count=1` (pass)
- `go test ./core/types -run TestStateAccountExtraViaTrieStorage -count=1` (pass)

2. `coreth`:
- `go test ./graft/coreth/core -run TestStateProcessor -count=1` (pass)
- `go test ./graft/coreth/core/extstate -run 'TestMultiCoinOperations|TestGenerateMultiCoinAccounts' -count=1` (pass)
- `go test ./graft/coreth/plugin/evm/tempextrastest -count=1` (pass)
- `go test ./graft/coreth/core -run TestCreateThenDeletePostByzantium -count=1` (pass)

### 2026-02-13

Implemented:

1. Added feature flags in `graft/coreth/eth/ethconfig/config.go`:
- `ParallelExecutionEnabled`
- `ParallelExecutionWorkers`

2. Regenerated `graft/coreth/eth/ethconfig/gen_config.go` so the new fields are
   included in TOML marshal/unmarshal.

3. Updated this staged plan:
- removed explicit `BlockStateView` / `TxnStateView` phase requirement
- kept `ProcessParallel(...)` and validation work in Phase 3
- updated local libevm workspace note to reflect `go.mod` replace wiring

Notes:

1. Full-suite verification was not rerun in this session; only focused regression and
   integration targets around account extras and tx-local execution were run.
