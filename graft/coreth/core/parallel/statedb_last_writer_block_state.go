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

	// Side-effect tracking by tx index.
	logsByTx      []atomic.Pointer[txLogs]
	preimagesByTx []atomic.Pointer[txPreimages]
}

func NewStateDBLastWriterBlockState(statedb *state.StateDB, txHashs []common.Hash) *StateDBLastWriterBlockState {
	numTx := len(txHashs)

	return &StateDBLastWriterBlockState{
		statedb:       statedb,
		txHashs:       txHashs,
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

func (b *StateDBLastWriterBlockState) getOrCreateExistsPtr(addr common.Address) *atomic.Pointer[ExistsState] {
	ptr, _ := b.existsStates.LoadOrStore(addr, &atomic.Pointer[ExistsState]{})
	return ptr.(*atomic.Pointer[ExistsState])
}

func (b *StateDBLastWriterBlockState) getOrCreateObjectPtr(key StateObjectKey) *atomic.Pointer[VersionedValue] {
	ptr, _ := b.objectStates.LoadOrStore(key, &atomic.Pointer[VersionedValue]{})
	return ptr.(*atomic.Pointer[VersionedValue])
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

func (b *StateDBLastWriterBlockState) Exists(addr common.Address) (bool, ObjectVersion, error) {
	if entry := b.loadExistsState(addr); entry != nil {
		return entry.Exists, entry.Version, nil
	}
	if b.statedb == nil {
		return false, ERROR_VERSION, fmt.Errorf("nil base state")
	}
	return b.statedb.Exist(addr), COMMITTED_VERSION, nil
}

func (b *StateDBLastWriterBlockState) Read(key StateObjectKey, _ uint64) (*VersionedValue, error) {
	exists, version, err := b.Exists(key.Address)
	if err != nil {
		return nil, fmt.Errorf("failed to check existence for address %s: %w", key.Address.Hex(), err)
	}
	if !exists {
		return &VersionedValue{Value: StateObjectValue{}, Version: version}, nil
	}

	if entry := b.loadObjectState(key); entry != nil {
		return entry, nil
	}

	if b.statedb == nil {
		return nil, fmt.Errorf("nil base state")
	}

	switch key.Kind {
	case StateObjectBalance:
		return &VersionedValue{Value: NewBalanceValue(b.statedb.GetBalance(key.Address)), Version: COMMITTED_VERSION}, nil
	case StateObjectNonce:
		return &VersionedValue{Value: NewNonceValue(b.statedb.GetNonce(key.Address)), Version: COMMITTED_VERSION}, nil
	case StateObjectCodeHash:
		return &VersionedValue{Value: NewCodeHashValue(b.statedb.GetCodeHash(key.Address)), Version: COMMITTED_VERSION}, nil
	case StateObjectCode:
		return &VersionedValue{Value: NewCodeValue(b.statedb.GetCode(key.Address)), Version: COMMITTED_VERSION}, nil
	case StateObjectStorage:
		return &VersionedValue{Value: NewStorageValue(b.statedb.GetState(key.Address, key.Slot, stateconf.SkipStateKeyTransformation())), Version: COMMITTED_VERSION}, nil
	case StateObjectExtra:
		return &VersionedValue{Value: NewExtraValue(b.statedb.GetExtra(key.Address)), Version: COMMITTED_VERSION}, nil
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
	fmt.Printf("ApplyWriteSet for version %d\n", version)
	if ws == nil {
		return nil
	}

	// Resolve existence/lifecycle first.
	for addr, lifecycle := range ws.accountLifecycleChanges {
		fmt.Printf("\tLifecycle: %x, %s\n", addr, lifecycle.String())
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
		// If this account is destructed at same or newer version, ignore non-balance writes from this merge.
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

func (b *StateDBLastWriterBlockState) ValidateReadSet(_ *TxReadSet) bool {
	// TODO:
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

	b.existsStates = sync.Map{}                                                 // clear existence states after materialization
	b.objectStates = sync.Map{}                                                 // clear object states after materialization
	b.logsByTx = make([]atomic.Pointer[txLogs], len(b.logsByTx))                // clear logs after materialization
	b.preimagesByTx = make([]atomic.Pointer[txPreimages], len(b.preimagesByTx)) // clear preimages after materialization
	b.dbErr = nil                                                               // clear error after materialization

	return nil
}

func (b *StateDBLastWriterBlockState) Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error) {
	if err := b.WriteBack(); err != nil {
		return common.Hash{}, fmt.Errorf("failed to write back block state: %w", err)
	}
	return b.statedb.Commit(block, deleteEmptyObjects, opts...)
}

var _ BlockState = (*StateDBLastWriterBlockState)(nil)
