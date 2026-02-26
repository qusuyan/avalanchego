package blockexecutor

import (
	"context"

	"github.com/ava-labs/libevm/core/types"
)

type Executor interface {
	Run(ctx context.Context, d Driver) (types.Receipts, []*types.Log, error)
}

// Driver abstracts block execution operations so scheduler logic can be reused
// by multiple concrete executor designs.
type Driver interface {
	TxCount() int
	Execute(ctx context.Context, txIndex int) error
	Validate(txIndex int) (bool, error)
	Commit(txIndex int) (*types.Receipt, []*types.Log, error)
}

type Config struct {
	Workers int
}
