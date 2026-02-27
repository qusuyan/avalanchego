package core

import (
	"context"
	"fmt"
	"math/big"
	"runtime"
	"sync/atomic"

	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel"
	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel/blockexecutor"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	ethparams "github.com/ava-labs/libevm/params"
)

type txExecutionResult struct {
	txState *parallel.TxnState
	receipt *types.Receipt
	gasUsed uint64
}

type txExecutionSlot struct {
	state  atomic.Uint32
	result atomic.Pointer[txExecutionResult]
}

const (
	txSlotIdle uint32 = iota
	txSlotRunning
	txSlotReady
)

type stateProcessorBlockExecutorDriver struct {
	config *params.ChainConfig
	vmCfg  vm.Config

	txs         types.Transactions
	messages    []*Message
	blockNumber *big.Int
	blockHash   common.Hash
	blockCtx    vm.BlockContext
	blockState  parallel.BlockState

	slots []txExecutionSlot

	initialGasLimit uint64
	remainingGas    atomic.Uint64

	evmByWorker []*vm.EVM
}

func newStateProcessorBlockExecutorDriver(
	config *params.ChainConfig,
	chain ChainContext,
	block *types.Block,
	blockState parallel.BlockState,
	vmCfg vm.Config,
) (*stateProcessorBlockExecutorDriver, error) {
	header := block.Header()
	signer := types.MakeSigner(config, header.Number, header.Time)

	txs := block.Transactions()
	messages := make([]*Message, len(txs))
	for i, tx := range txs {
		msg, err := TransactionToMessage(tx, signer, header.BaseFee)
		if err != nil {
			return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
		}
		messages[i] = msg
	}

	d := &stateProcessorBlockExecutorDriver{
		config:          config,
		vmCfg:           vmCfg,
		txs:             txs,
		messages:        messages,
		blockNumber:     new(big.Int).Set(header.Number),
		blockHash:       block.Hash(),
		blockCtx:        NewEVMBlockContext(header, chain, nil),
		blockState:      blockState,
		slots:           make([]txExecutionSlot, len(txs)),
		evmByWorker:     make([]*vm.EVM, normalizeWorkerCount(vmCfg.ParallelExecutionWorkers)),
		initialGasLimit: header.GasLimit,
	}
	d.initWorkerEVMs()
	d.remainingGas.Store(header.GasLimit)
	return d, nil
}

func (d *stateProcessorBlockExecutorDriver) TxCount() int {
	return len(d.txs)
}

func (d *stateProcessorBlockExecutorDriver) Execute(ctx context.Context, txIndex int) error {
	if txIndex < 0 || txIndex >= len(d.txs) {
		return fmt.Errorf("tx index %d out of range", txIndex)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	tx := d.txs[txIndex]
	msg := d.messages[txIndex]
	slot := &d.slots[txIndex]
	for {
		state := slot.state.Load()
		if state == txSlotRunning {
			return fmt.Errorf("tx %d already has a running task", txIndex)
		}
		if state == txSlotIdle || state == txSlotReady {
			if slot.state.CompareAndSwap(state, txSlotRunning) {
				break
			}
			continue
		}
		return fmt.Errorf("tx %d has invalid slot state %d", txIndex, state)
	}

	txState := parallel.NewTxnState(d.blockState, tx.Hash(), txIndex, 1)

	remainingGasSnapshot := d.remainingGas.Load()
	gp := new(GasPool).AddGas(remainingGasSnapshot)
	evm := d.acquireWorkerEVM(ctx, txState, msg)
	evm.Reset(NewEVMTxContext(msg), txState)

	result, err := ApplyMessage(evm, msg, gp)
	if err != nil {
		slot.state.Store(txSlotIdle)
		return err
	}

	receipt := &types.Receipt{
		Type: tx.Type(),
		// CumulativeGasUsed is assigned deterministically at commit time.
		GasUsed:          result.UsedGas,
		TxHash:           tx.Hash(),
		BlockHash:        d.blockHash,
		BlockNumber:      d.blockNumber,
		TransactionIndex: uint(txIndex),
	}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * ethparams.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = d.blockCtx.BlobBaseFee
	}
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	receipt.Logs = txState.GetLogs(tx.Hash(), d.blockNumber.Uint64(), d.blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})

	execResult := &txExecutionResult{
		txState: txState,
		receipt: receipt,
		gasUsed: result.UsedGas,
	}
	slot.result.Store(execResult)
	slot.state.Store(txSlotReady)

	return nil
}

