// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package core

import (
	"fmt"
	"maps"

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
	base    *state.StateDB
	txHash  common.Hash
	txIndex int

	writeSet *TxWriteSet
	readSet  map[StateObjectKey]struct{}

	refund uint64

	lifecycle map[common.Address]accountLifecycle

	logs     []*types.Log
	logSize  uint
	preimage map[common.Hash][]byte

	transient map[common.Address]map[common.Hash]common.Hash
	accessA   map[common.Address]struct{}
	accessS   map[common.Address]map[common.Hash]struct{}
}

type accountLifecycle uint8

const (
	lifecycleCreated accountLifecycle = iota + 1
	lifecycleSelfDestructLegacy
	lifecycleSelfDestructEIP6780
)

var _ vm.StateDB = (*TxnState)(nil)

func NewTxnState(base *state.StateDB, txHash common.Hash, txIndex int) *TxnState {
	return &TxnState{
		base:      base,
		txHash:    txHash,
		txIndex:   txIndex,
		writeSet:  NewTxWriteSet(),
		readSet:   make(map[StateObjectKey]struct{}),
		lifecycle: make(map[common.Address]accountLifecycle),
		preimage:  make(map[common.Hash][]byte),
		transient: make(map[common.Address]map[common.Hash]common.Hash),
		accessA:   make(map[common.Address]struct{}),
		accessS:   make(map[common.Address]map[common.Hash]struct{}),
	}
}

func (t *TxnState) TxHash() common.Hash {
	return t.txHash
}

func (t *TxnState) TxIndex() int {
	return t.txIndex
}

// Validate is a placeholder for phase-1 direct StateDB wrapping.
func (t *TxnState) Validate() bool {
	return true
}

func (t *TxnState) Commit() error {
	if t.base == nil {
		return fmt.Errorf("nil base state")
	}

	// Apply account lifecycle with last-op-wins semantics per address.
	for addr, op := range t.lifecycle {
		switch op {
		case lifecycleCreated:
			t.base.CreateAccount(addr)
		case lifecycleSelfDestructEIP6780:
			t.base.Selfdestruct6780(addr)
		case lifecycleSelfDestructLegacy:
			t.base.SelfDestruct(addr)
		}
	}

	// Apply typed writes to canonical state.
	for key, value := range t.writeSet.Entries() {
		switch key.Kind {
		case StateObjectBalance:
			if balance, ok := value.Balance(); ok {
				t.base.SetBalance(key.Address, balance)
			}
		case StateObjectNonce:
			if nonce, ok := value.Nonce(); ok {
				t.base.SetNonce(key.Address, nonce)
			}
		case StateObjectCode:
			if code, ok := value.Code(); ok {
				t.base.SetCode(key.Address, code)
			}
		case StateObjectStorage:
			if storageValue, ok := value.Storage(); ok {
				t.base.SetState(key.Address, key.Slot, storageValue)
			}
		}
	}
	return nil
}

func (t *TxnState) CreateAccount(addr common.Address) {
	t.lifecycle[addr] = lifecycleCreated
}

func (t *TxnState) SetBalance(addr common.Address, value *uint256.Int) {
	t.writeSet.Set(BalanceKey(addr), NewBalanceValue(value))
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
	if value, ok := t.writeSet.Get(BalanceKey(addr)); ok {
		if balance, ok := value.Balance(); ok {
			return balance
		}
	}
	t.readSet[BalanceKey(addr)] = struct{}{}
	if t.base == nil {
		return uint256.NewInt(0)
	}
	return cloneU256(t.base.GetBalance(addr))
}

func (t *TxnState) GetNonce(addr common.Address) uint64 {
	if value, ok := t.writeSet.Get(NonceKey(addr)); ok {
		if nonce, ok := value.Nonce(); ok {
			return nonce
		}
	}
	t.readSet[NonceKey(addr)] = struct{}{}
	if t.base == nil {
		return 0
	}
	return t.base.GetNonce(addr)
}

func (t *TxnState) SetNonce(addr common.Address, nonce uint64) {
	t.writeSet.Set(NonceKey(addr), NewNonceValue(nonce))
}

func (t *TxnState) GetCodeHash(addr common.Address) common.Hash {
	if value, ok := t.writeSet.Get(CodeKey(addr)); ok {
		if code, ok := value.Code(); ok {
			return crypto.Keccak256Hash(code)
		}
	}
	t.readSet[CodeKey(addr)] = struct{}{}
	if t.base == nil {
		return common.Hash{}
	}
	return t.base.GetCodeHash(addr)
}

