package parallel

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/libevm/stateconf"
	"github.com/holiman/uint256"
)

type testBlockState struct{}

type transformTestHooks struct{}

func (transformTestHooks) TransformStateKey(_ common.Address, key common.Hash) common.Hash {
	key[0] |= 0x80
	return key
}

type recordingBlockState struct {
	lastReadKey StateObjectKey
}

func (b *recordingBlockState) Exists(common.Address) (bool, ObjectVersion, error) {
	return true, COMMITTED_VERSION, nil
}

func (b *recordingBlockState) Read(key StateObjectKey, _ uint64) (*VersionedValue, error) {
	b.lastReadKey = key
	if key.Kind == StateObjectStorage {
		return &VersionedValue{Value: NewStorageValue(common.HexToHash("0x99")), Version: COMMITTED_VERSION}, nil
	}
	return (&testBlockState{}).Read(key, 0)
}

func (b *recordingBlockState) Logs() []*types.Log { return nil }

func (b *recordingBlockState) ApplyWriteSet(int, ObjectVersion, *TxWriteSet) error { return nil }

func (b *recordingBlockState) AddLogs(int, []*types.Log) error { return nil }

func (b *recordingBlockState) AddPreimages(int, map[common.Hash][]byte) error { return nil }

func (b *recordingBlockState) ValidateReadSet(*TxReadSet) bool { return true }

func (b *recordingBlockState) WriteBack() error { return nil }

func (b *recordingBlockState) Commit(uint64, bool, ...stateconf.StateDBCommitOption) (common.Hash, error) {
	return common.Hash{}, nil
}

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

func TestTxnStateStorageCanonicalization(t *testing.T) {
	state.TestOnlyClearRegisteredExtras()
	defer state.TestOnlyClearRegisteredExtras()
	state.RegisterExtras(transformTestHooks{})

	tx := NewTxnState(testBlockState{}, common.HexToHash("0x1234"), 2, 0)
	addr := common.HexToAddress("0xabc")
	slot := common.HexToHash("0x2")
	value := common.HexToHash("0x55")
	transformedSlot := state.TransformStateKey(addr, slot)

	tx.SetState(addr, slot, value)

	if _, ok := tx.writeSet.Get(StorageKey(addr, slot)); ok {
		t.Fatalf("expected raw slot key to not be present in writeset")
	}

	write, ok := tx.writeSet.Entries()[StorageKey(addr, transformedSlot)]
	if !ok {
		t.Fatalf("expected transformed slot key to be present in writeset")
	}
	if storedValue, ok := write.Storage(); !ok || storedValue != value {
		t.Fatalf("unexpected stored value: %s", storedValue)
	}
}

func TestTxnStateStorageReadUsesCanonicalizedKey(t *testing.T) {
	state.TestOnlyClearRegisteredExtras()
	defer state.TestOnlyClearRegisteredExtras()
	state.RegisterExtras(transformTestHooks{})

	base := &recordingBlockState{}
	tx := NewTxnState(base, common.HexToHash("0x1234"), 2, 0)
	addr := common.HexToAddress("0xabc")
	slot := common.HexToHash("0x2")
	transformedSlot := state.TransformStateKey(addr, slot)

	got := tx.GetState(addr, slot)
	if got != common.HexToHash("0x99") {
		t.Fatalf("unexpected storage value: %s", got)
	}
	if base.lastReadKey != StorageKey(addr, transformedSlot) {
		t.Fatalf("expected transformed key %v, got %v", StorageKey(addr, transformedSlot), base.lastReadKey)
	}
}

func TestTxnStateSnapshotDirtyBitAndIndices(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x1234"), 2, 0)
	addr := common.HexToAddress("0xabc")

	if snap := tx.Snapshot(); snap != 0 {
		t.Fatalf("expected empty snapshot index 0, got %d", snap)
	}

	tx.SetBalance(addr, uint256.NewInt(1))
	if snap := tx.Snapshot(); snap != 1 {
		t.Fatalf("expected first dirty snapshot index 1, got %d", snap)
	}

	if snap := tx.Snapshot(); snap != 1 {
		t.Fatalf("expected unchanged snapshot index 1, got %d", snap)
	}

	tx.SetNonce(addr, 2)
	if snap := tx.Snapshot(); snap != 2 {
		t.Fatalf("expected second dirty snapshot index 2, got %d", snap)
	}
}

