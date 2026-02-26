package parallel

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
)

func TestCreateExisting(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	txnHash1 := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	txnHashes := []common.Hash{txnHash0, txnHash1}
	blockState := NewStateDBLastWriterBlockState(statedb, txnHashes)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	balance := uint256.NewInt(1000)
	state_key := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	state_value := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	// Txn 0 creates then destructs an account, then Txn 1 creates the same account - it should inherit the balance from Txn 0's creation
	txnState0 := NewTxnState(blockState, txnHash0, 0, 1)
	txnState0.CreateAccount(addr)
	txnState0.SetBalance(addr, balance)
	txnState0.SetState(addr, state_key, state_value)
	if err := txnState0.CommitTxn(); err != nil {
		t.Fatalf("failed to commit txn 0: %v", err)
	}

	txnState1 := NewTxnState(blockState, txnHash1, 1, 1)
	txnState1.CreateAccount(addr)
	if actual := txnState1.GetBalance(addr); actual.Cmp(balance) != 0 {
		t.Fatalf("unexpected balance for recreated account: got %s, want %s", actual, balance)
	}
	if actual := txnState1.GetState(addr, state_key); actual.Cmp(common.Hash{}) != 0 {
		t.Fatalf("unexpected state value for recreated account: got %s, want %s", actual, common.Hash{})
	}
	if err := txnState1.CommitTxn(); err != nil {
		t.Fatalf("failed to commit txn 1: %v", err)
	}
}

func TestWriteToDestructedAccountShouldNotRecreate(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	txnHashes := []common.Hash{txnHash0}
	blockState := NewStateDBLastWriterBlockState(statedb, txnHashes)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	balance1 := uint256.NewInt(1000)
	balance2 := uint256.NewInt(2000)
	state_key := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	state_value := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	// Txn 0 destructs an account then writes to it - the account should not be recreated and the write should be ignored
	txnState0 := NewTxnState(blockState, txnHash0, 0, 1)
	txnState0.CreateAccount(addr)
	txnState0.SetBalance(addr, balance1)
	txnState0.SelfDestruct(addr)
	txnState0.SetBalance(addr, balance2)
	txnState0.SetState(addr, state_key, state_value)
	if err := txnState0.CommitTxn(); err != nil {
		t.Fatalf("failed to commit txn 0: %v", err)
	}
	if err := blockState.WriteBack(); err != nil {
		t.Fatalf("failed to write back block state: %v", err)
	}

	if exists := statedb.Exist(addr); exists {
		t.Fatalf("account should not exist after being destructed")
	}
	if actual := statedb.GetBalance(addr); actual.Cmp(uint256.NewInt(0)) != 0 {
		t.Fatalf("unexpected balance for destructed account: got %s, want 0", actual)
	}
}

func TestValidateReadSetMatchesCurrentVersions(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0})

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	slot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	key := StorageKey(addr, slot)

	readSet := NewTxReadSet()
	exists, existsVersion, err := blockState.Exists(addr)
	if err != nil {
		t.Fatalf("failed to read existence: %v", err)
	}
	if exists {
		t.Fatalf("expected test account to not exist initially")
	}
	if old := readSet.RecordAccountExistence(addr, existsVersion); old != nil {
		t.Fatalf("unexpected existing version when recording existence")
	}
	value, err := blockState.Read(key, 0)
	if err != nil {
		t.Fatalf("failed to read object version: %v", err)
	}
	if old := readSet.RecordObjectVersion(key, value.Version); old != nil {
		t.Fatalf("unexpected existing version when recording object")
	}

	if !blockState.ValidateReadSet(readSet) {
		t.Fatalf("expected read-set validation to succeed for unchanged versions")
	}
}

func TestValidateReadSetDetectsVersionMismatches(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0})

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	slot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	key := StorageKey(addr, slot)

	// Build a read-set against the initial committed versions.
	readSet := NewTxReadSet()
	_, existsVersion, err := blockState.Exists(addr)
	if err != nil {
		t.Fatalf("failed to read existence: %v", err)
	}
	if old := readSet.RecordAccountExistence(addr, existsVersion); old != nil {
		t.Fatalf("unexpected existing version when recording existence")
	}
	value, err := blockState.Read(key, 0)
	if err != nil {
		t.Fatalf("failed to read object version: %v", err)
	}
	if old := readSet.RecordObjectVersion(key, value.Version); old != nil {
		t.Fatalf("unexpected existing version when recording object")
	}

	// Advance canonical versions via writes.
	ws := NewTxWriteSet()
	ws.CreateAccount(addr)
	ws.Set(key, NewStorageValue(common.HexToHash("0x1")))
	if err := blockState.ApplyWriteSet(0, 1, ws); err != nil {
		t.Fatalf("failed to apply write-set: %v", err)
	}

	if blockState.ValidateReadSet(readSet) {
		t.Fatalf("expected read-set validation to fail on version mismatch")
	}
}
