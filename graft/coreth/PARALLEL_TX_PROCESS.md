# Parallel Transaction Execution Notes

Last updated: 2026-02-10

## Scope

This document summarizes:

1. The current serial execution path in this workspace.
2. The agreed target direction for parallel transaction execution.
3. Key constraints and open decisions to pick up later.

## Current Serial Execution Path

1. Block insertion entry point:
- `graft/coreth/core/blockchain.go:1329` (`insertBlock`)
- Creates `StateDB` from parent root via `state.New(parent.Root, bc.stateCache, bc.snaps)`.
- Starts prefetcher, then calls processor.

2. Block transaction processing:
- `graft/coreth/core/state_processor.go:71` (`Process`)
- Creates one EVM context for the block.
- Iterates txs strictly in order.
- Per tx: `TransactionToMessage` -> `statedb.SetTxContext` -> `applyTransaction(...)`.

3. Transaction application:
- `graft/coreth/core/state_processor.go` (`applyTransaction`)
- Calls `ApplyMessage(...)`.
- Then finalizes/intermediate-root path:
  - Byzantium+: `statedb.Finalise(true)`
  - pre-Byzantium: `statedb.IntermediateRoot(...)`
- Builds receipt (`PostState`, gas, logs, etc.).

4. State transition core:
- `ApplyMessage(...)` reaches `graft/coreth/core/state_transition.go`.

5. Post-processing in `insertBlock`:
- `ValidateState(...)`
- later state commit path (through `StateDB.Commit(...)` in write path helpers).

## StateDB Location and Ownership

- `StateDB` type and constructor are in `../libevm/core/state/statedb.go`.
- Constructor: `New(root common.Hash, db Database, snaps SnapshotTree) (*StateDB, error)`.
- Current workspace uses module-versioned `github.com/ava-labs/libevm`.
- `go.work` currently does **not** include local `../libevm`; local libevm edits are not active yet.

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
- `ReadSet`
- `WriteSet`
- `TxExecResult` (status, logs, gas, error, rw-sets)

2. Coordinator/scheduler:
- Fixed worker count for speculative `run`.
- Ordered validate/commit coordinator.
- Conflict handling by scheduling additional `run` attempts.

3. State versioning for conflict checks:
- Per-key version tracking in canonical state view.

Note: for Phase 1 implementation, `TxnState` wraps `*state.StateDB` directly and does
not use block-level version tracking yet.

## State Object Versioning Rules (Planned for BlockState Phase)

When `BlockState` is introduced, track versions at fine granularity.

1. Version meaning:
- Version value is the tx index of the last committed transaction that changed the object.
- If untouched in the block so far, version is the baseline pre-block value.

2. Object granularity for conflict detection:
- `Balance(addr)`
- `Nonce(addr)`
- `Code(addr)`
- `Storage(addr, slot)` for each distinct storage key

3. Derived object coupling:
- `CodeHash(addr)` and `CodeSize(addr)` are coupled to `Code(addr)` and change whenever code changes.
- `StateRoot(addr)` is coupled to storage and changes whenever any `Storage(addr, slot)` changes.

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

1. Define concrete structs/interfaces in `graft/coreth/core` for rw-set artifacts.
2. Draft `ProcessParallel(...)` control flow in `state_processor.go`.
3. Identify minimal libevm API surface needed for rw-set capture/apply.
4. Add a feature flag/config gate to toggle serial vs parallel processor path.

## Staged Migration Plan

### Phase 0: Contracts and Feature Gate

Goal: lock interfaces before behavior changes.

1. Add feature flags:
- `ParallelExecutionEnabled`
- `ParallelExecutionWorkers`

2. Define interfaces in `graft/coreth/core`:
- `BlockStateView` (future canonical state owner)
- `TxnStateView` (EVM-facing state methods + `Commit()` + `Validate()`)

3. Keep existing serial path unchanged and default.

Acceptance criteria:
- Build compiles with new interfaces and flags.
- No runtime behavior change when flag is disabled.

### Phase 1: TxnState Overlay on Existing StateDB

