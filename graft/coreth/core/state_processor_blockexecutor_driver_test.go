package core

import (
	"context"
	"crypto/ecdsa"
	"math/big"
	"sync"
	"testing"

	"github.com/ava-labs/avalanchego/graft/coreth/consensus/dummy"
	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel"
	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel/blockexecutor"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/trie"
	"github.com/holiman/uint256"
)

func makeSignedLegacyTx(t *testing.T, key *ecdsa.PrivateKey, signer types.Signer, nonce uint64, to common.Address, gasLimit uint64) *types.Transaction {
	t.Helper()
	tx, err := types.SignTx(types.NewTransaction(
		nonce,
		to,
		big.NewInt(1),
		gasLimit,
		big.NewInt(225000000000),
		nil,
	), signer, key)
	if err != nil {
		t.Fatalf("failed to sign tx: %v", err)
	}
	return tx
}

func newDriverTestSetup(t *testing.T, gasLimit uint64, alloc types.GenesisAlloc, txs types.Transactions, vmCfg vm.Config) (*BlockChain, *stateProcessorBlockExecutorDriver, *state.StateDB) {
	t.Helper()
	cfg := *params.TestChainConfig
	config := &cfg

	db := rawdb.NewMemoryDatabase()
	gspec := &Genesis{
		Config:   config,
		Alloc:    alloc,
		GasLimit: gasLimit,
	}

	blockchain, err := NewBlockChain(db, DefaultCacheConfig, gspec, dummy.NewCoinbaseFaker(), vmCfg, common.Hash{}, false)
	if err != nil {
		t.Fatalf("failed to create blockchain: %v", err)
	}

	parent := gspec.ToBlock()
	header := &types.Header{
		ParentHash: parent.Hash(),
		Coinbase:   parent.Coinbase(),
		Difficulty: big.NewInt(1),
		GasLimit:   gasLimit,
		Number:     new(big.Int).Add(parent.Number(), common.Big1),
		Time:       parent.Time() + 1,
		UncleHash:  types.EmptyUncleHash,
		BaseFee:    big.NewInt(1),
	}
	block := types.NewBlock(header, txs, nil, nil, trie.NewStackTrie(nil))

	statedb, err := blockchain.StateAt(parent.Root())
	if err != nil {
		t.Fatalf("failed to open parent state: %v", err)
	}

	txHashes := make([]common.Hash, len(txs))
	for i, tx := range txs {
		txHashes[i] = tx.Hash()
	}
	blockState := parallel.NewStateDBLastWriterBlockState(statedb, txHashes)
	driver, err := newStateProcessorBlockExecutorDriver(config, blockchain, block, blockState, vmCfg)
	if err != nil {
		t.Fatalf("failed to create driver: %v", err)
	}

	return blockchain, driver, statedb
}

func TestStateProcessorBlockExecutorDriverExecuteCommitWriteBack(t *testing.T) {
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	sender := crypto.PubkeyToAddress(key1.PublicKey)
	receiver := common.HexToAddress("0x1111111111111111111111111111111111111111")

	alloc := types.GenesisAlloc{
		sender: {Balance: big.NewInt(9_000_000_000_000_000_000)},
	}

	cfg := *params.TestChainConfig
	signer := types.MakeSigner(&cfg, big.NewInt(1), 1)
	tx := makeSignedLegacyTx(t, key1, signer, 0, receiver, 21000)

	blockchain, driver, statedb := newDriverTestSetup(t, 1_000_000, alloc, types.Transactions{tx}, vm.Config{})
	defer blockchain.Stop()

	if !statedb.GetBalance(receiver).IsZero() {
		t.Fatalf("expected receiver to start with zero balance")
	}

	if err := driver.Execute(context.Background(), 0); err != nil {
		t.Fatalf("execute failed: %v", err)
	}
	receipt, err := driver.Commit(0)
	if err != nil {
		t.Fatalf("commit failed: %v", err)
	}
	if receipt == nil {
		t.Fatalf("expected non-nil receipt")
	}
	if receipt.CumulativeGasUsed != receipt.GasUsed {
		t.Fatalf("unexpected cumulative gas: got %d want %d", receipt.CumulativeGasUsed, receipt.GasUsed)
	}
	if len(receipt.Logs) != 0 {
		t.Fatalf("unexpected logs: %d", len(receipt.Logs))
	}
	if !statedb.GetBalance(receiver).IsZero() {
		t.Fatalf("state should not be materialized before WriteBack")
	}

	if err := driver.blockState.WriteBack(); err != nil {
		t.Fatalf("writeback failed: %v", err)
	}
	if statedb.GetBalance(receiver).Cmp(uint256.NewInt(1)) != 0 {
		t.Fatalf("unexpected receiver balance after writeback: %s", statedb.GetBalance(receiver))
	}
}

