package parallel

import (
	"bytes"
	"fmt"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/ava-labs/libevm/params"
	"github.com/holiman/uint256"
)

// TxnState is a transaction-local state overlay with read/write tracking.
// Reads check local writes first, then fallback to wrapped canonical state.
type TxnState struct {
	// transaction identity
	base    BlockState
	txHash  common.Hash
	txIndex int
	nonce   uint32

	// read/write tracking for state objects
	writeSet *TxWriteSet
	readSet  *TxReadSet

	// Snapshots for transactional rollback during EVM execution.
	writeSetSnapshots []*txnWriteSetSnapshot
	writeSetDirty     bool

	// These never have conflicts with other transactions (write-only)
	// but we need to write them back to block state at the end of transaction execution.
	logs     []*types.Log
	preimage map[common.Hash][]byte

	// These are per-transaction
	refund     uint64
	transient  state.TransientStorage
	accessList *state.AccessList
}

type txnWriteSetSnapshot struct {
	writeSet   *TxWriteSet
	logs       []*types.Log
	preimage   map[common.Hash][]byte
	refund     uint64
	transient  state.TransientStorage
	accessList *state.AccessList
}

var _ vm.StateDB = (*TxnState)(nil)

func NewTxnState(base BlockState, txHash common.Hash, txIndex int, nonce uint32) *TxnState {
	return &TxnState{
		base:       base,
		txHash:     txHash,
		txIndex:    txIndex,
		nonce:      nonce,
		writeSet:   NewTxWriteSet(),
		readSet:    NewTxReadSet(),
		preimage:   make(map[common.Hash][]byte),
		transient:  state.NewTransientStorage(),
		accessList: state.NewAccessList(),
	}
}

func (t *TxnState) TxHash() common.Hash {
	return t.txHash
}

func (t *TxnState) TxIndex() int {
	return t.txIndex
}

func (t *TxnState) ReadSet() *TxReadSet {
	return t.readSet
}

func (t *TxnState) WriteSet() *TxWriteSet {
	return t.writeSet
}

// Validate is a placeholder for phase-1 direct StateDB wrapping.
func (t *TxnState) Validate() bool {
	return true
}

func (t *TxnState) CommitTxn() error {
	version := uint64(t.txIndex)<<32 | uint64(t.nonce) // so that we get different versions for different transactions and different executions of the same transaction
	if t.base == nil {
		return fmt.Errorf("nil base state")
	}
	if err := t.base.ApplyWriteSet(t.txIndex, version, t.writeSet); err != nil {
		return err
	}
	if err := t.base.AddLogs(t.txIndex, t.logs); err != nil {
		return err
	}
	if err := t.base.AddPreimages(t.txIndex, t.preimage); err != nil {
		return err
	}
	return nil
}

// This function is used for testing purposes
func (t *TxnState) Finalise(deleteEmptyObjects bool) {
	if err := t.CommitTxn(); err != nil {
		panic(err)
	}
	return
}

func (t *TxnState) CreateAccount(addr common.Address) {
	t.writeSet.CreateAccount(addr)
	t.writeSetDirty = true
}

func (t *TxnState) SetBalance(addr common.Address, value *uint256.Int) {
	t.write(BalanceKey(addr), NewBalanceValue(value))
}

func (t *TxnState) SubBalance(addr common.Address, amount *uint256.Int) {
	if amount == nil || amount.IsZero() {
		return
	}
	current := t.GetBalance(addr)
	next := new(uint256.Int).Sub(current, amount)
	t.SetBalance(addr, next)
}

func (t *TxnState) AddBalance(addr common.Address, amount *uint256.Int) {
	if amount == nil || amount.IsZero() {
		return
	}
	current := t.GetBalance(addr)
	next := new(uint256.Int).Add(current, amount)
	t.SetBalance(addr, next)
}

func (t *TxnState) GetBalance(addr common.Address) *uint256.Int {
	if value, err := t.read(BalanceKey(addr)); err == nil {
		if balance, ok := value.Balance(); ok {
			return balance
		}
	}
	return uint256.NewInt(0)
}

func (t *TxnState) GetNonce(addr common.Address) uint64 {
	if value, err := t.read(NonceKey(addr)); err == nil {
		if nonce, ok := value.Nonce(); ok {
			return nonce
		}
	}
	return 0
}

