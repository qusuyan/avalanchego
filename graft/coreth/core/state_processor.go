// Copyright (C) 2019, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.
//
// This file is a derived work, based on the go-ethereum library whose original
// notices appear below.
//
// It is distributed under a license compatible with the licensing terms of the
// original code from which it is derived.
//
// Much love to the original authors for their work.
// **********
// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	stdctx "context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ava-labs/avalanchego/graft/coreth/consensus"
	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel"
	"github.com/ava-labs/avalanchego/graft/coreth/core/parallel/blockexecutor"
	"github.com/ava-labs/avalanchego/graft/coreth/params"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/state"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/core/vm"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/log"
	ethparams "github.com/ava-labs/libevm/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, parent *types.Header, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	var (
		receipts types.Receipts
		header   = block.Header()
		allLogs  []*types.Log
	)

	// Configure any upgrades that should go into effect during this block.
	blockContext := NewBlockContext(block.Number(), block.Time())
	err := ApplyUpgrades(p.config, &parent.Time, blockContext, statedb)
	if err != nil {
		log.Error("failed to configure precompiles processing block", "hash", block.Hash(), "number", block.NumberU64(), "timestamp", block.Time(), "err", err)
		return nil, nil, 0, err
	}

	var (
		context = NewEVMBlockContext(header, p.bc, nil)
		vmenv   = vm.NewEVM(context, vm.TxContext{}, statedb, p.config, cfg)
	)

	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, vmenv, statedb)
	}
	txHashes := make([]common.Hash, len(block.Transactions()))
	for i, tx := range block.Transactions() {
		txHashes[i] = tx.Hash()
	}
	blockState := parallel.NewStateDBLastWriterBlockState(statedb, txHashes)
	driver, err := newStateProcessorBlockExecutorDriver(p.config, p.bc, block, blockState, cfg)
	if err != nil {
		return nil, nil, 0, err
	}
	executor := chooseBlockExecutor(cfg)
	receipts, err = executor.Run(stdctx.Background(), driver)
	if err != nil {
		if txErr, ok := err.(*blockexecutor.TxIndexedError); ok &&
			txErr.Index >= 0 && txErr.Index < len(block.Transactions()) {
			txHash := block.Transactions()[txErr.Index].Hash().Hex()
			return nil, nil, 0, fmt.Errorf("could not apply tx %d [%v]: %w", txErr.Index, txHash, txErr.Err)
		}
		return nil, nil, 0, err
	}
	for _, receipt := range receipts {
		allLogs = append(allLogs, receipt.Logs...)
	}
	var usedGas uint64
	if len(receipts) > 0 {
		usedGas = receipts[len(receipts)-1].CumulativeGasUsed
	}
	if err := blockState.WriteBack(); err != nil {
		return nil, nil, 0, fmt.Errorf("failed to write back block state: %w", err)
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	if err := p.engine.Finalize(p.bc, block, parent, statedb, receipts); err != nil {
		return nil, nil, 0, fmt.Errorf("engine finalization check failed: %w", err)
	}

	return receipts, allLogs, usedGas, nil
}

func chooseBlockExecutor(cfg vm.Config) blockexecutor.Executor {
	execType := strings.ToLower(strings.TrimSpace(cfg.ParallelExecutionExecutor))
	switch execType {
	case "", blockexecutor.ExecutorTypeSequential:
		return blockexecutor.NewSequentialExecutor()
	case blockexecutor.ExecutorTypeSequentialValidate:
		return blockexecutor.NewSequentialValidateExecutor(blockexecutor.Config{
			Workers: cfg.ParallelExecutionWorkers,
		})
	default:
		// Keep behavior predictable on invalid config: fallback to sequential.
		return blockexecutor.NewSequentialExecutor()
	}
}

func applyTransaction(msg *Message, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (*types.Receipt, error) {
	// Create a new context to be used in the EVM environment.
	txContext := NewEVMTxContext(msg)
	evm.Reset(txContext, statedb)

	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(evm, msg, gp)
	if err != nil {
		return nil, err
	}

	var root []byte
	if config.IsByzantium(blockNumber) {
		statedb.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(config.IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: *usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * ethparams.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if msg.To == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt, err
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(config *params.ChainConfig, bc ChainContext, blockContext vm.BlockContext, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, cfg vm.Config) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(config, header.Number, header.Time), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	txContext := NewEVMTxContext(msg)
	vmenv := vm.NewEVM(blockContext, txContext, statedb, config, cfg)
	return applyTransaction(msg, config, gp, statedb, header.Number, header.Hash(), tx, usedGas, vmenv)
}

// ProcessBeaconBlockRoot applies the EIP-4788 system call to the beacon block root
// contract. This method is exported to be used in tests.
func ProcessBeaconBlockRoot(beaconRoot common.Hash, vmenv *vm.EVM, statedb *state.StateDB) {
	// If EIP-4788 is enabled, we need to invoke the beaconroot storage contract with
	// the new root
	msg := &Message{
		From:      ethparams.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &ethparams.BeaconRootsStorageAddress,
		Data:      beaconRoot[:],
	}
	vmenv.Reset(NewEVMTxContext(msg), statedb)
	statedb.AddAddressToAccessList(ethparams.BeaconRootsStorageAddress)
	_, _, _ = vmenv.Call(vm.AccountRef(msg.From), *msg.To, msg.Data, 30_000_000, common.U2560)
	statedb.Finalise(true)
}