func TestStateProcessorBlockExecutorDriverAtomicGasLimitOnCommit(t *testing.T) {
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	key2, _ := crypto.HexToECDSA("8a1f9a8f3f709e18e0ad2ef889ca7d57f7d2f1d7f181f7f4a45f4a3f8f8c1a21")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	receiver := common.HexToAddress("0x2222222222222222222222222222222222222222")

	alloc := types.GenesisAlloc{
		sender1: {Balance: big.NewInt(9_000_000_000_000_000_000)},
		sender2: {Balance: big.NewInt(9_000_000_000_000_000_000)},
	}

	cfg := *params.TestChainConfig
	signer := types.MakeSigner(&cfg, big.NewInt(1), 1)
	tx0 := makeSignedLegacyTx(t, key1, signer, 0, receiver, 21000)
	tx1 := makeSignedLegacyTx(t, key2, signer, 0, receiver, 21000)

	blockchain, driver, _ := newDriverTestSetup(t, 21000, alloc, types.Transactions{tx0, tx1}, vm.Config{})
	defer blockchain.Stop()

	if err := driver.Execute(context.Background(), 0); err != nil {
		t.Fatalf("execute tx0 failed: %v", err)
	}
	if err := driver.Execute(context.Background(), 1); err != nil {
		t.Fatalf("execute tx1 failed: %v", err)
	}

	if _, err := driver.Commit(0); err != nil {
		t.Fatalf("commit tx0 failed: %v", err)
	}
	if _, err := driver.Commit(1); err != ErrGasLimitReached {
		t.Fatalf("expected ErrGasLimitReached on tx1 commit, got %v", err)
	}
}

func TestStateProcessorBlockExecutorDriverConcurrentExecute(t *testing.T) {
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	key2, _ := crypto.HexToECDSA("8a1f9a8f3f709e18e0ad2ef889ca7d57f7d2f1d7f181f7f4a45f4a3f8f8c1a21")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	receiver := common.HexToAddress("0x3333333333333333333333333333333333333333")

	alloc := types.GenesisAlloc{
		sender1: {Balance: big.NewInt(9_000_000_000_000_000_000)},
		sender2: {Balance: big.NewInt(9_000_000_000_000_000_000)},
	}

	cfg := *params.TestChainConfig
	signer := types.MakeSigner(&cfg, big.NewInt(1), 1)
	tx0 := makeSignedLegacyTx(t, key1, signer, 0, receiver, 21000)
	tx1 := makeSignedLegacyTx(t, key2, signer, 0, receiver, 21000)

	blockchain, driver, _ := newDriverTestSetup(t, 1_000_000, alloc, types.Transactions{tx0, tx1}, vm.Config{ParallelExecutionWorkers: 2})
	defer blockchain.Stop()

	var (
		wg   sync.WaitGroup
		errs [2]error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		err := driver.Execute(blockexecutor.WithWorkerID(context.Background(), 0), 0)
		errs[0] = err
	}()
	go func() {
		defer wg.Done()
		err := driver.Execute(blockexecutor.WithWorkerID(context.Background(), 1), 1)
		errs[1] = err
	}()
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("concurrent execute failed: err0=%v err1=%v", errs[0], errs[1])
	}
	if _, err := driver.Commit(0); err != nil {
		t.Fatalf("commit tx0 failed: %v", err)
	}
	if _, err := driver.Commit(1); err != nil {
		t.Fatalf("commit tx1 failed: %v", err)
	}
}