func (t *TxnState) SetNonce(addr common.Address, nonce uint64) {
	t.write(NonceKey(addr), NewNonceValue(nonce))
}

func (t *TxnState) GetCodeHash(addr common.Address) common.Hash {
	if value, err := t.read(CodeHashKey(addr)); err == nil {
		if codeHash, ok := value.CodeHash(); ok {
			return codeHash
		}
	}
	return common.Hash{}
}

func (t *TxnState) GetCode(addr common.Address) []byte {
	if value, err := t.read(CodeKey(addr)); err == nil {
		if code, ok := value.Code(); ok {
			return code
		}
	}
	return nil
}

func (t *TxnState) SetCode(addr common.Address, code []byte) {
	codeHash := crypto.Keccak256Hash(code)
	t.write(CodeKey(addr), NewCodeValue(code))
	t.write(CodeHashKey(addr), NewCodeHashValue(codeHash))
}

func (t *TxnState) GetCodeSize(addr common.Address) int {
	if value, err := t.read(CodeKey(addr)); err == nil {
		if code, ok := value.Code(); ok {
			return len(code)
		}
	}
	return 0
}

func (t *TxnState) AddRefund(gas uint64) {
	t.refund += gas
	t.writeSetDirty = true
}

func (t *TxnState) SubRefund(gas uint64) {
	if gas > t.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, t.refund))
	}
	t.refund -= gas
	t.writeSetDirty = true
}

func (t *TxnState) GetRefund() uint64 {
	return t.refund
}

func (t *TxnState) GetCommittedState(addr common.Address, state_key common.Hash, opt ...stateconf.StateDBStateOption) common.Hash {
	// Commited state refers to the state before transaction execution - we directly query blockstate for data.
	canonicalKey := state.TransformStateKey(addr, state_key, opt...)
	key := StorageKey(addr, canonicalKey)
	versionedValue, _ := t.readFromBase(key)
	if versionedValue == nil {
		return common.Hash{}
	}
	if storageValue, ok := versionedValue.Value.Storage(); ok {
		return storageValue
	}
	return common.Hash{}
}

func (t *TxnState) GetState(addr common.Address, key common.Hash, opt ...stateconf.StateDBStateOption) common.Hash {
	canonicalKey := state.TransformStateKey(addr, key, opt...)
	if value, err := t.read(StorageKey(addr, canonicalKey)); err == nil {
		if storageValue, ok := value.Storage(); ok {
			return storageValue
		}
	}
	return common.Hash{}
}

func (t *TxnState) SetState(addr common.Address, key, value common.Hash, opt ...stateconf.StateDBStateOption) {
	canonicalKey := state.TransformStateKey(addr, key, opt...)
	t.write(StorageKey(addr, canonicalKey), NewStorageValue(value))
}

// Transient state are local to transaction and not tracked in read/write sets.
func (t *TxnState) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	return t.transient.Get(addr, key)
}

func (t *TxnState) SetTransientState(addr common.Address, key, value common.Hash) {
	t.transient.Set(addr, key, value)
	t.writeSetDirty = true
}

func (t *TxnState) SelfDestruct(addr common.Address) {
	t.writeSet.DestructAccount(addr)
	t.writeSetDirty = true
}

// Only checks if an account is marked as self-destructed in the current transaction
// If the account is destructed in another transaction, it is marked deleted and will not be considered HasSelfDestructed
func (t *TxnState) HasSelfDestructed(addr common.Address) bool {
	if op, ok := t.writeSet.accountLifecycleChanges[addr]; ok {
		return op == lifecycleDestructed || op == lifecycleCreatedAndDestructed
	}
	return false // accounts that do not exist in the first place are not considered self-destructed
}

// call SelfDestruct only if the txn is created in the current transaction
func (t *TxnState) Selfdestruct6780(addr common.Address) {
	t.writeSet.DestructAccount6780(addr)
	t.writeSetDirty = true
}

func (t *TxnState) Exist(addr common.Address) bool {
	if op, ok := t.writeSet.accountLifecycleChanges[addr]; ok {
		if op == lifecycleCreated || op == lifecycleCreatedAndDestructed {
			return true
		}
		// a self-destructed account is considered existing until the end of block execution - we should query BlockState if the account exists
	}
	exists, version, err := t.base.Exists(addr)
	if err != nil {
		return false
	}
	if t.readSet.RecordAccountExistence(addr, version) != nil {
		// TODO: existence read inconsistent with previous read - early terminate
		return exists
	}
	return exists
}

