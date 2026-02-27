package blockexecutor

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ava-labs/libevm/core/types"
)

const (
	slotIdle uint32 = iota
	slotReady
	slotClaimed
)

// SequentialValidateExecutor implements:
// 1) shared worker pool for execute+validate tasks
// 2) validation priority over execution
// 3) strictly sequential validation/commit by tx index
// 4) direct re-execute+commit on validation failure
type SequentialValidateExecutor struct {
	cfg Config
}

func NewSequentialValidateExecutor(cfg Config) *SequentialValidateExecutor {
	return &SequentialValidateExecutor{
		cfg: cfg,
	}
}

func (e *SequentialValidateExecutor) Run(ctx context.Context, d Driver) (types.Receipts, error) {
	txCount := d.TxCount()
	if txCount == 0 {
		return nil, nil
	}
	workers := e.cfg.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	var (
		nextExecToIssue      atomic.Uint64
		nextValidateToCommit atomic.Uint64
		runErr               atomic.Pointer[error]
		stop                 atomic.Bool
		wg                   sync.WaitGroup
	)
	slots := make([]atomic.Uint32, txCount)
	receipts := make(types.Receipts, txCount)

	storeErr := func(err error) {
		if err == nil {
			return
		}
		errCopy := err
		runErr.CompareAndSwap(nil, &errCopy)
		stop.Store(true)
	}

	worker := func(workerID int) {
		defer wg.Done()
		workerCtx := WithWorkerID(ctx, workerID)
		for {
			if stop.Load() {
				return
			}
			if err := ctx.Err(); err != nil {
				storeErr(err)
				return
			}
			committed := int(nextValidateToCommit.Load())
			if committed >= txCount {
				return
			}

			// Always prioritize validation.
			if slots[committed].CompareAndSwap(slotReady, slotClaimed) {
				valid, err := d.Validate(committed)
				if err != nil {
					storeErr(&TxIndexedError{Index: committed, Err: err})
					return
				}

				if valid {
					receipt, err := d.Commit(committed)
					if err != nil {
						storeErr(&TxIndexedError{Index: committed, Err: err})
						return
					}
					receipts[committed] = receipt
				} else {
					if err := d.Execute(workerCtx, committed); err != nil {
						storeErr(&TxIndexedError{Index: committed, Err: err})
						return
					}
					receipt, err := d.Commit(committed)
					if err != nil {
						storeErr(&TxIndexedError{Index: committed, Err: err})
						return
					}
					receipts[committed] = receipt
				}
				nextValidateToCommit.Add(1)
				continue
			}

			// No validation task available - try to claim an execution task.
			if nextExecToIssue.Load() >= uint64(txCount) {
				runtime.Gosched()
				continue
			}
			i := int(nextExecToIssue.Add(1) - 1)
			if i >= txCount {
				runtime.Gosched()
				continue
			}
			err := d.Execute(workerCtx, i)
			if err != nil {
				storeErr(&TxIndexedError{Index: i, Err: err})
				return
			}
			slots[i].Store(slotReady)
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker(i)
	}
	wg.Wait()

	if err := runErr.Load(); err != nil {
		return nil, *err
	}
	if got := int(nextValidateToCommit.Load()); got != txCount {
		return nil, fmt.Errorf("executor ended before all txs committed: committed=%d total=%d", got, txCount)
	}
	return receipts, nil
}

var _ Executor = (*SequentialValidateExecutor)(nil)
