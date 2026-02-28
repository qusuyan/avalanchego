package parallel

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/triedb"
	"github.com/holiman/uint256"
)

type testingError string

func (e testingError) Error() string { return string(e) }

func testingErrorf(format string, args ...any) error {
	return testingError(fmt.Sprintf(format, args...))
}

func TestCreateExisting(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	txnHash1 := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)
	statedb, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	txnHashes := []common.Hash{txnHash0, txnHash1}
	blockState := NewStateDBLastWriterBlockState(statedb, txnHashes, 1)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	balance := uint256.NewInt(1000)
	state_key := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	state_value := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	// Txn 0 creates then destructs an account, then Txn 1 creates the same account - it should inherit the balance from Txn 0's creation
	txnState0 := NewTxnState(blockState, txnHash0, 0, 0, 1)
	txnState0.CreateAccount(addr)
	txnState0.SetBalance(addr, balance)
	txnState0.SetState(addr, state_key, state_value)
	if err := txnState0.CommitTxn(); err != nil {
		t.Fatalf("failed to commit txn 0: %v", err)
	}

	txnState1 := NewTxnState(blockState, txnHash1, 1, 0, 1)
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
	blockState := NewStateDBLastWriterBlockState(statedb, txnHashes, 1)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	balance1 := uint256.NewInt(1000)
	balance2 := uint256.NewInt(2000)
	state_key := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	state_value := common.HexToHash("0xabcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890")
	// Txn 0 destructs an account then writes to it - the account should not be recreated and the write should be ignored
	txnState0 := NewTxnState(blockState, txnHash0, 0, 0, 1)
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
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, 1)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	slot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	key := StorageKey(addr, slot)

	readSet := NewTxReadSet()
	exists, existsVersion, err := blockState.Exists(addr, 0)
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
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, 1)

	addr := common.HexToAddress("0xcccccccccccccccccccccccccccccccccccccccc")
	slot := common.HexToHash("0x1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef")
	key := StorageKey(addr, slot)

	// Build a read-set against the initial committed versions.
	readSet := NewTxReadSet()
	_, existsVersion, err := blockState.Exists(addr, 0)
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

func TestWriteBackPreloadAvoidsCanonicalAccountReload(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)

	seed, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	addr := common.HexToAddress("0x1111111111111111111111111111111111111111")
	seed.SetBalance(addr, uint256.NewInt(3))
	root, err := seed.Commit(0, true)
	if err != nil {
		t.Fatalf("seed commit failed: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit failed: %v", err)
	}

	statedb, _ := state.New(root, state.NewDatabaseWithNodeDB(db, tdb), nil)
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, 1)

	if _, err := blockState.Read(BalanceKey(addr), 0); err != nil {
		t.Fatalf("baseline read failed: %v", err)
	}

	ws := NewTxWriteSet()
	ws.Set(BalanceKey(addr), NewBalanceValue(uint256.NewInt(9)))
	if err := blockState.ApplyWriteSet(0, 1, ws); err != nil {
		t.Fatalf("apply writeset failed: %v", err)
	}
	if err := blockState.WriteBack(); err != nil {
		t.Fatalf("writeback failed: %v", err)
	}
	if got := statedb.AccountReads; got != 0 {
		t.Fatalf("expected no canonical account reread during writeback, got %s", got)
	}
	if got := statedb.GetBalance(addr); got.Cmp(uint256.NewInt(9)) != 0 {
		t.Fatalf("unexpected balance after writeback: %s", got)
	}
}

func TestWriteBackPreloadAvoidsCanonicalStorageReload(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)

	seed, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	addr := common.HexToAddress("0x2222222222222222222222222222222222222222")
	slot := common.HexToHash("0x1")
	seed.SetBalance(addr, uint256.NewInt(1))
	seed.SetState(addr, slot, common.HexToHash("0x11"))
	root, err := seed.Commit(0, true)
	if err != nil {
		t.Fatalf("seed commit failed: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit failed: %v", err)
	}

	statedb, _ := state.New(root, state.NewDatabaseWithNodeDB(db, tdb), nil)
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, 1)

	if _, err := blockState.Read(StorageKey(addr, slot), 0); err != nil {
		t.Fatalf("baseline storage read failed: %v", err)
	}

	ws := NewTxWriteSet()
	ws.Set(StorageKey(addr, slot), NewStorageValue(common.HexToHash("0x22")))
	if err := blockState.ApplyWriteSet(0, 1, ws); err != nil {
		t.Fatalf("apply writeset failed: %v", err)
	}
	if err := blockState.WriteBack(); err != nil {
		t.Fatalf("writeback failed: %v", err)
	}
	if got := statedb.AccountReads; got != 0 {
		t.Fatalf("expected no canonical account reread during storage writeback, got %s", got)
	}
	if got := statedb.StorageReads; got != 0 {
		t.Fatalf("expected no canonical storage reread during writeback, got %s", got)
	}
	if got := statedb.GetState(addr, slot); got != common.HexToHash("0x22") {
		t.Fatalf("unexpected storage value after writeback: %x", got)
	}
}