Goal: introduce per-tx read/write tracking without replacing canonical storage yet.

1. Implement `TxnState`:
- Wraps canonical `*state.StateDB` reference directly.
- Maintains tx-local read set and write set.
- Read path: `write-set first`, then canonical read.
- Write/delete path: update write set only.

2. Implement `Commit()`:
- Applies write set into canonical state in deterministic commit order.
- Uses immutable tx context (`txHash`, `txIndex`) created with `TxnState`.

3. Implement `Validate()` as a phase-1 placeholder returning `true` (conflict
validation deferred until `BlockState` version tracking is added).

4. Keep receipt `PostState` empty in parallel mode.

Acceptance criteria:
- Deterministic ordered commit with parallel run attempts.
- `TxnState` methods are compatible with current EVM `vm.StateDB` usage.

### Phase 2: Parallel Processor Path

Goal: run txs in parallel using scheduler while preserving deterministic commit.

1. Add `ProcessParallel(...)` in `state_processor.go`.
2. Scheduler creates `TxnState` per run attempt.
3. Commit coordinator validates read sets and commits by tx index order.
4. On conflict, schedule another run attempt (no sequential fallback, no max-retry abort for now).

Acceptance criteria:
- Same block output as serial path on conflict-free and conflict-heavy test sets.
- Deterministic results across repeated runs.

### Phase 3: Introduce BlockState (Database-Backed Canonical State)

Goal: move canonical ownership from `state.StateDB` toward new `BlockState`.

1. Implement `BlockState` around database/trie/snapshot:
- canonical read interfaces
- in-memory cache for hot objects/slots
- `StartPrefetcher()`, `StopPrefetcher()`
- block-level `Commit()`

2. Keep a compatibility bridge to existing `state.StateDB` semantics where needed.
3. Route `TxnCommit` writes into `BlockState` instead of direct canonical `state.StateDB`.

Acceptance criteria:
- Parallel path no longer depends on canonical `state.StateDB` for reads/writes.
- Snapshot/trie commit behavior remains correct.

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
type ObjectKind uint8

const (
    ObjBalance ObjectKind = iota + 1
    ObjNonce
    ObjCode
    ObjCodeHash
    ObjCodeSize
    ObjStorage
    ObjStateRoot
    ObjSuicided
    ObjExist
    ObjEmpty
)

type ObjectKey struct {
    Kind    ObjectKind
    Address common.Address
    Slot    common.Hash // only for ObjStorage
}

type ObjectVersion struct {
    LastWriterTx uint64
    IsBaseline   bool
}

type VersionedValue struct {
    Value   []byte        // canonical encoded value for key
    Version ObjectVersion // version of key at read time
}

type ReadSet map[ObjectKey]ObjectVersion
type WriteSet map[ObjectKey]WriteOp

