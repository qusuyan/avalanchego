package blockexecutor

import (
	"context"
	"fmt"

	"github.com/ava-labs/libevm/core/types"
)

// SequentialExecutor executes transactions in strict tx-index order.
// It shares the same Driver contract as parallel executors so state_processor
// can switch executor implementations without changing driver logic.
type SequentialExecutor struct{}

func NewSequentialExecutor() *SequentialExecutor {
	return &SequentialExecutor{}
}

func (e *SequentialExecutor) Run(ctx context.Context, d Driver) (types.Receipts, []*types.Log, error) {
	txCount := d.TxCount()
	if txCount == 0 {
		return nil, nil, nil
	}

	receipts := make(types.Receipts, txCount)
	allLogs := make([]*types.Log, 0)
	workerCtx := WithWorkerID(ctx, 0)

	for i := 0; i < txCount; i++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}

		if err := d.Execute(workerCtx, i); err != nil {
			return nil, nil, fmt.Errorf("tx %d speculative execution failed: %w", i, err)
		}
		receipt, logs, err := d.Commit(i)
		if err != nil {
			return nil, nil, fmt.Errorf("tx %d commit failed: %w", i, err)
		}
		receipts[i] = receipt
		allLogs = append(allLogs, logs...)
	}

	return receipts, allLogs, nil
}

var _ Executor = (*SequentialExecutor)(nil)
