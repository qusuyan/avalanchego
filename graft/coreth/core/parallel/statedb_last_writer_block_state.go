package parallel

import (
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/stateconf"
)

type ExistsState struct {
	Exists  bool
	Version ObjectVersion
}

type txLogs struct {
	entries []*types.Log
}

type txPreimages struct {
	entries map[common.Hash][]byte
}

type storageCacheKey struct {
	address common.Address
	slot    common.Hash
}

// StateDBLastWriterBlockState keeps canonical in-block changes in a
// last-writer-wins map and uses StateDB as baseline + final sink.
//
// Reads check the last-writer-wins map first, then fall back to StateDB.
// ApplyWriteSet merges tx writes with last-writer-wins semantics.
// Commit materializes the last-writer-wins map into StateDB, then delegates to StateDB.Commit.
type StateDBLastWriterBlockState struct {
	statedb *state.StateDB
	txHashs []common.Hash

	mu sync.RWMutex

	// Sticky shared error: first DB/state failure wins.
	dbErr error

	// Canonical last-writer-wins entries carry both value and version together.
	objectStates sync.Map // map[StateObjectKey]*atomic.Pointer[VersionedValue]
	existsStates sync.Map // map[common.Address]*atomic.Pointer[ExistsState]
	// Account and storage caches are used to avoid redundant loads from StateDB for commonly accessed accounts
	accountCache sync.Map // map[common.Address]*atomic.Pointer[state.ParallelAccountCache]
	storageCache sync.Map // map[storageCacheKey]*atomic.Pointer[common.Hash]

	readers []*state.ParallelReader

	// Side-effect tracking by tx index.
	logsByTx      []atomic.Pointer[txLogs]
	preimagesByTx []atomic.Pointer[txPreimages]
}

func NewStateDBLastWriterBlockState(statedb *state.StateDB, txHashs []common.Hash, workerCount int) *StateDBLastWriterBlockState {
	numTx := len(txHashs)
	if workerCount <= 0 {
		workerCount = 1
	}
	readers := make([]*state.ParallelReader, workerCount)
	readers[0] = state.NewParallelReader(statedb)
	for i := 1; i < len(readers); i++ {
		readers[i] = readers[0].Copy()
	}

	return &StateDBLastWriterBlockState{
		statedb:       statedb,
		txHashs:       txHashs,
		readers:       readers,
		logsByTx:      make([]atomic.Pointer[txLogs], numTx),
		preimagesByTx: make([]atomic.Pointer[txPreimages], numTx),
	}
}

func (b *StateDBLastWriterBlockState) loadExistsState(addr common.Address) *ExistsState {
	value, ok := b.existsStates.Load(addr)
	if !ok {
		return nil
	}
	ptr := value.(*atomic.Pointer[ExistsState])
	exists := ptr.Load()
	if exists == nil {
		return nil
	}
	return exists
}

func (b *StateDBLastWriterBlockState) loadObjectState(key StateObjectKey) *VersionedValue {
	value, ok := b.objectStates.Load(key)
	if !ok {
		return nil
	}
	ptr := value.(*atomic.Pointer[VersionedValue])
	object := ptr.Load()
	if object == nil {
		return nil
	}
	return object
}

func (b *StateDBLastWriterBlockState) loadAccountCache(addr common.Address) (*state.ParallelAccountCache, bool) {
	value, ok := b.accountCache.Load(addr)
	if !ok {
		return nil, false
	}
	ptr := value.(*atomic.Pointer[state.ParallelAccountCache])
	cache := ptr.Load()
	if cache == nil {
		return nil, false
	}
	return cache, true
}

func (b *StateDBLastWriterBlockState) loadStorageCache(addr common.Address, slot common.Hash) *common.Hash {
	value, ok := b.storageCache.Load(storageCacheKey{address: addr, slot: slot})
	if !ok {
		return nil
	}
	ptr := value.(*atomic.Pointer[common.Hash])
	return ptr.Load()
}

func (b *StateDBLastWriterBlockState) getOrCreateExistsPtr(addr common.Address) *atomic.Pointer[ExistsState] {
	ptr, _ := b.existsStates.LoadOrStore(addr, &atomic.Pointer[ExistsState]{})
	return ptr.(*atomic.Pointer[ExistsState])
}

func (b *StateDBLastWriterBlockState) getOrCreateAccountPtr(addr common.Address) *atomic.Pointer[state.ParallelAccountCache] {
	ptr, _ := b.accountCache.LoadOrStore(addr, &atomic.Pointer[state.ParallelAccountCache]{})
	return ptr.(*atomic.Pointer[state.ParallelAccountCache])
}

