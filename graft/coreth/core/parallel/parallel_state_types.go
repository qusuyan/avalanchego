package parallel

import (
	"fmt"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/holiman/uint256"
)

// StateObjectKind identifies tx-local write-set object granularity.
type StateObjectKind uint8

const (
	StateObjectBalance StateObjectKind = iota + 1
	StateObjectNonce
	StateObjectCodeHash
	StateObjectCode
	StateObjectStorage
	StateObjectExtra
)

// StateObjectKey identifies a single tracked object.
type StateObjectKey struct {
	Kind    StateObjectKind
	Address common.Address
	Slot    common.Hash // Only used for storage keys.
}

func BalanceKey(addr common.Address) StateObjectKey {
	return StateObjectKey{Kind: StateObjectBalance, Address: addr}
}

func NonceKey(addr common.Address) StateObjectKey {
	return StateObjectKey{Kind: StateObjectNonce, Address: addr}
}

func CodeKey(addr common.Address) StateObjectKey {
	return StateObjectKey{Kind: StateObjectCode, Address: addr}
}

func CodeHashKey(addr common.Address) StateObjectKey {
	return StateObjectKey{Kind: StateObjectCodeHash, Address: addr}
}

func StorageKey(addr common.Address, slot common.Hash) StateObjectKey {
	return StateObjectKey{Kind: StateObjectStorage, Address: addr, Slot: slot}
}

func ExtraKey(addr common.Address) StateObjectKey {
	return StateObjectKey{Kind: StateObjectExtra, Address: addr}
}

// StateObjectValue stores the new value written for a state object.
type StateObjectValue struct {
	balance  *uint256.Int
	nonce    *uint64
	codeHash *common.Hash
	code     []byte
	storage  *common.Hash
	extra    *types.StateAccountExtra
}

type ObjectVersion = uint64

const COMMITTED_VERSION ObjectVersion = 0
const ERROR_VERSION ObjectVersion = ^uint64(0) // Max uint64 to represent an error state.

type VersionedValue struct {
	Value   StateObjectValue
	Version ObjectVersion
}

func NewBalanceValue(value *uint256.Int) StateObjectValue {
	return StateObjectValue{balance: cloneU256(value)}
}

func NewNonceValue(value uint64) StateObjectValue {
	return StateObjectValue{nonce: &value}
}

func NewCodeHashValue(value common.Hash) StateObjectValue {
	v := value
	return StateObjectValue{codeHash: &v}
}

func NewCodeValue(value []byte) StateObjectValue {
	return StateObjectValue{code: cloneBytes(value)}
}

func NewStorageValue(value common.Hash) StateObjectValue {
	v := value
	return StateObjectValue{storage: &v}
}

func NewExtraValue(value *types.StateAccountExtra) StateObjectValue {
	return StateObjectValue{extra: value}
}

func (v StateObjectValue) Balance() (*uint256.Int, bool) {
	if v.balance == nil {
		return nil, false
	}
	return cloneU256(v.balance), true
}

func (v StateObjectValue) Nonce() (uint64, bool) {
	if v.nonce == nil {
		return 0, false
	}
	return *v.nonce, true
}

func (v StateObjectValue) CodeHash() (common.Hash, bool) {
	if v.codeHash == nil {
		return common.Hash{}, false
	}
	return *v.codeHash, true
}

func (v StateObjectValue) Code() ([]byte, bool) {
	if v.code == nil {
		return nil, false
	}
	return cloneBytes(v.code), true
}

func (v StateObjectValue) Storage() (common.Hash, bool) {
	if v.storage == nil {
		return common.Hash{}, false
	}
	return *v.storage, true
}

func (v StateObjectValue) Extra() (*types.StateAccountExtra, bool) {
	if v.extra == nil {
		return nil, false
	}
	return v.extra, true
}

type AccountLifecycle uint8

const (
	lifecycleCreated              AccountLifecycle = iota + 1 // exist: true, destructed: false
	lifecycleDestructed                                       // exist: unchange, destructed: true
	lifecycleCreatedAndDestructed                             // exist: true, destructed: true
	// Note: we don't need a lifecycle for DestructedAndCreated since an account destructed then created in the same transaction would
	// be the same as a created account except that its balance would be surely 0. In this case, the balance would be set to 0 during destruction,
	// and create will not change balance.
)

func (l AccountLifecycle) String() string {
	switch l {
	case lifecycleCreated:
		return "created"
	case lifecycleDestructed:
		return "destructed"
	case lifecycleCreatedAndDestructed:
		return "createdAndDestructed"
	default:
		return "unknown"
	}
}

// TxWriteSet captures keys mutated during a tx run and their new values.
type TxWriteSet struct {
	accountLifecycleChanges map[common.Address]AccountLifecycle
	writes                  map[StateObjectKey]StateObjectValue
}

func NewTxWriteSet() *TxWriteSet {
	return &TxWriteSet{
		accountLifecycleChanges: make(map[common.Address]AccountLifecycle),
		writes:                  make(map[StateObjectKey]StateObjectValue),
	}
}