func (d *stateProcessorBlockExecutorDriver) Validate(txIndex int) (bool, error) {
	if txIndex < 0 || txIndex >= len(d.txs) {
		return false, fmt.Errorf("tx index %d out of range", txIndex)
	}
	slot := &d.slots[txIndex]
	if slot.state.Load() != txSlotReady {
		return false, fmt.Errorf("tx %d is not ready for validation", txIndex)
	}
	result := slot.result.Load()
	if result == nil {
		return false, fmt.Errorf("tx %d has no execution result to validate", txIndex)
	}
	// validate block has enough gas
	if result.gasUsed > d.remainingGas.Load() {
		return false, ErrGasLimitReached
	}
	return d.blockState.ValidateReadSet(result.txState.ReadSet()), nil
}

func (d *stateProcessorBlockExecutorDriver) Commit(txIndex int) (*types.Receipt, error) {
	if txIndex < 0 || txIndex >= len(d.txs) {
		return nil, fmt.Errorf("tx index %d out of range", txIndex)
	}
	slot := &d.slots[txIndex]
	if !slot.state.CompareAndSwap(txSlotReady, txSlotRunning) {
		return nil, fmt.Errorf("tx %d is not ready for commit", txIndex)
	}
	result := slot.result.Load()
	if result == nil {
		slot.state.Store(txSlotReady)
		return nil, fmt.Errorf("tx %d has no execution result to commit", txIndex)
	}

	remainingAfter, err := d.consumeGas(result.gasUsed)
	if err != nil {
		slot.state.Store(txSlotReady)
		return nil, err
	}
	if err := result.txState.CommitTxn(); err != nil {
		slot.state.Store(txSlotReady)
		return nil, err
	}

	result.receipt.CumulativeGasUsed = d.initialGasLimit - remainingAfter
	slot.result.Store(nil)
	slot.state.Store(txSlotIdle)

	return result.receipt, nil
}

func (d *stateProcessorBlockExecutorDriver) consumeGas(gasUsed uint64) (uint64, error) {
	for {
		remaining := d.remainingGas.Load()
		if gasUsed > remaining {
			return 0, ErrGasLimitReached
		}
		remainingAfter := remaining - gasUsed
		if d.remainingGas.CompareAndSwap(remaining, remainingAfter) {
			return remainingAfter, nil
		}
	}
}

func (d *stateProcessorBlockExecutorDriver) acquireWorkerEVM(ctx context.Context, txState *parallel.TxnState, msg *Message) *vm.EVM {
	workerID, ok := blockexecutor.WorkerIDFromContext(ctx)
	if !ok {
		workerID = 0
	}
	if len(d.evmByWorker) == 0 {
		return vm.NewEVM(d.blockCtx, NewEVMTxContext(msg), txState, d.config, d.vmCfg)
	}
	if workerID < 0 || workerID >= len(d.evmByWorker) {
		workerID = 0
	}
	return d.evmByWorker[workerID]
}

func normalizeWorkerCount(configured int) int {
	if configured > 0 {
		return configured
	}
	n := runtime.GOMAXPROCS(0)
	if n <= 0 {
		return 1
	}
	return n
}

func (d *stateProcessorBlockExecutorDriver) initWorkerEVMs() {
	// Use one lightweight placeholder tx-state to initialize worker-scoped EVMs.
	placeholder := parallel.NewTxnState(d.blockState, common.Hash{}, 0, 1)
	for i := range d.evmByWorker {
		d.evmByWorker[i] = vm.NewEVM(d.blockCtx, vm.TxContext{}, placeholder, d.config, d.vmCfg)
	}
}

var _ blockexecutor.Driver = (*stateProcessorBlockExecutorDriver)(nil)
