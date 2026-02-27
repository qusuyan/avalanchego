package blockexecutor

import (
	"context"

	"github.com/ava-labs/libevm/core/types"
)

// SequentialExecutor executes transactions in strict tx-index order.
// It shares the same Driver contract as parallel executors so state_processor
// can switch executor implementations without changing driver logic.
type SequentialExecutor struct{}

func NewSequentialExecutor() *SequentialExecutor {
	return &SequentialExecutor{}
}

func (e *SequentialExecutor) Run(ctx context.Context, d Driver) (types.Receipts, error) {
	txCount := d.TxCount()
	if txCount == 0 {
		return nil, nil
	}

	receipts := make(types.Receipts, txCount)
	workerCtx := WithWorkerID(ctx, 0)

	for i := 0; i < txCount; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if err := d.Execute(workerCtx, i); err != nil {
			return nil, &TxIndexedError{Index: i, Err: err}
		}
		receipt, err := d.Commit(i)
		if err != nil {
			return nil, &TxIndexedError{Index: i, Err: err}
		}
		receipts[i] = receipt
	}

	return receipts, nil
}

var _ Executor = (*SequentialExecutor)(nil)
