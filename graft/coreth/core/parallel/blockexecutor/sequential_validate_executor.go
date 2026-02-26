package blockexecutor

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ava-labs/libevm/core/types"
)

type txSlot struct {
	mu                   sync.Mutex
	ready                bool
	claimedForValidation bool
	err                  error
}

func (s *txSlot) publish(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	s.ready = true
}

func (s *txSlot) tryClaimValidation() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || s.claimedForValidation {
		return false
	}
	s.claimedForValidation = true
	return true
}

func (s *txSlot) unclaimValidation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimedForValidation = false
}

func (s *txSlot) consumeClaimed() (error, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready || !s.claimedForValidation {
		return nil, false
	}
	err := s.err
	s.ready = false
	s.claimedForValidation = false
	s.err = nil
	return err, true
}

// SequentialValidateExecutor implements:
// 1) shared worker pool for execute+validate tasks
// 2) validation priority over execution
// 3) strictly sequential validation/commit by tx index
// 4) direct re-execute+commit on validation failure
type SequentialValidateExecutor struct {
	cfg Config
}

func NewSequentialValidateExecutor(cfg Config) *SequentialValidateExecutor {
	return &SequentialValidateExecutor{cfg: cfg}
}

func (e *SequentialValidateExecutor) Run(ctx context.Context, d Driver) (types.Receipts, []*types.Log, error) {
	txCount := d.TxCount()
	if txCount == 0 {
		return nil, nil, nil
	}
	workers := e.cfg.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}

	slots := make([]txSlot, txCount)
	receipts := make(types.Receipts, txCount)
	allLogs := make([]*types.Log, 0)

	var (
		nextExecToIssue      atomic.Uint64
		nextValidateToCommit atomic.Uint64
		runErr               atomic.Pointer[error]
		stop                 atomic.Bool
		validationMu         sync.Mutex
		wg                   sync.WaitGroup
	)

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
			if slots[committed].tryClaimValidation() {
				validationMu.Lock()
				current := int(nextValidateToCommit.Load())
				if current != committed {
					slots[committed].unclaimValidation()
					validationMu.Unlock()
					continue
				}

				execErr, ok := slots[committed].consumeClaimed()
				if !ok {
					validationMu.Unlock()
					continue
				}
				if execErr != nil {
					validationMu.Unlock()
					storeErr(fmt.Errorf("tx %d speculative execution failed: %w", committed, execErr))
					return
				}

				valid, err := d.Validate(committed)
				if err != nil {
					validationMu.Unlock()
					storeErr(fmt.Errorf("tx %d validation failed with error: %w", committed, err))
					return
				}

				if valid {
					receipt, logs, err := d.Commit(committed)
					if err != nil {
						validationMu.Unlock()
						storeErr(fmt.Errorf("tx %d commit failed: %w", committed, err))
						return
					}
					receipts[committed] = receipt
					allLogs = append(allLogs, logs...)
				} else {
					if err := d.Execute(workerCtx, committed); err != nil {
						validationMu.Unlock()
						storeErr(fmt.Errorf("tx %d re-execute failed: %w", committed, err))
						return
					}
					receipt, logs, err := d.Commit(committed)
					if err != nil {
						validationMu.Unlock()
						storeErr(fmt.Errorf("tx %d re-execute commit failed: %w", committed, err))
						return
					}
					receipts[committed] = receipt
					allLogs = append(allLogs, logs...)
				}
				nextValidateToCommit.Add(1)
				validationMu.Unlock()
				continue
			}

			i := int(nextExecToIssue.Add(1) - 1)
			if i >= txCount {
				runtime.Gosched()
				continue
			}
			err := d.Execute(workerCtx, i)
			slots[i].publish(err)
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go worker(i)
	}
	wg.Wait()

	if err := runErr.Load(); err != nil {
		return nil, nil, *err
	}
	if got := int(nextValidateToCommit.Load()); got != txCount {
		return nil, nil, fmt.Errorf("executor ended before all txs committed: committed=%d total=%d", got, txCount)
	}
	return receipts, allLogs, nil
}

var _ Executor = (*SequentialValidateExecutor)(nil)
