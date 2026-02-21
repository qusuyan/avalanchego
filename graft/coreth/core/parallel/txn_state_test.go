package parallel

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/holiman/uint256"
)

type testBlockState struct{}

func (testBlockState) Exists(common.Address) (bool, ObjectVersion, error) {
	return true, COMMITTED_VERSION, nil
}

func (testBlockState) Read(key StateObjectKey, _ uint64) (*VersionedValue, error) {
	switch key.Kind {
	case StateObjectBalance:
		return &VersionedValue{Value: NewBalanceValue(uint256.NewInt(0)), Version: COMMITTED_VERSION}, nil
	case StateObjectNonce:
		return &VersionedValue{Value: NewNonceValue(0), Version: COMMITTED_VERSION}, nil
	case StateObjectCodeHash:
		return &VersionedValue{Value: NewCodeHashValue(common.Hash{}), Version: COMMITTED_VERSION}, nil
	case StateObjectCode:
		return &VersionedValue{Value: NewCodeValue(nil), Version: COMMITTED_VERSION}, nil
	case StateObjectStorage:
		return &VersionedValue{Value: NewStorageValue(common.Hash{}), Version: COMMITTED_VERSION}, nil
	case StateObjectExtra:
		return &VersionedValue{Value: NewExtraValue(nil), Version: COMMITTED_VERSION}, nil
	default:
		return nil, nil
	}
}

func (testBlockState) GetCommittedState(StateObjectKey) (common.Hash, error) {
	return common.Hash{}, nil
}

func (testBlockState) Logs() []*types.Log { return nil }

func (testBlockState) ApplyWriteSet(int, ObjectVersion, *TxWriteSet) error { return nil }

func (testBlockState) AddLogs(int, []*types.Log) error { return nil }

func (testBlockState) AddPreimages(int, map[common.Hash][]byte) error { return nil }

func (testBlockState) ValidateReadSet(*TxReadSet) bool { return true }

func (testBlockState) WriteBack() error {
	return nil
}

func (testBlockState) Commit(uint64, bool, ...stateconf.StateDBCommitOption) (common.Hash, error) {
	return common.Hash{}, nil
}

func TestTxnStateReadOwnWrites(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x1234"), 2, 0)
	addr := common.HexToAddress("0xabc")
	slot := common.HexToHash("0x2")
	slot2 := common.HexToHash("0x3")

	tx.SetBalance(addr, uint256.NewInt(99))
	tx.SetNonce(addr, 7)
	tx.SetCode(addr, []byte{0x60, 0x00})
	tx.SetState(addr, slot, common.HexToHash("0x55"))
	tx.SetState(addr, slot2, common.HexToHash("0x66"))

	if got := tx.GetBalance(addr); got.Cmp(uint256.NewInt(99)) != 0 {
		t.Fatalf("unexpected balance: %s", got)
	}
	if got := tx.GetNonce(addr); got != 7 {
		t.Fatalf("unexpected nonce: %d", got)
	}
	if got := tx.GetState(addr, slot); got != common.HexToHash("0x55") {
		t.Fatalf("unexpected state value: %s", got)
	}
	if got := tx.GetState(addr, slot2); got != common.HexToHash("0x66") {
		t.Fatalf("unexpected state value for second slot: %s", got)
	}
	if got := tx.GetCodeSize(addr); got != 2 {
		t.Fatalf("unexpected code size: %d", got)
	}
	if len(tx.readSet.objectVersions) != 0 {
		t.Fatalf("expected no base reads when reading own writes, got %d", len(tx.readSet.objectVersions))
	}
	if _, ok := tx.writeSet.Get(StorageKey(addr, slot)); !ok {
		t.Fatalf("expected first storage slot write to be tracked in writeSet")
	}
	if _, ok := tx.writeSet.Get(StorageKey(addr, slot2)); !ok {
		t.Fatalf("expected second storage slot write to be tracked in writeSet")
	}
}

func TestTxnStateValidatePhase1AlwaysTrue(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x1"), 1, 0)
	if !tx.Validate() {
		t.Fatalf("expected Validate() to return true in phase-1 direct wrapper")
	}
}

func TestTxnStateLifecycleLastOpWins(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x2"), 3, 0)
	addr := common.HexToAddress("0xdef")

	tx.SelfDestruct(addr)
	tx.CreateAccount(addr)
	if tx.HasSelfDestructed(addr) {
		t.Fatalf("expected create after selfdestruct to clear selfdestruct flag")
	}
	if !tx.Exist(addr) {
		t.Fatalf("expected account to exist after create")
	}

	tx.CreateAccount(addr)
	tx.Selfdestruct6780(addr)
	if !tx.HasSelfDestructed(addr) {
		t.Fatalf("expected selfdestruct6780 after create to mark selfdestructed")
	}
	if !tx.Exist(addr) {
		t.Fatalf("expected suicided account to still be considered existing in tx overlay")
	}
}