func TestConcurrentBaselineReadsShareCacheAndValidate(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)

	seed, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	addr := common.HexToAddress("0x3333333333333333333333333333333333333333")
	slot := common.HexToHash("0x5")
	seed.SetBalance(addr, uint256.NewInt(13))
	seed.SetNonce(addr, 17)
	seed.SetState(addr, slot, common.HexToHash("0x44"))
	root, err := seed.Commit(0, true)
	if err != nil {
		t.Fatalf("seed commit failed: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit failed: %v", err)
	}

	statedb, _ := state.New(root, state.NewDatabaseWithNodeDB(db, tdb), nil)
	const workers = 24
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, workers)
	var wg sync.WaitGroup
	errCh := make(chan error, workers)
	for i := 0; i < workers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			readSet := NewTxReadSet()

			exists, version, err := blockState.Exists(addr, workerID)
			if err != nil {
				errCh <- err
				return
			}
			if !exists {
				errCh <- testingError("expected account to exist")
				return
			}
			readSet.RecordAccountExistence(addr, version)

			balanceValue, err := blockState.Read(BalanceKey(addr), workerID)
			if err != nil {
				errCh <- err
				return
			}
			balance, ok := balanceValue.Value.Balance()
			if !ok || balance.Cmp(uint256.NewInt(13)) != 0 {
				errCh <- testingErrorf("unexpected balance: %v", balance)
				return
			}
			readSet.RecordObjectVersion(BalanceKey(addr), balanceValue.Version)

			stateValue, err := blockState.Read(StorageKey(addr, slot), workerID)
			if err != nil {
				errCh <- err
				return
			}
			storageValue, ok := stateValue.Value.Storage()
			if !ok || storageValue != common.HexToHash("0x44") {
				errCh <- testingErrorf("unexpected storage value: %x", storageValue)
				return
			}
			readSet.RecordObjectVersion(StorageKey(addr, slot), stateValue.Version)

			if !blockState.ValidateReadSet(readSet) {
				errCh <- testingError("expected read-set validation to succeed")
				return
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	if cache, ok := blockState.loadAccountCache(addr); !ok || cache == nil || !cache.Exists {
		t.Fatal("expected shared account cache entry to be populated")
	}
	if cache := blockState.loadStorageCache(addr, slot); cache == nil || *cache != common.HexToHash("0x44") {
		t.Fatalf("expected shared storage cache entry to be populated, got %v", cache)
	}
}

func TestConcurrentReadThenWriteBack(t *testing.T) {
	txnHash0 := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	db := rawdb.NewMemoryDatabase()
	tdb := triedb.NewDatabase(db, nil)

	seed, _ := state.New(types.EmptyRootHash, state.NewDatabaseWithNodeDB(db, tdb), nil)
	addr := common.HexToAddress("0x4444444444444444444444444444444444444444")
	slot := common.HexToHash("0x7")
	seed.SetBalance(addr, uint256.NewInt(21))
	seed.SetState(addr, slot, common.HexToHash("0x55"))
	root, err := seed.Commit(0, true)
	if err != nil {
		t.Fatalf("seed commit failed: %v", err)
	}
	if err := tdb.Commit(root, false); err != nil {
		t.Fatalf("triedb commit failed: %v", err)
	}

	statedb, _ := state.New(root, state.NewDatabaseWithNodeDB(db, tdb), nil)
	const readers = 16
	blockState := NewStateDBLastWriterBlockState(statedb, []common.Hash{txnHash0}, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		workerID := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := blockState.Read(BalanceKey(addr), workerID); err != nil {
				t.Errorf("balance read failed: %v", err)
			}
			if _, err := blockState.Read(StorageKey(addr, slot), workerID); err != nil {
				t.Errorf("storage read failed: %v", err)
			}
		}()
	}
	wg.Wait()

	ws := NewTxWriteSet()
	ws.Set(BalanceKey(addr), NewBalanceValue(uint256.NewInt(34)))
	ws.Set(StorageKey(addr, slot), NewStorageValue(common.HexToHash("0x66")))
	if err := blockState.ApplyWriteSet(0, 1, ws); err != nil {
		t.Fatalf("apply writeset failed: %v", err)
	}
	if err := blockState.WriteBack(); err != nil {
		t.Fatalf("writeback failed: %v", err)
	}
	if got := statedb.AccountReads; got != 0 {
		t.Fatalf("expected no canonical account reread, got %s", got)
	}
	if got := statedb.StorageReads; got != 0 {
		t.Fatalf("expected no canonical storage reread, got %s", got)
	}
	if got := statedb.GetBalance(addr); got.Cmp(uint256.NewInt(34)) != 0 {
		t.Fatalf("unexpected balance after writeback: %s", got)
	}
	if got := statedb.GetState(addr, slot); got != common.HexToHash("0x66") {
		t.Fatalf("unexpected storage after writeback: %x", got)
	}
}