func (t *TxnState) GetCode(addr common.Address) []byte {
	if value, ok := t.writeSet.Get(CodeKey(addr)); ok {
		if code, ok := value.Code(); ok {
			return code
		}
	}
	t.readSet[CodeKey(addr)] = struct{}{}
	if t.base == nil {
		return nil
	}
	return cloneBytes(t.base.GetCode(addr))
}

func (t *TxnState) SetCode(addr common.Address, code []byte) {
	codeCopy := cloneBytes(code)
	t.writeSet.Set(CodeKey(addr), NewCodeValue(codeCopy))
}

func (t *TxnState) GetCodeSize(addr common.Address) int {
	if value, ok := t.writeSet.Get(CodeKey(addr)); ok {
		if code, ok := value.Code(); ok {
			return len(code)
		}
	}
	t.readSet[CodeKey(addr)] = struct{}{}
	if t.base == nil {
		return 0
	}
	return t.base.GetCodeSize(addr)
}

func (t *TxnState) AddRefund(gas uint64) {
	t.refund += gas
}

func (t *TxnState) SubRefund(gas uint64) {
	if gas > t.refund {
		panic(fmt.Sprintf("Refund counter below zero (gas: %d > refund: %d)", gas, t.refund))
	}
	t.refund -= gas
}

func (t *TxnState) GetRefund() uint64 {
	return t.refund
}

func (t *TxnState) GetCommittedState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	if value, ok := t.writeSet.Get(StorageKey(addr, key)); ok {
		if storageValue, ok := value.Storage(); ok {
			return storageValue
		}
	}
	t.readSet[StorageKey(addr, key)] = struct{}{}
	if t.base == nil {
		return common.Hash{}
	}
	return t.base.GetCommittedState(addr, key)
}

func (t *TxnState) GetState(addr common.Address, key common.Hash, _ ...stateconf.StateDBStateOption) common.Hash {
	if value, ok := t.writeSet.Get(StorageKey(addr, key)); ok {
		if storageValue, ok := value.Storage(); ok {
			return storageValue
		}
	}
	t.readSet[StorageKey(addr, key)] = struct{}{}
	if t.base == nil {
		return common.Hash{}
	}
	return t.base.GetState(addr, key)
}

func (t *TxnState) SetState(addr common.Address, key, value common.Hash, _ ...stateconf.StateDBStateOption) {
	t.writeSet.Set(StorageKey(addr, key), NewStorageValue(value))
}

func (t *TxnState) GetTransientState(addr common.Address, key common.Hash) common.Hash {
	if slots, ok := t.transient[addr]; ok {
		if value, ok := slots[key]; ok {
			return value
		}
	}
	return common.Hash{}
}

func (t *TxnState) SetTransientState(addr common.Address, key, value common.Hash) {
	if _, ok := t.transient[addr]; !ok {
		t.transient[addr] = make(map[common.Hash]common.Hash)
	}
	t.transient[addr][key] = value
}

func (t *TxnState) SelfDestruct(addr common.Address) {
	t.lifecycle[addr] = lifecycleSelfDestructLegacy
}

func (t *TxnState) HasSelfDestructed(addr common.Address) bool {
	if op, ok := t.lifecycle[addr]; ok {
		return op == lifecycleSelfDestructLegacy || op == lifecycleSelfDestructEIP6780
	}
	if t.base == nil {
		return false
	}
	return t.base.HasSelfDestructed(addr)
}

func (t *TxnState) Selfdestruct6780(addr common.Address) {
	t.lifecycle[addr] = lifecycleSelfDestructEIP6780
}

func (t *TxnState) Exist(addr common.Address) bool {
	if op, ok := t.lifecycle[addr]; ok {
		if op == lifecycleCreated {
			return true
		}
		// Mirrors StateDB behavior where suicided objects can still be considered existing until finalization.
		return true
	}
	if t.base == nil {
		return false
	}
	return t.base.Exist(addr)
}

func (t *TxnState) Empty(addr common.Address) bool {
	// If tx wrote any field, compute emptiness from local view.
	if _, ok := t.writeSet.Get(BalanceKey(addr)); ok {
		return t.GetBalance(addr).IsZero() && t.GetNonce(addr) == 0 && len(t.GetCode(addr)) == 0
	}
	if _, ok := t.writeSet.Get(NonceKey(addr)); ok {
		return t.GetBalance(addr).IsZero() && t.GetNonce(addr) == 0 && len(t.GetCode(addr)) == 0
	}
	if _, ok := t.writeSet.Get(CodeKey(addr)); ok {
		return t.GetBalance(addr).IsZero() && t.GetNonce(addr) == 0 && len(t.GetCode(addr)) == 0
	}
	if t.base == nil {
		return true
	}
	return t.base.Empty(addr)
}

