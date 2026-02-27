package blockexecutor

import (
	"context"
	"fmt"

	"github.com/ava-labs/libevm/core/types"
)

const (
	ExecutorTypeSequential         = "sequential"
	ExecutorTypeSequentialValidate = "sequential-validate"
)

type Executor interface {
	Run(ctx context.Context, d Driver) (types.Receipts, error)
}

// Driver abstracts block execution operations so scheduler logic can be reused
// by multiple concrete executor designs.
type Driver interface {
	TxCount() int
	Execute(ctx context.Context, txIndex int) error
	Validate(txIndex int) (bool, error)
	Commit(txIndex int) (*types.Receipt, error)
}

type Config struct {
	Workers int
}

type TxIndexedError struct {
	Index int
	Err   error
}

func (e *TxIndexedError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("tx %d: %v", e.Index, e.Err)
}

func (e *TxIndexedError) Unwrap() error { return e.Err }