// Add tracks that a key was written. Prefer Set for writes with value.
func (w *TxWriteSet) CreateAccount(addr common.Address) {
	fmt.Printf("createObject(%x)\n", addr)
	if w.accountLifecycleChanges == nil {
		w.accountLifecycleChanges = make(map[common.Address]AccountLifecycle)
	}
	w.accountLifecycleChanges[addr] = lifecycleCreated
	// remove all writes to the account - we start with a clean slate for the created account
	for key := range w.writes {
		// balance of the original account will be carried over
		// if the account was previously self-destructed, its balance would have been set to 0, so we don't need to remove the balance write
		if key.Address == addr && key.Kind != StateObjectBalance {
			delete(w.writes, key)
		}
	}
}

func (w *TxWriteSet) DestructAccount(addr common.Address) {
	if w.accountLifecycleChanges == nil {
		w.accountLifecycleChanges = make(map[common.Address]AccountLifecycle)
	}
	// Note: even if the account was created in the same transaction, we still want to mark it as self-destructed,
	// since CreateAccount can be called on an address with a non-empty account.
	if w.IsCreated(addr) {
		w.accountLifecycleChanges[addr] = lifecycleCreatedAndDestructed
	} else {
		w.accountLifecycleChanges[addr] = lifecycleDestructed
	}
	w.writes[BalanceKey(addr)] = StateObjectValue{balance: new(uint256.Int)}
}

func (w *TxWriteSet) DestructAccount6780(addr common.Address) {
	if w.accountLifecycleChanges == nil {
		w.accountLifecycleChanges = make(map[common.Address]AccountLifecycle)
	}
	// Only mark as self-destructed if the account was created in the same transaction.
	if lifecycle, exists := w.accountLifecycleChanges[addr]; exists && lifecycle == lifecycleCreated {
		w.DestructAccount(addr)
	}
}

func (w *TxWriteSet) IsCreated(addr common.Address) bool {
	if w.accountLifecycleChanges == nil {
		return false
	}
	lifecycle, exists := w.accountLifecycleChanges[addr]
	return exists && (lifecycle == lifecycleCreated || lifecycle == lifecycleCreatedAndDestructed)
}

func (w *TxWriteSet) Set(key StateObjectKey, value StateObjectValue) {
	if w.writes == nil {
		w.writes = make(map[StateObjectKey]StateObjectValue)
	}
	w.writes[key] = value
}

func (w *TxWriteSet) Get(key StateObjectKey) (StateObjectValue, bool) {
	value, ok := w.writes[key]
	return value, ok
}

func (w *TxWriteSet) Entries() map[StateObjectKey]StateObjectValue {
	out := make(map[StateObjectKey]StateObjectValue, len(w.writes))
	for key, value := range w.writes {
		out[key] = value
	}
	return out
}

func (w *TxWriteSet) Clone() *TxWriteSet {
	if w == nil {
		return NewTxWriteSet()
	}
	out := NewTxWriteSet()
	for addr, lifecycle := range w.accountLifecycleChanges {
		out.accountLifecycleChanges[addr] = lifecycle
	}
	for key, value := range w.writes {
		out.writes[key] = cloneStateObjectValue(value)
	}
	return out
}

type TxReadSet struct {
	accountExistsVersion map[common.Address]ObjectVersion
	objectVersions       map[StateObjectKey]ObjectVersion
}

func NewTxReadSet() *TxReadSet {
	return &TxReadSet{
		accountExistsVersion: make(map[common.Address]ObjectVersion),
		objectVersions:       make(map[StateObjectKey]ObjectVersion),
	}
}

// Insert the version for the given address and return the previous version if it exists and is different from the new version.
func (r *TxReadSet) RecordAccountExistence(addr common.Address, version ObjectVersion) *ObjectVersion {
	if r.accountExistsVersion == nil {
		r.accountExistsVersion = make(map[common.Address]ObjectVersion)
	}
	if prevVersion, exists := r.accountExistsVersion[addr]; exists {
		if prevVersion != version {
			return &prevVersion
		}
	} else {
		r.accountExistsVersion[addr] = version
	}
	return nil
}

// Insert the version for the given key and return the previous version if it exists and is different from the new version.
func (r *TxReadSet) RecordObjectVersion(key StateObjectKey, version ObjectVersion) *ObjectVersion {
	if r.objectVersions == nil {
		r.objectVersions = make(map[StateObjectKey]ObjectVersion)
	}
	if prevVersion, exists := r.objectVersions[key]; exists {
		if prevVersion != version {
			return &prevVersion
		}
	} else {
		r.objectVersions[key] = version
	}
	return nil
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	clone := make([]byte, len(value))
	copy(clone, value)
	return clone
}

func cloneU256(value *uint256.Int) *uint256.Int {
	if value == nil {
		return nil
	}
	return new(uint256.Int).Set(value)
}

func cloneStateObjectValue(value StateObjectValue) StateObjectValue {
	cloned := StateObjectValue{
		balance: cloneU256(value.balance),
		code:    cloneBytes(value.code),
		extra:   value.extra,
	}
	if value.nonce != nil {
		v := *value.nonce
		cloned.nonce = &v
	}
	if value.codeHash != nil {
		v := *value.codeHash
		cloned.codeHash = &v
	}
	if value.storage != nil {
		v := *value.storage
		cloned.storage = &v
	}
	return cloned
}
