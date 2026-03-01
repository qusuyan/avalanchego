package parallel

import (
	"fmt"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/stateconf"
)

// BlockState is the canonical block-level state used by parallel execution.
// It exposes versioned reads and deterministic write-set application.
type BlockState interface {
	Exists(addr common.Address, workerID int) (bool, ObjectVersion, error)
	Read(key StateObjectKey, workerID int) (*VersionedValue, error)
	Logs() []*types.Log
	ApplyWriteSet(txIndex int, version ObjectVersion, ws *TxWriteSet) error
	AddLogs(txIndex int, logs []*types.Log) error
	AddPreimages(txIndex int, preimages map[common.Hash][]byte) error
	ValidateReadSet(rs *TxReadSet) bool
	WriteBack() error
	Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error)
}

// SequentialBlockState adapts state.StateDB to BlockState for phase-2 wiring.
// This is a placeholder implementation that treats all reads as committed and does not perform any validation,
// but it allows us to wire up the TxnState and test the plumbing of speculative execution.
type SequentialBlockState struct {
	base *state.StateDB
}

func NewSequentialBlockState(base *state.StateDB) *SequentialBlockState {
	return &SequentialBlockState{base: base}
}

func (b *SequentialBlockState) Exists(addr common.Address, _ int) (bool, ObjectVersion, error) {
	if b == nil || b.base == nil {
		return false, ERROR_VERSION, nil
	}
	exists := b.base.Exist(addr)
	return exists, COMMITTED_VERSION, nil
}

func (b *SequentialBlockState) Read(key StateObjectKey, _ int) (*VersionedValue, error) {
	if b == nil || b.base == nil {
		return nil, fmt.Errorf("nil base state")
	}
	switch key.Kind {
	case StateObjectBalance:
		return &VersionedValue{
			Value:   NewBalanceValue(b.base.GetBalance(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectNonce:
		return &VersionedValue{
			Value:   NewNonceValue(b.base.GetNonce(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectCodeHash:
		return &VersionedValue{
			Value:   NewCodeHashValue(b.base.GetCodeHash(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectCode:
		return &VersionedValue{
			Value:   NewCodeValue(b.base.GetCode(key.Address)),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectStorage:
		return &VersionedValue{
			Value:   NewStorageValue(b.base.GetState(key.Address, key.Slot, stateconf.SkipStateKeyTransformation())),
			Version: COMMITTED_VERSION,
		}, nil
	case StateObjectExtra:
		return &VersionedValue{
			Value:   NewExtraValue(b.base.GetExtra(key.Address)),
			Version: 0,
		}, nil
	default:
		return nil, fmt.Errorf("unknown state object kind: %d", key.Kind)
	}
}

func (b *SequentialBlockState) Logs() []*types.Log {
	return b.base.Logs()
}

func (b *SequentialBlockState) ApplyWriteSet(_ int, _ ObjectVersion, ws *TxWriteSet) error {
	if b == nil || b.base == nil || ws == nil {
		return nil
	}

	for addr, lifecycle := range ws.accountLifecycleChanges {
		if lifecycle == lifecycleCreated {
			b.base.CreateAccount(addr)
		} else if lifecycle == lifecycleDestructed {
			b.base.SelfDestruct(addr)
		}
	}

	for key, write := range ws.Entries() {
		switch key.Kind {
		case StateObjectBalance:
			if balance, ok := write.Balance(); ok {
				b.base.SetBalance(key.Address, balance)
			}
		case StateObjectNonce:
			if nonce, ok := write.Nonce(); ok {
				b.base.SetNonce(key.Address, nonce)
			}
		case StateObjectCode:
			if code, ok := write.Code(); ok {
				b.base.SetCode(key.Address, code)
			}
		case StateObjectStorage:
			if storageValue, ok := write.Storage(); ok {
				b.base.SetState(key.Address, key.Slot, storageValue, stateconf.SkipStateKeyTransformation())
			}
		case StateObjectExtra:
			if extra, ok := write.Extra(); ok {
				b.base.SetExtra(key.Address, extra)
			}
		}
	}
	return nil
}

func (b *SequentialBlockState) AddLogs(_ int, logs []*types.Log) error {
	if b == nil || b.base == nil {
		return nil
	}
	for _, log := range logs {
		b.base.AddLog(log)
	}
	return nil
}

func (b *SequentialBlockState) AddPreimages(_ int, preimages map[common.Hash][]byte) error {
	if b == nil || b.base == nil {
		return nil
	}
	for hash, preimage := range preimages {
		b.base.AddPreimage(hash, preimage)
	}
	return nil
}

func (b *SequentialBlockState) ValidateReadSet(_ *TxReadSet) bool {
	return true
}

func (b *SequentialBlockState) WriteBack() error {
	// No-op since this BlockState writes directly to the base StateDB.
	return nil
}

func (b *SequentialBlockState) Commit(block uint64, deleteEmptyObjects bool, opts ...stateconf.StateDBCommitOption) (common.Hash, error) {
	if b == nil || b.base == nil {
		return common.Hash{}, fmt.Errorf("nil base state")
	}
	return b.base.Commit(block, deleteEmptyObjects, opts...)
}