func TestTxnStateRevertToSnapshotRestoresWriteSetAndTxnLocalData(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x1111"), 7, 0)
	addr := common.HexToAddress("0xabcd")
	slot := common.HexToHash("0x1")
	log1 := &types.Log{Address: addr}
	log2 := &types.Log{Address: common.HexToAddress("0xbeef")}
	preimage1Key := common.HexToHash("0x11")
	preimage2Key := common.HexToHash("0x22")

	tx.SetBalance(addr, uint256.NewInt(10))
	tx.SetState(addr, slot, common.HexToHash("0xaa"))
	tx.AddRefund(9)
	tx.SetTransientState(addr, slot, common.HexToHash("0x31"))
	tx.AddAddressToAccessList(addr)
	tx.AddLog(log1)
	tx.AddPreimage(preimage1Key, []byte{0x01})
	snap := tx.Snapshot()
	if snap != 1 {
		t.Fatalf("expected snapshot id 1, got %d", snap)
	}

	tx.SetBalance(addr, uint256.NewInt(20))
	tx.SetState(addr, slot, common.HexToHash("0xbb"))
	tx.AddRefund(3)
	tx.SetTransientState(addr, slot, common.HexToHash("0x32"))
	tx.AddSlotToAccessList(addr, slot)
	tx.AddLog(log2)
	tx.AddPreimage(preimage2Key, []byte{0x02})

	tx.RevertToSnapshot(snap)

	if got := tx.GetBalance(addr); got.Cmp(uint256.NewInt(10)) != 0 {
		t.Fatalf("unexpected balance after revert: %s", got)
	}
	if got := tx.GetState(addr, slot); got != common.HexToHash("0xaa") {
		t.Fatalf("unexpected storage after revert: %s", got)
	}
	if got := tx.GetRefund(); got != 9 {
		t.Fatalf("unexpected refund after revert: %d", got)
	}
	if got := tx.GetTransientState(addr, slot); got != common.HexToHash("0x31") {
		t.Fatalf("unexpected transient value after revert: %s", got)
	}
	if present := tx.AddressInAccessList(addr); !present {
		t.Fatalf("expected address to remain in access list after revert")
	}
	if _, slotPresent := tx.SlotInAccessList(addr, slot); slotPresent {
		t.Fatalf("expected slot access-list entry to be reverted")
	}
	logs := tx.GetLogs(tx.TxHash(), 1, common.HexToHash("0x1"))
	if len(logs) != 1 {
		t.Fatalf("expected 1 log after revert, got %d", len(logs))
	}
	if len(tx.Preimages()) != 1 {
		t.Fatalf("expected 1 preimage after revert, got %d", len(tx.Preimages()))
	}
	if _, exists := tx.Preimages()[preimage2Key]; exists {
		t.Fatalf("unexpected second preimage after revert")
	}
}

func TestTxnStateRevertToZeroSnapshotResetsTrackedState(t *testing.T) {
	tx := NewTxnState(testBlockState{}, common.HexToHash("0x2222"), 3, 0)
	addr := common.HexToAddress("0x1234")
	slot := common.HexToHash("0x7")

	tx.SetBalance(addr, uint256.NewInt(5))
	tx.SetState(addr, slot, common.HexToHash("0xcc"))
	tx.AddRefund(4)
	tx.SetTransientState(addr, slot, common.HexToHash("0x44"))
	tx.AddAddressToAccessList(addr)
	tx.AddLog(&types.Log{Address: addr})
	tx.AddPreimage(common.HexToHash("0x1"), []byte{0x99})

	tx.RevertToSnapshot(0)

	if got := tx.GetBalance(addr); !got.IsZero() {
		t.Fatalf("expected zero balance after revert to 0, got %s", got)
	}
	if got := tx.GetState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("expected empty storage after revert to 0, got %s", got)
	}
	if got := tx.GetRefund(); got != 0 {
		t.Fatalf("expected zero refund after revert to 0, got %d", got)
	}
	if got := tx.GetTransientState(addr, slot); got != (common.Hash{}) {
		t.Fatalf("expected empty transient after revert to 0, got %s", got)
	}
	if present := tx.AddressInAccessList(addr); present {
		t.Fatalf("expected access list reset after revert to 0")
	}
	if got := tx.GetLogs(tx.TxHash(), 1, common.HexToHash("0x1")); len(got) != 0 {
		t.Fatalf("expected no logs after revert to 0, got %d", len(got))
	}
	if got := tx.Preimages(); len(got) != 0 {
		t.Fatalf("expected no preimages after revert to 0, got %d", len(got))
	}
}

func TestRevert(t *testing.T) {
	addr1 := common.HexToAddress("0xabc")
	addr2 := common.HexToAddress("0xdef")

	txn := NewTxnState(testBlockState{}, common.HexToHash("0x1234"), 2, 0)
	txn.AddAddressToAccessList(addr1)

	snapshot0 := txn.Snapshot()
	txn.AddLog(&types.Log{})
	// first snapshot - this will be what we revert to
	snapshot1 := txn.Snapshot()
	if snapshot0 >= snapshot1 {
		t.Fatalf("expected different snapshot indices for snapshot0 and snapshot1, got %d", snapshot0)
	}
	txn.AddLog(&types.Log{})
	snapshot2 := txn.Snapshot()
	if snapshot1 >= snapshot2 {
		t.Fatalf("expected different snapshot indices for snapshot1 and snapshot2, got %d", snapshot2)
	}
	// add item
	txn.AddAddressToAccessList(addr2)
	// new snapshot - we will revert to this at the end
	snapshot3 := txn.Snapshot()
	if snapshot2 >= snapshot3 {
		t.Fatalf("expected different snapshot indices for snapshot2 and snapshot3, got %d", snapshot3)
	}
	// added item should be gone
	txn.RevertToSnapshot(2)
	snapshot4 := txn.Snapshot()
	if snapshot3 > snapshot4 {
		t.Fatalf("expected different snapshot indices for snapshot3 and snapshot4, got %d", snapshot4)
	}
	txn.AddLog(&types.Log{})
	snapshot5 := txn.Snapshot()
	if snapshot4 >= snapshot5 {
		t.Fatalf("expected different snapshot indices for snapshot4 and snapshot5, got %d", snapshot5)
	}

	txn.RevertToSnapshot(snapshot4)
	if !txn.AddressInAccessList(addr1) {
		t.Fatalf("expected addr1 to be in access list after revert to snapshot1")
	}
	if txn.AddressInAccessList(addr2) {
		t.Fatalf("expected addr2 to not be in access list after revert to snapshot1")
	}
}