func (b *StateDBLastWriterBlockState) getOrCreateObjectPtr(key StateObjectKey) *atomic.Pointer[VersionedValue] {
	ptr, _ := b.objectStates.LoadOrStore(key, &atomic.Pointer[VersionedValue]{})
	return ptr.(*atomic.Pointer[VersionedValue])
}

func (b *StateDBLastWriterBlockState) getOrCreateStoragePtr(addr common.Address, slot common.Hash) *atomic.Pointer[common.Hash] {
	ptr, _ := b.storageCache.LoadOrStore(storageCacheKey{address: addr, slot: slot}, &atomic.Pointer[common.Hash]{})
	return ptr.(*atomic.Pointer[common.Hash])
}

func (b *StateDBLastWriterBlockState) storeExistsLWW(addr common.Address, next ExistsState) bool {
	ptr := b.getOrCreateExistsPtr(addr)
	for {
		curr := ptr.Load()
		if curr != nil && curr.Version > next.Version {
			return false
		}
		if ptr.CompareAndSwap(curr, &next) {
			return true
		}
	}
}

func (b *StateDBLastWriterBlockState) storeObjectLWW(key StateObjectKey, next VersionedValue) bool {
	ptr := b.getOrCreateObjectPtr(key)
	for {
		curr := ptr.Load()
		if curr != nil && curr.Version > next.Version {
			return false
		}
		if ptr.CompareAndSwap(curr, &next) {
			return true
		}
	}
}

func (b *StateDBLastWriterBlockState) clearAddressObjectsUpToVersion(addr common.Address, version ObjectVersion, keepBalance bool) {
	b.objectStates.Range(func(k, v any) bool {
		key := k.(StateObjectKey)
		if key.Address != addr {
			return true
		}
		if keepBalance && key.Kind == StateObjectBalance {
			return true
		}
		ptr := v.(*atomic.Pointer[VersionedValue])
		for {
			curr := ptr.Load()
			if curr == nil || curr.Version > version {
				return true
			}
			if ptr.CompareAndSwap(curr, nil) {
				return true
			}
		}
	})
}

func (b *StateDBLastWriterBlockState) setError(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.dbErr == nil {
		b.dbErr = err
	}
}

func (b *StateDBLastWriterBlockState) Error() error {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dbErr
}

func (b *StateDBLastWriterBlockState) readerIndexForWorker(workerID int) int {
	if len(b.readers) == 0 {
		return 0
	}
	if workerID < 0 {
		workerID = -workerID
	}
	return workerID % len(b.readers)
}

func (b *StateDBLastWriterBlockState) getOrLoadAccountCache(addr common.Address, workerID int) (*state.ParallelAccountCache, error) {
	if cache, ok := b.loadAccountCache(addr); ok {
		return cache, nil
	}
	if len(b.readers) == 0 {
		return nil, fmt.Errorf("nil baseline reader")
	}
	readerIndex := b.readerIndexForWorker(workerID)
	if cache, ok := b.loadAccountCache(addr); ok {
		return cache, nil
	}
	cache, err := b.readers[readerIndex].GetAccount(addr)
	if err != nil {
		return nil, err
	}
	b.getOrCreateAccountPtr(addr).Store(cache)
	return cache, nil
}

func (b *StateDBLastWriterBlockState) getOrLoadStorage(addr common.Address, slot common.Hash, workerID int) (common.Hash, error) {
	if cache := b.loadStorageCache(addr, slot); cache != nil {
		return *cache, nil
	}
	if len(b.readers) == 0 {
		return common.Hash{}, fmt.Errorf("nil baseline reader")
	}
	readerIndex := b.readerIndexForWorker(workerID)
	if cache := b.loadStorageCache(addr, slot); cache != nil {
		return *cache, nil
	}
	value, err := b.readers[readerIndex].GetState(addr, slot, stateconf.SkipStateKeyTransformation())
	if err != nil {
		return common.Hash{}, err
	}
	valueCopy := value
	b.getOrCreateStoragePtr(addr, slot).Store(&valueCopy)
	return value, nil
}

func (b *StateDBLastWriterBlockState) Exists(addr common.Address, workerID int) (bool, ObjectVersion, error) {
	if entry := b.loadExistsState(addr); entry != nil {
		return entry.Exists, entry.Version, nil
	}
	cache, err := b.getOrLoadAccountCache(addr, workerID)
	if err != nil {
		return false, ERROR_VERSION, err
	}
	return cache.Exists, COMMITTED_VERSION, nil
}

