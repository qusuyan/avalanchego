# Parallel Tx Reader And Cache Rehydration Plan

Last updated: 2026-02-28

## Current Status

Implemented on this branch:

- `libevm/core/state/parallel_reader.go`
  - `ParallelReader`
  - `ParallelAccountCache`
  - `StateDB.PreloadParallelAccount`
- `StateDBLastWriterBlockState` shared account/storage cache
- one `ParallelReader` per configured worker
- `TxnState.workerID`
- `BlockState.Exists(..., workerID)` and `BlockState.Read(..., workerID)`
- conservative canonical cache rehydration before `WriteBack()`
- focused reader/cache/rehydration tests, including `-race`

Current execution wiring status:

- `core/state_processor.go` constructs `StateDBLastWriterBlockState` with the configured worker count
- `TxnState` is currently created with `workerID = 0`
- this means the reader/cache path is ready for end-to-end testing in the current single-worker execution path
- true multi-worker execution still needs executor wiring to pass the real worker ID during scheduling

## Goal

Enable safe parallel baseline reads during speculative transaction execution while preserving enough account and storage locality to avoid rereading the same objects during canonical writeback.

This plan is intentionally conservative:

- do not share live `*stateObject` instances across workers
- do not rewrite `StateDB` internals around `sync.Map`
- do not change commit semantics in the first pass

Instead, the design introduces:

- a worker-local parallel baseline reader in `libevm/core/state`
- a shared cache owned by `BlockState`
- a canonical `StateDB` preload path before `WriteBack()`

## High-Level Design

1. Add a baseline reader in `libevm/core/state` that reads committed account/storage state without touching `StateDB` live caches.
2. Give each speculative worker its own reader instance.
3. Keep a shared immutable-ish read cache in `StateDBLastWriterBlockState`.
4. Route reads through:
   - LWW committed overlay
   - shared read cache
   - worker-local baseline reader
5. Before canonical `WriteBack()`, preload canonical `StateDB` from the shared cache for addresses that will be written.
6. Let existing write paths (`SetBalance`, `SetState`, `SetCode`, etc.) reuse those canonical-owned cached objects instead of rereading from storage.

## Non-Goals For Phase 1

- sharing live `stateObject` pointers across workers
- bulk replacing `StateDB` maps with concurrent maps
- transplanting worker-local trie handles into canonical `StateDB`
- changing trie/state commit logic

## File-By-File Plan

This section now reflects the implemented shape on this branch, plus the remaining work.

### 1. `libevm/core/state`

#### New file: `parallel_reader.go`

Implemented: a new baseline reader type that reads committed state without touching `StateDB.stateObjects`.

Implemented types:

```go
type ParallelAccountCache struct {
    Address common.Address
    Exists  bool
    Data    *types.StateAccount
    Code    []byte
    Storage map[common.Hash]common.Hash
}

type ParallelReader struct {
    db           Database
    trie         Trie
    snaps        SnapshotTree
    snap         snapshot.Snapshot
    originalRoot common.Hash
    hasher       crypto.KeccakState
}
```

Implemented constructor:

```go
func NewParallelReader(base *StateDB) *ParallelReader
```

Implemented methods:

```go
func (r *ParallelReader) Copy() *ParallelReader
func (r *ParallelReader) GetAccount(addr common.Address) (*ParallelAccountCache, error)
func (r *ParallelReader) GetState(addr common.Address, slot common.Hash, opts ...stateconf.StateDBStateOption) (common.Hash, error)
```

Implemented rules:

- Do not touch `StateDB.stateObjects`.
- Do not call `StateDB.getDeletedStateObject`.
- Use snapshot first if available.
- Fall back to direct trie reads otherwise.
- Use a reader-local hasher, never `StateDB.hasher`.
- `Copy()` should keep cost low:
  - share `db`, `snaps`, `snap`
  - copy the trie handle with `db.CopyTrie(...)`
  - allocate a fresh hasher

Implemented helper methods:

```go
func (r *ParallelReader) getAccountFromSnapshot(addr common.Address) (*types.StateAccount, bool, error)
func (r *ParallelReader) getAccountFromTrie(addr common.Address) (*types.StateAccount, bool, error)
func (r *ParallelReader) getCode(addr common.Address, codeHash common.Hash) ([]byte, error)
func (r *ParallelReader) getStorage(addr common.Address, root common.Hash, slot common.Hash) (common.Hash, error)
```

#### Existing file: `statedb.go`

Implemented canonical preload hook so `WriteBack()` can hydrate canonical-owned cached objects before mutating them.

Implemented addition:

```go
func (s *StateDB) PreloadParallelAccount(cache *ParallelAccountCache) error
func (s *StateDB) PreloadParallelAccounts(caches map[common.Address]*ParallelAccountCache) error
```

Implementation rules:

