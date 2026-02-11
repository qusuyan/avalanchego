// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package core

import (
	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

// StateObjectKind identifies tx-local write-set object granularity.
type StateObjectKind uint8

const (
	StateObjectBalance StateObjectKind = iota + 1
	StateObjectNonce
	StateObjectCode
	StateObjectStorage
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

func StorageKey(addr common.Address, slot common.Hash) StateObjectKey {
	return StateObjectKey{Kind: StateObjectStorage, Address: addr, Slot: slot}
}

// StateObjectValue stores the new value written for a state object.
type StateObjectValue struct {
	balance *uint256.Int
	nonce   *uint64
	code    []byte
	storage *common.Hash
}

func NewBalanceValue(value *uint256.Int) StateObjectValue {
	return StateObjectValue{balance: cloneU256(value)}
}

func NewNonceValue(value uint64) StateObjectValue {
	return StateObjectValue{nonce: &value}
}

func NewCodeValue(value []byte) StateObjectValue {
	return StateObjectValue{code: cloneBytes(value)}
}

func NewStorageValue(value common.Hash) StateObjectValue {
	v := value
	return StateObjectValue{storage: &v}
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

// TxWriteSet captures keys mutated during a tx run and their new values.
type TxWriteSet struct {
	entries map[StateObjectKey]StateObjectValue
}

func NewTxWriteSet() *TxWriteSet {
	return &TxWriteSet{entries: make(map[StateObjectKey]StateObjectValue)}
}

// Add tracks that a key was written. Prefer Set for writes with value.
func (w *TxWriteSet) Add(key StateObjectKey) {
	if w.entries == nil {
		w.entries = make(map[StateObjectKey]StateObjectValue)
	}
	if _, exists := w.entries[key]; !exists {
		w.entries[key] = StateObjectValue{}
	}
}

func (w *TxWriteSet) Set(key StateObjectKey, value StateObjectValue) {
	if w.entries == nil {
		w.entries = make(map[StateObjectKey]StateObjectValue)
	}
	w.entries[key] = value
}

func (w *TxWriteSet) Get(key StateObjectKey) (StateObjectValue, bool) {
	value, ok := w.entries[key]
	return value, ok
}

func (w *TxWriteSet) Entries() map[StateObjectKey]StateObjectValue {
	out := make(map[StateObjectKey]StateObjectValue, len(w.entries))
	for key, value := range w.entries {
		out[key] = value
	}
	return out
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