func TestStateProcessorBlockExecutorDriverReusesEVMSequentialExecute(t *testing.T) {
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	key2, _ := crypto.HexToECDSA("8a1f9a8f3f709e18e0ad2ef889ca7d57f7d2f1d7f181f7f4a45f4a3f8f8c1a21")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	receiver := common.HexToAddress("0x4444444444444444444444444444444444444444")

	alloc := types.GenesisAlloc{
		sender1: {Balance: big.NewInt(9_000_000_000_000_000_000)},
		sender2: {Balance: big.NewInt(9_000_000_000_000_000_000)},
	}

	cfg := *params.TestChainConfig
	signer := types.MakeSigner(&cfg, big.NewInt(1), 1)
	tx0 := makeSignedLegacyTx(t, key1, signer, 0, receiver, 21000)
	tx1 := makeSignedLegacyTx(t, key2, signer, 0, receiver, 21000)

	blockchain, driver, _ := newDriverTestSetup(t, 1_000_000, alloc, types.Transactions{tx0, tx1}, vm.Config{ParallelExecutionWorkers: 2})
	defer blockchain.Stop()

	ctx := blockexecutor.WithWorkerID(context.Background(), 0)
	slotEVM := driver.evmByWorker[0]
	if slotEVM == nil {
		t.Fatalf("expected preinitialized worker EVM")
	}
	if err := driver.Execute(ctx, 0); err != nil {
		t.Fatalf("execute tx0 failed: %v", err)
	}
	if err := driver.Execute(ctx, 1); err != nil {
		t.Fatalf("execute tx1 failed: %v", err)
	}
	if driver.evmByWorker[0] != slotEVM {
		t.Fatalf("expected worker 0 EVM pointer to be stable across executes")
	}
}

func TestStateProcessorBlockExecutorDriverCreatesOneEVMPerWorker(t *testing.T) {
	key1, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291")
	key2, _ := crypto.HexToECDSA("8a1f9a8f3f709e18e0ad2ef889ca7d57f7d2f1d7f181f7f4a45f4a3f8f8c1a21")
	sender1 := crypto.PubkeyToAddress(key1.PublicKey)
	sender2 := crypto.PubkeyToAddress(key2.PublicKey)
	receiver := common.HexToAddress("0x5555555555555555555555555555555555555555")

	alloc := types.GenesisAlloc{
		sender1: {Balance: big.NewInt(9_000_000_000_000_000_000)},
		sender2: {Balance: big.NewInt(9_000_000_000_000_000_000)},
	}

	cfg := *params.TestChainConfig
	signer := types.MakeSigner(&cfg, big.NewInt(1), 1)
	tx0 := makeSignedLegacyTx(t, key1, signer, 0, receiver, 21000)
	tx1 := makeSignedLegacyTx(t, key2, signer, 0, receiver, 21000)

	blockchain, driver, _ := newDriverTestSetup(t, 1_000_000, alloc, types.Transactions{tx0, tx1}, vm.Config{ParallelExecutionWorkers: 2})
	defer blockchain.Stop()

	if err := driver.Execute(blockexecutor.WithWorkerID(context.Background(), 0), 0); err != nil {
		t.Fatalf("execute tx0 failed: %v", err)
	}
	if err := driver.Execute(blockexecutor.WithWorkerID(context.Background(), 1), 1); err != nil {
		t.Fatalf("execute tx1 failed: %v", err)
	}
	if len(driver.evmByWorker) != 2 {
		t.Fatalf("unexpected worker EVM slot count: got %d want 2", len(driver.evmByWorker))
	}
	if driver.evmByWorker[0] == nil || driver.evmByWorker[1] == nil {
		t.Fatalf("expected non-nil preinitialized EVM slots")
	}
	if driver.evmByWorker[0] == driver.evmByWorker[1] {
		t.Fatalf("expected distinct EVM instance per worker slot")
	}
}
