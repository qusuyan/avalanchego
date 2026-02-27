package core

import (
	"crypto/ecdsa"
	"math/big"
	"reflect"
	"testing"

	"github.com/ava-labs/avalanchego/graft/coreth/consensus/dummy"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/avalanchego/upgrade"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/rawdb"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
)

const (
	logRuntimeCodeHex   = "60006000a000" // LOG0 + STOP
	storeRuntimeCodeHex = "600160005500" // SSTORE(0,1) + STOP
)

type logView struct {
	Address common.Address
	Topics  []common.Hash
	Data    []byte
	TxIndex uint
	Index   uint
}

type receiptView struct {
	Type              uint8
	Status            uint64
	CumulativeGasUsed uint64
	GasUsed           uint64
	TxHash            common.Hash
	ContractAddress   common.Address
	Logs              []logView
}

type runView struct {
	UsedGas          uint64
	Receipts         []receiptView
	AllLogs          []logView
	StateRoot        common.Hash
	SenderNonces     []uint64
	ReceiverBalances []uint64
	StoreSlot0       common.Hash
	LogCodeSize      int
	StoreCodeSize    int
}

func TestStateProcessorParallelExecutorEquivalenceMultiWorker(t *testing.T) {
	keys := make([]*ecdsa.PrivateKey, 5)
	senders := make([]common.Address, len(keys))
	for i := range keys {
		key, err := crypto.GenerateKey()
		if err != nil {
			t.Fatalf("failed to generate key %d: %v", i, err)
		}
		keys[i] = key
		senders[i] = crypto.PubkeyToAddress(key.PublicKey)
	}

	receiver1 := common.HexToAddress("0x1000000000000000000000000000000000000001")
	receiver2 := common.HexToAddress("0x1000000000000000000000000000000000000002")
	receiver3 := common.HexToAddress("0x1000000000000000000000000000000000000003")
	logContract := common.HexToAddress("0x2000000000000000000000000000000000000001")
	storeContract := common.HexToAddress("0x2000000000000000000000000000000000000002")
	receivers := []common.Address{receiver1, receiver2, receiver3}

	cfgCopy := *params.TestChainConfig
	cfg := &cfgCopy
	signer := types.MakeSigner(cfg, big.NewInt(1), 1)
	gasPrice := big.NewInt(225000000000)

	signLegacy := func(key *ecdsa.PrivateKey, nonce uint64, to common.Address, gas uint64, value *big.Int, data []byte) *types.Transaction {
		tx, err := types.SignTx(types.NewTransaction(nonce, to, value, gas, gasPrice, data), signer, key)
		if err != nil {
			t.Fatalf("failed to sign tx: %v", err)
		}
		return tx
	}

	txs := types.Transactions{
		signLegacy(keys[0], 0, receiver1, 21000, big.NewInt(11), nil),
		signLegacy(keys[1], 0, receiver2, 21000, big.NewInt(7), nil),
		signLegacy(keys[2], 0, logContract, 80000, big.NewInt(0), nil),
		signLegacy(keys[3], 0, storeContract, 100000, big.NewInt(0), nil),
		signLegacy(keys[4], 0, receiver3, 21000, big.NewInt(5), nil),
	}

	alloc := types.GenesisAlloc{
		logContract:   {Code: common.FromHex(logRuntimeCodeHex)},
		storeContract: {Code: common.FromHex(storeRuntimeCodeHex)},
	}
	for _, sender := range senders {
		alloc[sender] = types.Account{
			Balance: big.NewInt(9_000_000_000_000_000_000),
		}
	}

	baseline := runMixedOpsBlock(t, alloc, txs, senders, receivers, logContract, storeContract, vm.Config{
		ParallelExecutionExecutor: "sequential",
	})
	if baseline.LogCodeSize == 0 || baseline.StoreCodeSize == 0 {
		t.Fatalf("expected predeployed contracts to keep non-zero code size")
	}
	if baseline.StoreSlot0 != common.BigToHash(big.NewInt(1)) {
		t.Fatalf("expected storage slot0 to be 1, got %s", baseline.StoreSlot0)
	}
	if len(baseline.AllLogs) != 1 {
		t.Fatalf("expected one log from LOG0 contract call, got %d", len(baseline.AllLogs))
	}
	for i, nonce := range baseline.SenderNonces {
		if nonce != 1 {
			t.Fatalf("expected sender %d nonce to be 1, got %d", i, nonce)
		}
	}

	for _, workers := range []int{2, 4, 8} {
		workers := workers
		t.Run("workers_"+big.NewInt(int64(workers)).String(), func(t *testing.T) {
			for i := 0; i < 5; i++ {
				got := runMixedOpsBlock(t, alloc, txs, senders, receivers, logContract, storeContract, vm.Config{
					ParallelExecutionExecutor: "sequential-validate",
					ParallelExecutionWorkers:  workers,
				})
				assertRunEquivalent(t, baseline, got)
			}
		})
	}
}