- Create or fetch a canonical-owned `stateObject`.
- Populate only safe cache state:
  - account existence/data
  - code bytes
  - committed storage cache
- Do not import:
  - `dirtyStorage`
  - `pendingStorage`
  - worker-local trie handles
  - speculative lifecycle state

`state_object.go` did not need a separate preload helper in this pass. The canonical preload is handled in `StateDB.PreloadParallelAccount`.

### 2. `avalanchego/graft/coreth/core/parallel`

#### Existing file: `statedb_last_writer_block_state.go`

Implemented: `StateDBLastWriterBlockState` owns shared read caches and worker reader creation.

Implemented fields:

```go
accountCache sync.Map // map[common.Address]*atomic.Pointer[state.ParallelAccountCache]
readers      []*state.ParallelReader
```

Implemented constructor changes:

- Initialize one reader per worker, or support lazy worker-reader creation.
- The reader set should be based on the post-upgrade, post-beacon canonical `statedb`.

Implemented helper direction:

- `getOrLoadAccountCache(addr, workerID)`
- `getOrLoadStorage(addr, slot, workerID)`
- `preloadCanonicalState()`

Implemented read-path behavior:

- `Exists` and account-level `Read`:
  - check LWW overlay first
  - check shared account cache second
  - consult worker-local reader on miss
  - publish result into shared cache

- storage reads:
  - use shared LWW overlay first
  - then worker-local reader
  - optionally record the slot into the shared account cache's committed storage map

Implemented writeback changes:

- Before mutating canonical `StateDB`, call `preloadCanonicalState()`.
- Only preload addresses that actually appear in:
  - `existsStates`
  - `objectStates`

Current implementation rule:

- No concurrent writes are expected during `WriteBack()`, so preload can run under the same canonical-state lock currently used for writeback.

#### Existing file: `block_state.go`

Implemented change:

- `BlockState` now takes `workerID` on `Exists` and `Read`
- worker-bound wrapper views were not added in this pass

#### Existing file: `txn_state.go`

Implemented changes:

- `TxnState` now carries `workerID`
- `TxnState` passes `workerID` through baseline reads
- read-set logic is otherwise unchanged

### 3. `avalanchego/graft/coreth/core`

#### Existing file: `state_processor_blockexecutor_driver.go`

Not wired on this branch. True multi-worker executor integration is still pending.

Support method in driver:

```go
func (d *stateProcessorBlockExecutorDriver) blockStateForWorker(ctx context.Context) parallel.BlockState
```

#### Existing file: `state_processor.go`

Implemented status:

- `blockState := parallel.NewStateDBLastWriterBlockState(statedb, txHashes, cfg.ParallelExecutionWorkers)` still happens after upgrade/beacon mutations
- worker reader construction therefore sees the correct post-upgrade baseline
- `TxnState` is currently created with `workerID = 0` in this file

### 4. Tests

#### Tests under `libevm/core/state`

Implemented tests for:

- `ParallelReader.GetAccount` matches `StateDB` baseline reads
- `ParallelReader.GetState` matches `StateDB.GetState`
- snapshot-backed and trie-backed cases
- `Copy()` creates independent reader-local mutable state

Implemented file:

- `libevm/core/state/parallel_reader_test.go`

#### Tests under `avalanchego/graft/coreth/core/parallel`

Implemented tests for:

- multiple worker views reading same account populate one shared cache entry
- storage values loaded by speculative reads are available to canonical preload
- `WriteBack()` after preload avoids semantic regressions
- account create/destruct paths do not preload invalid stale cache state

Implemented file:

- `avalanchego/graft/coreth/core/parallel/parallel_state_test.go`

#### Existing parallel execution tests

Still useful to add:

- conflict-free block with repeated reads to same account across different txs
- block with post-beacon baseline and parallel speculative reads
- block on precompile activation boundary if a configurable module is present

## Suggested Implementation Order

1. Run end-to-end testing against the current single-worker path.
2. Wire the real executor worker ID into `TxnState`.
3. Add integration coverage once the executor path is moved over.
4. Benchmark before deciding whether shared slot cache or more aggressive `stateObject` changes are needed.

## Performance Expectations

Expected gains from this phase:

- safe parallel baseline reads
- shared account-level locality across workers
- reduced rereads during canonical writeback

Expected limitations:

- storage-slot reuse will still be weaker than a fully shared live `stateObject` design
- some canonical preload copying cost remains
- worker-local reader caches are still duplicated where shared cache misses occur

## Escalation Path If This Is Not Enough

If phase 1 performance is insufficient, next options are:

1. add a dedicated shared `(address, slot)` storage cache
2. add a more aggressive canonical auxiliary cache consulted directly by `StateDB`
3. evaluate whether a state-object-shaped shared cache can be introduced without sharing live `*stateObject` ownership

These are intentionally deferred until after measurements.