func (b *StateDBLastWriterBlockState) Read(key StateObjectKey, workerID int) (*VersionedValue, error) {
	exists, version, err := b.Exists(key.Address, workerID)
	if err != nil {
		return nil, fmt.Errorf("failed to check existence for address %s: %w", key.Address.Hex(), err)
	}
	if !exists {
		return &VersionedValue{Value: StateObjectValue{}, Version: version}, nil
	}

	if entry := b.loadObjectState(key); entry != nil {
		return entry, nil
	}

	cache, err := b.getOrLoadAccountCache(key.Address, workerID)
	if err != nil {
		return nil, err
	}

	switch key.Kind {
	case StateObjectBalance:
		return &VersionedValue{Value: NewBalanceValue(cache.Data.Balance), Version: COMMITTED_VERSION}, nil
	case StateObjectNonce:
		return &VersionedValue{Value: NewNonceValue(cache.Data.Nonce), Version: COMMITTED_VERSION}, nil
	case StateObjectCodeHash:
		return &VersionedValue{Value: NewCodeHashValue(common.BytesToHash(cache.Data.CodeHash)), Version: COMMITTED_VERSION}, nil
	case StateObjectCode:
		return &VersionedValue{Value: NewCodeValue(cache.Code), Version: COMMITTED_VERSION}, nil
	case StateObjectStorage:
		value, err := b.getOrLoadStorage(key.Address, key.Slot, workerID)
		if err != nil {
			return nil, err
		}
		return &VersionedValue{Value: NewStorageValue(value), Version: COMMITTED_VERSION}, nil
	case StateObjectExtra:
		return &VersionedValue{Value: NewExtraValue(cache.Data.Extra), Version: COMMITTED_VERSION}, nil
	default:
		return nil, fmt.Errorf("unknown state object kind: %d", key.Kind)
	}
}

func (b *StateDBLastWriterBlockState) Logs() []*types.Log {
	total := 0
	for i := range b.logsByTx {
		logs := b.logsByTx[i].Load()
		if logs != nil {
			total += len(logs.entries)
		}
	}
	out := make([]*types.Log, 0, total)
	for i := range b.logsByTx {
		logs := b.logsByTx[i].Load()
		if logs == nil {
			continue
		}
		for _, entry := range logs.entries {
			if entry == nil {
				continue
			}
			out = append(out, entry)
		}
	}
	return out
}

func (b *StateDBLastWriterBlockState) ApplyWriteSet(_ int, version ObjectVersion, ws *TxWriteSet) error {
	if ws == nil {
		return nil
	}

	// Resolve existence/lifecycle first.
	for addr, lifecycle := range ws.accountLifecycleChanges {
		switch lifecycle {
		case lifecycleCreated:
			if !b.storeExistsLWW(addr, ExistsState{Exists: true, Version: version}) {
				// A newer version already exists; skip stale lifecycle.
				continue
			}
			// Creation starts a fresh object epoch for the account.
			// Keep balance to match TxWriteSet.CreateAccount semantics.
			b.clearAddressObjectsUpToVersion(addr, version, true)
		case lifecycleDestructed | lifecycleCreatedAndDestructed: // when a txn completes, destructed accounts no longer exists.
			if !b.storeExistsLWW(addr, ExistsState{Exists: false, Version: version}) {
				// A newer version already exists; skip stale lifecycle.
				continue
			}
		}
	}

	// Resolve object writes next.
	for key, write := range ws.Entries() {
		// If this account is destructed at same or newer version, ignore
		// non-balance writes from this merge.
		if exists := b.loadExistsState(key.Address); exists != nil && !exists.Exists && exists.Version >= version {
			if key.Kind != StateObjectBalance {
				continue
			}
		}
		if !b.storeObjectLWW(key, VersionedValue{Value: write, Version: version}) {
			// A newer object version already exists; skip stale write.
			continue
		}
	}

	return nil
}

func (b *StateDBLastWriterBlockState) AddLogs(txIndex int, logs []*types.Log) error {
	if txIndex < 0 || txIndex >= len(b.logsByTx) {
		return fmt.Errorf("tx index %d out of range for logs", txIndex)
	}
	b.logsByTx[txIndex].Store(&txLogs{entries: logs})
	return nil
}

func (b *StateDBLastWriterBlockState) AddPreimages(txIndex int, preimages map[common.Hash][]byte) error {
	if txIndex < 0 || txIndex >= len(b.preimagesByTx) {
		return fmt.Errorf("tx index %d out of range for preimages", txIndex)
	}
	b.preimagesByTx[txIndex].Store(&txPreimages{entries: preimages})
	return nil
}

func (b *StateDBLastWriterBlockState) ValidateReadSet(rs *TxReadSet) bool {
	if rs == nil {
		return true
	}

	for addr, observedVersion := range rs.accountExistsVersion {
		_, currentVersion, err := b.Exists(addr, 0)
		if err != nil {
			b.setError(err)
			return false
		}
		if currentVersion != observedVersion {
			return false
		}
	}

	for key, observedVersion := range rs.objectVersions {
		current, err := b.Read(key, 0)
		if err != nil {
			b.setError(err)
			return false
		}
		if current == nil || current.Version != observedVersion {
			return false
		}
	}

	return true
}