func (t *TxnState) Empty(addr common.Address) bool {
	if !t.Exist(addr) {
		return true
	}
	// 1. check if the balance is zero
	if !t.GetBalance(addr).IsZero() {
		return false
	}
	// 2. check if the nonce is zero
	if t.GetNonce(addr) != 0 {
		return false
	}
	// 3. check if the code hash is empty
	if bytes.Equal(t.GetCodeHash(addr).Bytes(), types.EmptyCodeHash.Bytes()) {
		return false
	}
	// 4. check if extra is empty
	if extra := t.GetExtra(addr); extra != nil && !extra.IsZero() {
		return false
	}
	return true
}

// access list is per-transaction and not tracked in read/write sets.
func (t *TxnState) AddressInAccessList(addr common.Address) bool {
	return t.accessList.ContainsAddress(addr)
}

func (t *TxnState) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	return t.accessList.Contains(addr, slot)
}

func (t *TxnState) AddAddressToAccessList(addr common.Address) {
	if t.accessList.AddAddress(addr) {
		t.writeSetDirty = true
	}
}

func (t *TxnState) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	if addrAdded, slotAdded := t.accessList.AddSlot(addr, slot); addrAdded || slotAdded {
		t.writeSetDirty = true
	}
}

func (t *TxnState) Prepare(rules params.Rules, sender, coinbase common.Address, dst *common.Address, precompiles []common.Address, list types.AccessList) {
	if rules.IsBerlin {
		// Clear out any leftover from previous executions
		al := state.NewAccessList()
		t.accessList = al

		al.AddAddress(sender)
		if dst != nil {
			al.AddAddress(*dst)
			// If it's a create-tx, the destination will be added inside evm.create
		}
		for _, addr := range precompiles {
			al.AddAddress(addr)
		}
		for _, el := range list {
			al.AddAddress(el.Address)
			for _, key := range el.StorageKeys {
				al.AddSlot(el.Address, key)
			}
		}
		if rules.IsShanghai { // EIP-3651: warm coinbase
			al.AddAddress(coinbase)
		}
	}
	// Reset transient storage at the beginning of transaction execution
	t.transient = state.NewTransientStorage()
	t.writeSetDirty = true
}

func (t *TxnState) RevertToSnapshot(i int) {
	if i < 0 || i > len(t.writeSetSnapshots) {
		panic(fmt.Errorf("invalid snapshot id %d", i))
	}
	if i == 0 || t.writeSetSnapshots[i-1] == nil {
		t.writeSet = NewTxWriteSet()
		t.logs = nil
		t.preimage = make(map[common.Hash][]byte)
		t.refund = 0
		t.transient = state.NewTransientStorage()
		t.accessList = state.NewAccessList()
		t.writeSetDirty = false
		t.writeSetSnapshots = append(t.writeSetSnapshots, nil)
		return
	}
	snapshot := t.writeSetSnapshots[i-1]
	t.writeSet = snapshot.writeSet.Clone()
	t.logs = copyLogs(snapshot.logs)
	t.preimage = copyPreimages(snapshot.preimage)
	t.refund = snapshot.refund
	t.transient = snapshot.transient.Copy()
	t.accessList = snapshot.accessList.Copy()
	t.writeSetDirty = false
	t.writeSetSnapshots = append(t.writeSetSnapshots, snapshot)
}

func (t *TxnState) Snapshot() int {
	if t.writeSetDirty {
		t.writeSetSnapshots = append(t.writeSetSnapshots, t.captureWriteSetSnapshot())
	}
	t.writeSetDirty = false
	return len(t.writeSetSnapshots)
}

func (t *TxnState) AddLog(log *types.Log) {
	log.TxHash = t.txHash
	log.TxIndex = uint(t.txIndex)
	t.logs = append(t.logs, log)
	t.writeSetDirty = true
}

func (t *TxnState) AddPreimage(hash common.Hash, preimage []byte) {
	if _, exists := t.preimage[hash]; exists {
		return
	}
	copyPreimage := make([]byte, len(preimage))
	copy(copyPreimage, preimage)
	t.preimage[hash] = copyPreimage
	t.writeSetDirty = true
}