type WriteOp struct {
    Delete bool
    Value  []byte // nil when Delete=true
}
```

### BlockState

`BlockState` is the canonical block-level state owner. It does not expose plain typed reads.

```go
type BlockState interface {
    StartPrefetcher(namespace string, opts ...PrefetcherOption)
    StopPrefetcher()
    Error() error

    // Generic versioned canonical read (planned for BlockState phase).
    Read(key ObjectKey) (VersionedValue, error)

    // Ordered deterministic apply path from tx write-set.
    ApplyWriteSet(txIndex uint64, ws WriteSet) error
    ValidateReadSet(rs ReadSet) bool

    // Block-level finalize and persistent commit.
    Finalise(deleteEmptyObjects bool)
    Commit(blockNumber uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error)
}
```

### TxnState

`TxnState` remains EVM-compatible with `Get*/Set*` style APIs.

Read semantics for every `Get*`:
1. check tx-local `WriteSet`
2. if absent, read from wrapped canonical state (`*state.StateDB` in phase 1,
   `BlockState.Read(ObjectKey)` in later phase)
3. decode value and return typed value

Write semantics for every `Set*`/delete:
1. encode and store in tx-local `WriteSet`
2. do not mutate wrapped canonical state until `Commit()`

```go
type TxnState interface {
    // Tx lifecycle
    TxHash() common.Hash
    TxIndex() int
    Commit() error
    Validate() bool

    // EVM compatibility surface (full vm.StateDB compatibility target)
    CreateAccount(addr common.Address)

    GetBalance(addr common.Address) *uint256.Int
    SetBalance(addr common.Address, value *uint256.Int)
    AddBalance(addr common.Address, amount *uint256.Int)
    SubBalance(addr common.Address, amount *uint256.Int)

    GetNonce(addr common.Address) uint64
    SetNonce(addr common.Address, nonce uint64)

    GetCode(addr common.Address) []byte
    SetCode(addr common.Address, code []byte)
    GetCodeHash(addr common.Address) common.Hash
    GetCodeSize(addr common.Address) int

    GetState(addr common.Address, slot common.Hash, opts ...stateconf.StateDBStateOption) common.Hash
    SetState(addr common.Address, slot common.Hash, value common.Hash, opts ...stateconf.StateDBStateOption)
    GetCommittedState(addr common.Address, slot common.Hash, opts ...stateconf.StateDBStateOption) common.Hash

    // Both names listed for design clarity; vm.StateDB currently uses Selfdestruct6780.
    SelfDestruct(addr common.Address)
    Selfdestruct6780(addr common.Address)
    SelfDestruct6780(addr common.Address) // optional alias for internal consistency
    HasSelfDestructed(addr common.Address) bool
    Exist(addr common.Address) bool
    Empty(addr common.Address) bool

    GetTransientState(addr common.Address, key common.Hash) common.Hash
    SetTransientState(addr common.Address, key, value common.Hash)

    AddressInAccessList(addr common.Address) bool
    SlotInAccessList(addr common.Address, slot common.Hash) (addressOk bool, slotOk bool)
    AddAddressToAccessList(addr common.Address)
    AddSlotToAccessList(addr common.Address, slot common.Hash)
    Prepare(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList)

    AddLog(log *types.Log)
    GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash) []*types.Log
    AddPreimage(hash common.Hash, preimage []byte)
    Preimages() map[common.Hash][]byte

    AddRefund(gas uint64)
    SubRefund(gas uint64)
    GetRefund() uint64
}
```

Notes:
1. `TxnState` intentionally has no trie `IntermediateRoot()` for parallel receipts.
2. Storage-level `Commit()` is block-level only via `BlockState.Commit(...)`.
3. Derived key coupling still applies on commit (`Code -> CodeHash/CodeSize`, `Storage -> StateRoot`).
4. When compiling against `vm.StateDB`, keep exact method names/signatures from `../libevm/core/vm/interface.go`.
5. `Snapshot()`/`RevertToSnapshot()` are intentionally omitted in this design; tx failure or conflict discards the current `TxnState` and schedules another `run` attempt with a fresh `TxnState`.
6. Tx context is immutable and initialized when creating `TxnState` (for example `NewTxnState(blockState, txHash, txIndex, ...)`); no `SetTxContext` mutator is needed.
7. Internal read/write sets are not exposed as public API. `Validate()` and `Commit()` are the only external coordination hooks required.

## Progress Log

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

3. Storage tracking model aligned with `stateObject` semantics:
- storage writes tracked as per-account slot map
- `storageWrites map[address]map[slot]value`
- supports multiple independent storage entries per account

4. Initial tests in `graft/coreth/core/txn_state_test.go`:
- read-own-write behavior for balance/nonce/code/state
- multiple storage-slot tracking on same account
- phase-1 validate behavior

Removed/replaced:

1. Removed `VersionedStateDB` path and related files from active implementation.
2. Replaced earlier generic storage scalar tracking with per-slot storage maps.

Deferred to next phase:

1. Block-level versioned validation in `BlockState`.
2. Parallel scheduler integration in `ProcessParallel(...)`.
3. Wiring `TxnState` into block processing path.

Environment note:

1. Local `go test` remains blocked by unrelated dependency/toolchain issues
(`zstd`, `blst`, `libevm/crypto` mismatch), so compile/test verification is incomplete.