func runMixedOpsBlock(
	t *testing.T,
	alloc types.GenesisAlloc,
	txs types.Transactions,
	senders []common.Address,
	receivers []common.Address,
	logContract common.Address,
	storeContract common.Address,
	vmCfg vm.Config,
) runView {
	t.Helper()

	cfgCopy := *params.TestChainConfig
	cfg := &cfgCopy
	db := rawdb.NewMemoryDatabase()
	gspec := &Genesis{
		Config:    cfg,
		Timestamp: uint64(upgrade.InitiallyActiveTime.Unix()),
		Alloc:     alloc,
		GasLimit:  15_000_000,
	}
	blockchain, err := NewBlockChain(db, DefaultCacheConfig, gspec, dummy.NewCoinbaseFaker(), vmCfg, common.Hash{}, false)
	if err != nil {
		t.Fatalf("failed to build blockchain: %v", err)
	}
	defer blockchain.Stop()

	parent := gspec.ToBlock()
	block := GenerateBadBlock(parent, dummy.NewCoinbaseFaker(), txs, cfg)
	statedb, err := blockchain.StateAt(parent.Root())
	if err != nil {
		t.Fatalf("failed to open parent state: %v", err)
	}

	receipts, logs, usedGas, err := blockchain.processor.Process(block, parent.Header(), statedb, vmCfg)
	if err != nil {
		t.Fatalf("process failed: %v", err)
	}

	senderNonces := make([]uint64, len(senders))
	for i, sender := range senders {
		senderNonces[i] = statedb.GetNonce(sender)
	}
	receiverBalances := make([]uint64, len(receivers))
	for i, receiver := range receivers {
		receiverBalances[i] = statedb.GetBalance(receiver).Uint64()
	}

	return runView{
		UsedGas:          usedGas,
		Receipts:         toReceiptViews(receipts),
		AllLogs:          toLogViews(logs),
		StateRoot:        statedb.IntermediateRoot(true),
		SenderNonces:     senderNonces,
		ReceiverBalances: receiverBalances,
		StoreSlot0:       statedb.GetState(storeContract, common.Hash{}),
		LogCodeSize:      statedb.GetCodeSize(logContract),
		StoreCodeSize:    statedb.GetCodeSize(storeContract),
	}
}

func toReceiptViews(receipts types.Receipts) []receiptView {
	out := make([]receiptView, len(receipts))
	for i, r := range receipts {
		out[i] = receiptView{
			Type:              r.Type,
			Status:            r.Status,
			CumulativeGasUsed: r.CumulativeGasUsed,
			GasUsed:           r.GasUsed,
			TxHash:            r.TxHash,
			ContractAddress:   r.ContractAddress,
			Logs:              toLogViews(r.Logs),
		}
	}
	return out
}

func toLogViews(logs []*types.Log) []logView {
	out := make([]logView, len(logs))
	for i, l := range logs {
		topics := make([]common.Hash, len(l.Topics))
		copy(topics, l.Topics)
		data := make([]byte, len(l.Data))
		copy(data, l.Data)
		out[i] = logView{
			Address: l.Address,
			Topics:  topics,
			Data:    data,
			TxIndex: l.TxIndex,
			Index:   l.Index,
		}
	}
	return out
}

func assertRunEquivalent(t *testing.T, baseline, got runView) {
	t.Helper()
	if baseline.UsedGas != got.UsedGas {
		t.Fatalf("usedGas mismatch: got %d want %d", got.UsedGas, baseline.UsedGas)
	}
	if baseline.StateRoot != got.StateRoot {
		t.Fatalf("state root mismatch: got %s want %s", got.StateRoot, baseline.StateRoot)
	}
	if !reflect.DeepEqual(baseline.Receipts, got.Receipts) {
		t.Fatalf("receipt mismatch")
	}
	if !reflect.DeepEqual(baseline.AllLogs, got.AllLogs) {
		t.Fatalf("allLogs mismatch")
	}
	if !reflect.DeepEqual(baseline.SenderNonces, got.SenderNonces) {
		t.Fatalf("sender nonce mismatch")
	}
	if !reflect.DeepEqual(baseline.ReceiverBalances, got.ReceiverBalances) {
		t.Fatalf("receiver balance mismatch")
	}
	if baseline.StoreSlot0 != got.StoreSlot0 {
		t.Fatalf("storage slot mismatch: got %s want %s", got.StoreSlot0, baseline.StoreSlot0)
	}
	if baseline.LogCodeSize != got.LogCodeSize || baseline.StoreCodeSize != got.StoreCodeSize {
		t.Fatalf("code size mismatch")
	}
}