// called only when finalizing a transaction - and only on the txn itself
// TxnState can track its own logs and avoid writing back to BlockState - no synchronization needed
func (t *TxnState) GetLogs(hash common.Hash, blockNumber uint64, blockHash common.Hash) []*types.Log {
	if hash != t.txHash {
		return nil
	}
	out := make([]*types.Log, len(t.logs))
	for i, entry := range t.logs {
		entryCopy := *entry
		entryCopy.BlockNumber = blockNumber
		entryCopy.BlockHash = blockHash
		out[i] = &entryCopy
	}
	return out
}

// deligate to block state - this is used for testing purposes
func (t *TxnState) Logs() []*types.Log {
	return t.base.Logs()
}

// called only when writing a block back - so preimages won't have read-write conflicts.
func (t *TxnState) Preimages() map[common.Hash][]byte {
	out := make(map[common.Hash][]byte, len(t.preimage))
	for hash, image := range t.preimage {
		copyImage := make([]byte, len(image))
		copy(copyImage, image)
		out[hash] = copyImage
	}
	return out
}

func (t *TxnState) GetExtra(addr common.Address) *types.StateAccountExtra {
	if value, err := t.read(ExtraKey(addr)); err == nil {
		if extra, ok := value.Extra(); ok {
			return extra
		}
	}
	return nil
}

func (t *TxnState) SetExtra(addr common.Address, extra *types.StateAccountExtra) {
	t.write(ExtraKey(addr), NewExtraValue(extra))
}

// For testing purposes - in the common case, BlockState will write changes back to database
func (t *TxnState) Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error) {
	if err := t.CommitTxn(); err != nil {
		return common.Hash{}, err
	}
	return t.base.Commit(block, deleteEmptyObjects, opts...)
}

func (t *TxnState) readFromBase(key StateObjectKey) (*VersionedValue, error) {
	if t.base == nil {
		return nil, fmt.Errorf("base is nil")
	}
	vv, err := t.base.Read(key, uint64(t.txIndex))
	if err != nil {
		return nil, err
	}
	if oldVersion := t.readSet.RecordObjectVersion(key, vv.Version); oldVersion != nil {
		return vv, fmt.Errorf("read version mismatch for key %v: previous version %d, current version %d", key, *oldVersion, vv.Version)
	}
	return vv, nil
}

// TODO: we need to change StateDB interface so that we can propagate mismatching state reads up to the EVM execution for early termination.
// For now we just keep the first read version so that it will fail the validation check
func (t *TxnState) read(key StateObjectKey) (*StateObjectValue, error) {
	if !t.Exist(key.Address) {
		return nil, fmt.Errorf("account does not exist: %v", key.Address)
	}
	if value, ok := t.writeSet.Get(key); ok {
		return &value, nil
	}
	// if created in the current transaction, return empty and do not query block state
	if t.writeSet.IsCreated(key.Address) && key.Kind != StateObjectBalance {
		return &StateObjectValue{}, nil
	}
	versionedValue, err := t.readFromBase(key)
	return &versionedValue.Value, err
}

func (t *TxnState) write(key StateObjectKey, value StateObjectValue) {
	if !t.Exist(key.Address) {
		t.CreateAccount(key.Address)
	}
	t.writeSet.Set(key, value)
	t.writeSetDirty = true
}

func (t *TxnState) captureWriteSetSnapshot() *txnWriteSetSnapshot {
	return &txnWriteSetSnapshot{
		writeSet:   t.writeSet.Clone(),
		logs:       copyLogs(t.logs),
		preimage:   copyPreimages(t.preimage),
		refund:     t.refund,
		transient:  t.transient.Copy(),
		accessList: t.accessList.Copy(),
	}
}

func copyLogs(logs []*types.Log) []*types.Log {
	if len(logs) == 0 {
		return nil
	}
	out := make([]*types.Log, len(logs))
	for i, entry := range logs {
		entryCopy := *entry
		out[i] = &entryCopy
	}
	return out
}

func copyPreimages(preimages map[common.Hash][]byte) map[common.Hash][]byte {
	out := make(map[common.Hash][]byte, len(preimages))
	for hash, image := range preimages {
		copyImage := make([]byte, len(image))
		copy(copyImage, image)
		out[hash] = copyImage
	}
	return out
}
