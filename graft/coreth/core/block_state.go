// Copyright (C) 2026, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package core

import (
	"fmt"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/libevm/stateconf"
)

// BlockState is the canonical block-level state used by parallel execution.
// It exposes versioned reads and deterministic write-set application.
type BlockState interface {
	Exists(addr common.Address) (bool, ObjectVersion, error)
	Read(key StateObjectKey, txIndex uint64) (VersionedValue, error)
	GetCommittedState(key StateObjectKey) (common.Hash, error)
	ApplyWriteSet(txIndex uint64, ws *TxWriteSet) error
	ValidateReadSet(rs *TxReadSet) bool
	Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error)
}

// StateDBBlockState adapts state.StateDB to BlockState for phase-2 wiring.
// This is a placeholder implementation that treats all reads as committed and does not perform any validation,
// but it allows us to wire up the TxnState and test the plumbing of speculative execution.
type StateDBBlockState struct {
	base *state.StateDB
}

func NewStateDBBlockState(base *state.StateDB) *StateDBBlockState {
	return &StateDBBlockState{base: base}
}

func (b *StateDBBlockState) Exists(addr common.Address) (bool, ObjectVersion, error) {
	if b == nil || b.base == nil {
		return false, ERROR_VERSION, nil
	}
	exists := b.base.Exist(addr)
	return exists, COMMITTED_VERSION, nil
}

func (b *StateDBBlockState) Read(key StateObjectKey, _ uint64) (VersionedValue, error) {
	if b == nil || b.base == nil {
		return VersionedValue{}, fmt.Errorf("nil base state")
	}
	switch key.Kind {
	case StateObjectBalance:
		return VersionedValue{
			Value:   NewBalanceValue(b.base.GetBalance(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectNonce:
		return VersionedValue{
			Value:   NewNonceValue(b.base.GetNonce(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectCodeHash:
		return VersionedValue{
			Value:   NewCodeHashValue(b.base.GetCodeHash(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectCode:
		return VersionedValue{
			Value:   NewCodeValue(b.base.GetCode(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectStorage:
		return VersionedValue{
			Value:   NewStorageValue(b.base.GetState(key.Address, key.Slot)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectExtra:
		return VersionedValue{
			Value:   NewExtraValue(b.base.GetExtra(key.Address)),
			Version: 0,
		}, nil
	default:
		return VersionedValue{}, fmt.Errorf("unknown state object kind: %d", key.Kind)
	}
}

func (b *StateDBBlockState) GetCommittedState(key StateObjectKey) (common.Hash, error) {
	// In this placeholder implementation, all reads are from the committed state,
	// so this is the same as Read. The dedicated BlockState implementations will
	// differentiate between committed and speculative reads.
	if v, err := b.Read(key, 0); err == nil {
		if storageValue, ok := v.Value.Storage(); ok {
			return storageValue, nil
		}
		return common.Hash{}, fmt.Errorf("committed value for key not found or not a storage value")
	} else {
		return common.Hash{}, err
	}
}

func (b *StateDBBlockState) ApplyWriteSet(_ uint64, ws *TxWriteSet) error {
	if b == nil || b.base == nil || ws == nil {
		return nil
	}

	for addr, lifecycle := range ws.accountLifecycleChanges {
		if lifecycle == lifecycleCreated {
			b.base.CreateAccount(addr)
		} else if lifecycle == lifecycleSelfDestructed {
			b.base.SelfDestruct(addr)
		}
	}

	for key, value := range ws.Entries() {
		switch key.Kind {
		case StateObjectBalance:
			if balance, ok := value.Balance(); ok {
				b.base.SetBalance(key.Address, balance)
			}
		case StateObjectNonce:
			if nonce, ok := value.Nonce(); ok {
				b.base.SetNonce(key.Address, nonce)
			}
		case StateObjectCode:
			if code, ok := value.Code(); ok {
				b.base.SetCode(key.Address, code)
			}
		case StateObjectStorage:
			if storageValue, ok := value.Storage(); ok {
				b.base.SetState(key.Address, key.Slot, storageValue)
			}
		case StateObjectExtra:
			if extra, ok := value.Extra(); ok {
				b.base.SetExtra(key.Address, extra)
			}
		}
	}
	return nil
}

func (b *StateDBBlockState) ValidateReadSet(_ *TxReadSet) bool {
	return true
}

func (b *StateDBBlockState) Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error) {
	if b == nil || b.base == nil {
		return common.Hash{}, nil
	}
	return b.base.Commit(block, deleteEmptyObjects, opts...)
}