// We can assume that when Commit is called, transactions have been fully applied and no more concurrent writes are happening, so we can safely materialize the last-writer-wins map into the underlying StateDB without additional synchronization.
func (b *StateDBLastWriterBlockState) WriteBack() error {
	if b.statedb == nil {
		return fmt.Errorf("nil base state")
	}
	if err := b.Error(); err != nil {
		return fmt.Errorf("commit aborted due to earlier error: %w", err)
	}
	if err := b.preloadCanonicalState(); err != nil {
		return err
	}

	// Apply existence lifecycle state first.
	b.existsStates.Range(func(k, v any) bool {
		addr := k.(common.Address)
		ptr := v.(*atomic.Pointer[ExistsState])
		value := ptr.Load()
		if value == nil {
			return true
		}
		if value.Exists {
			b.statedb.CreateAccount(addr)
		} else {
			b.statedb.SelfDestruct(addr)
		}
		return true
	})

	// Apply object last-writer-wins state.
	b.objectStates.Range(func(k, v any) bool {
		key := k.(StateObjectKey)
		ptr := v.(*atomic.Pointer[VersionedValue])
		objectState := ptr.Load()
		if objectState == nil {
			return true
		}
		switch key.Kind {
		case StateObjectBalance:
			if balance, ok := objectState.Value.Balance(); ok {
				// We need this special handling for balance writes since we cleared writes for others in ApplyWriteSet
				if exists := b.loadExistsState(key.Address); exists != nil && !exists.Exists {
					// Account is destructed in this block - skip its balance
				} else {
					b.statedb.SetBalance(key.Address, balance)
				}
			}
		case StateObjectNonce:
			if nonce, ok := objectState.Value.Nonce(); ok {
				b.statedb.SetNonce(key.Address, nonce)
			}
		case StateObjectCode:
			if code, ok := objectState.Value.Code(); ok {
				b.statedb.SetCode(key.Address, code)
			}
		case StateObjectStorage:
			if storageValue, ok := objectState.Value.Storage(); ok {
				b.statedb.SetState(key.Address, key.Slot, storageValue, stateconf.SkipStateKeyTransformation())
			}
		case StateObjectExtra:
			if extra, ok := objectState.Value.Extra(); ok {
				b.statedb.SetExtra(key.Address, extra)
			}
		}
		return true
	})

	// Apply logs and preimages in tx-index order.
	for i := range b.logsByTx {
		b.statedb.SetTxContext(b.txHashs[i], i)
		logs := b.logsByTx[i].Load()
		if logs != nil {
			for _, entry := range logs.entries {
				if entry != nil {
					b.statedb.AddLog(entry)
				}
			}
		}
	}

	for i := range b.preimagesByTx {
		preimages := b.preimagesByTx[i].Load()
		if preimages != nil {
			for hash, image := range preimages.entries {
				b.statedb.AddPreimage(hash, image)
			}
		}
	}

	b.statedb.SetError(b.dbErr)
	b.statedb.Finalise(true)
	return nil
}

func (b *StateDBLastWriterBlockState) preloadCanonicalState() error {
	addresses := make(map[common.Address]struct{})
	b.existsStates.Range(func(k, _ any) bool {
		addresses[k.(common.Address)] = struct{}{}
		return true
	})
	b.objectStates.Range(func(k, _ any) bool {
		addresses[k.(StateObjectKey).Address] = struct{}{}
		return true
	})
	for addr := range addresses {
		cache, ok := b.loadAccountCache(addr)
		if !ok || cache == nil || !cache.Exists {
			continue
		}
		preload := cache.Clone()
		b.storageCache.Range(func(k, v any) bool {
			cacheKey := k.(storageCacheKey)
			if cacheKey.address != addr {
				return true
			}
			value := v.(*atomic.Pointer[common.Hash]).Load()
			if value == nil {
				return true
			}
			if preload.Storage == nil {
				preload.Storage = make(map[common.Hash]common.Hash)
			}
			preload.Storage[cacheKey.slot] = *value
			return true
		})
		if err := b.statedb.PreloadParallelAccount(preload); err != nil {
			return err
		}
	}
	return nil
}

func (b *StateDBLastWriterBlockState) Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error) {
	if err := b.WriteBack(); err != nil {
		return common.Hash{}, fmt.Errorf("failed to write back block state: %w", err)
	}
	return b.statedb.Commit(block, deleteEmptyObjects, opts...)
}

var _ BlockState = (*StateDBLastWriterBlockState)(nil)