func (t *TxnState) AddressInAccessList(addr common.Address) bool {
	_, ok := t.accessA[addr]
	return ok
}

func (t *TxnState) SlotInAccessList(addr common.Address, slot common.Hash) (bool, bool) {
	_, addrOK := t.accessA[addr]
	if !addrOK {
		return false, false
	}
	if slots, ok := t.accessS[addr]; ok {
		_, slotOK := slots[slot]
		return true, slotOK
	}
	return true, false
}

func (t *TxnState) AddAddressToAccessList(addr common.Address) {
	t.accessA[addr] = struct{}{}
}

func (t *TxnState) AddSlotToAccessList(addr common.Address, slot common.Hash) {
	t.AddAddressToAccessList(addr)
	if _, ok := t.accessS[addr]; !ok {
		t.accessS[addr] = make(map[common.Hash]struct{})
	}
	t.accessS[addr][slot] = struct{}{}
}

func (t *TxnState) Prepare(_ params.Rules, sender, coinbase common.Address, dest *common.Address, precompiles []common.Address, txAccesses types.AccessList) {
	// Reset tx-local access list and prime it with tx inputs.
	t.accessA = make(map[common.Address]struct{})
	t.accessS = make(map[common.Address]map[common.Hash]struct{})

	t.AddAddressToAccessList(sender)
	t.AddAddressToAccessList(coinbase)
	if dest != nil {
		t.AddAddressToAccessList(*dest)
	}
	for _, precompile := range precompiles {
		t.AddAddressToAccessList(precompile)
	}
	for _, tuple := range txAccesses {
		t.AddAddressToAccessList(tuple.Address)
		for _, slot := range tuple.StorageKeys {
			t.AddSlotToAccessList(tuple.Address, slot)
		}
	}
}

func (t *TxnState) RevertToSnapshot(_ int) {
	// TxnState is discarded on conflict or execution failure in this model.
}

func (t *TxnState) Snapshot() int {
	// Snapshot/Revert is intentionally unused in this model.
	return 0
}

func (t *TxnState) AddLog(log *types.Log) {
	logCopy := *log
	logCopy.TxHash = t.txHash
	logCopy.TxIndex = uint(t.txIndex)
	logCopy.Index = t.logSize
	t.logs = append(t.logs, &logCopy)
	t.logSize++
}

func (t *TxnState) AddPreimage(hash common.Hash, preimage []byte) {
	if _, exists := t.preimage[hash]; exists {
		return
	}
	copyPreimage := make([]byte, len(preimage))
	copy(copyPreimage, preimage)
	t.preimage[hash] = copyPreimage
}

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

func (t *TxnState) Preimages() map[common.Hash][]byte {
	out := make(map[common.Hash][]byte, len(t.preimage))
	for hash, image := range t.preimage {
		copyImage := make([]byte, len(image))
		copy(copyImage, image)
		out[hash] = copyImage
	}
	return out
}

func (t *TxnState) HashCode(addr common.Address) common.Hash {
	return crypto.Keccak256Hash(t.GetCode(addr))
}

func (t *TxnState) Reset() {
	t.writeSet = NewTxWriteSet()
	t.readSet = make(map[StateObjectKey]struct{})
	t.refund = 0
	t.lifecycle = make(map[common.Address]accountLifecycle)
	t.logs = nil
	t.logSize = 0
	t.preimage = make(map[common.Hash][]byte)
	t.transient = make(map[common.Address]map[common.Hash]common.Hash)
	t.accessA = make(map[common.Address]struct{})
	t.accessS = make(map[common.Address]map[common.Hash]struct{})
}

func (t *TxnState) CloneForRetry() *TxnState {
	retry := NewTxnState(t.base, t.txHash, t.txIndex)
	retry.refund = t.refund
	retry.lifecycle = maps.Clone(t.lifecycle)
	for key, value := range t.writeSet.Entries() {
		if key.Kind == StateObjectStorage {
			if storageValue, ok := value.Storage(); ok {
				retry.writeSet.Set(key, NewStorageValue(storageValue))
			}
		}
	}
	return retry
}
