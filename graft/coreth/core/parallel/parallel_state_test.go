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
