package blockexecutor

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ava-labs/libevm/core/types"
)

type testDriver struct {
	txCount int

	mu sync.Mutex

	execCalls        map[int]int
	validateCalls    map[int]int
	commitOrder      []int
	failValidateOnce map[int]bool

	execHook func(ctx context.Context, txIndex int) error
}

func newTestDriver(txCount int, failValidateOnce map[int]bool) *testDriver {
	return &testDriver{
		txCount:          txCount,
		execCalls:        make(map[int]int),
		validateCalls:    make(map[int]int),
		failValidateOnce: failValidateOnce,
	}
}

func (d *testDriver) TxCount() int { return d.txCount }

func (d *testDriver) Execute(ctx context.Context, txIndex int) error {
	if d.execHook != nil {
		return d.execHook(ctx, txIndex)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.execCalls[txIndex]++
	return nil
}

func (d *testDriver) Validate(txIndex int) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.validateCalls[txIndex]++
	if d.failValidateOnce[txIndex] {
		delete(d.failValidateOnce, txIndex)
		return false, nil
	}
	return true, nil
}

func (d *testDriver) Commit(txIndex int) (*types.Receipt, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.commitOrder = append(d.commitOrder, txIndex)
	logs := []*types.Log{{TxIndex: uint(txIndex)}}
	receipt := &types.Receipt{
		TransactionIndex: uint(txIndex),
		Logs:             logs,
	}
	return receipt, nil
}

func TestSequentialValidateExecutorCommitOrder(t *testing.T) {
	drv := newTestDriver(6, map[int]bool{})
	ex := NewSequentialValidateExecutor(Config{Workers: 4})

	receipts, err := ex.Run(context.Background(), drv)
	if err != nil {
		t.Fatalf("executor run failed: %v", err)
	}
	if len(receipts) != 6 {
		t.Fatalf("unexpected receipt length: got %d want %d", len(receipts), 6)
	}
	if len(drv.commitOrder) != 6 {
		t.Fatalf("unexpected commit length: got %d want %d", len(drv.commitOrder), 6)
	}
	for i, got := range drv.commitOrder {
		if got != i {
			t.Fatalf("unexpected commit order at %d: got %d want %d", i, got, i)
		}
	}
}

func TestSequentialValidateExecutorDirectRerunOnValidationFailure(t *testing.T) {
	drv := newTestDriver(5, map[int]bool{2: true})
	ex := NewSequentialValidateExecutor(Config{Workers: 3})

	receipts, err := ex.Run(context.Background(), drv)
	if err != nil {
		t.Fatalf("executor run failed: %v", err)
	}
	if len(receipts) != 5 {
		t.Fatalf("unexpected receipt length: got %d want %d", len(receipts), 5)
	}

	if drv.execCalls[2] != 2 {
		t.Fatalf("expected tx 2 to be re-executed once, got execute count %d", drv.execCalls[2])
	}
	wantOrder := []int{0, 1, 2, 3, 4}
	if len(drv.commitOrder) != len(wantOrder) {
		t.Fatalf("unexpected commit length: got %d want %d", len(drv.commitOrder), len(wantOrder))
	}
	for i := range wantOrder {
		if drv.commitOrder[i] != wantOrder[i] {
			t.Fatalf("unexpected commit order at %d: got %d want %d", i, drv.commitOrder[i], wantOrder[i])
		}
	}
}

func TestSequentialValidateExecutorPropagatesExecutionError(t *testing.T) {
	drv := newTestDriver(3, map[int]bool{})
	ex := NewSequentialValidateExecutor(Config{Workers: 2})

	drv.execHook = func(_ context.Context, txIndex int) error {
		if txIndex == 1 {
			return fmt.Errorf("boom")
		}
		drv.mu.Lock()
		defer drv.mu.Unlock()
		drv.execCalls[txIndex]++
		return nil
	}

	_, err := ex.Run(context.Background(), drv)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

func TestSequentialExecutorCommitOrder(t *testing.T) {
	drv := newTestDriver(4, map[int]bool{})
	ex := NewSequentialExecutor()

	receipts, err := ex.Run(context.Background(), drv)
	if err != nil {
		t.Fatalf("executor run failed: %v", err)
	}
	if len(receipts) != 4 {
		t.Fatalf("unexpected receipt length: got %d want %d", len(receipts), 4)
	}
	if len(drv.validateCalls) != 0 {
		t.Fatalf("sequential executor should not call Validate, got %d calls", len(drv.validateCalls))
	}
	wantOrder := []int{0, 1, 2, 3}
	for i := range wantOrder {
		if drv.commitOrder[i] != wantOrder[i] {
			t.Fatalf("unexpected commit order at %d: got %d want %d", i, drv.commitOrder[i], wantOrder[i])
		}
	}
}

func TestSequentialExecutorIgnoresValidateFailures(t *testing.T) {
	drv := newTestDriver(3, map[int]bool{1: true})
	ex := NewSequentialExecutor()

	_, err := ex.Run(context.Background(), drv)
	if err != nil {
		t.Fatalf("executor run failed: %v", err)
	}
	if drv.execCalls[1] != 1 {
		t.Fatalf("expected tx 1 to execute once, got %d", drv.execCalls[1])
	}
	if len(drv.validateCalls) != 0 {
		t.Fatalf("sequential executor should not call Validate, got %d calls", len(drv.validateCalls))
	}
}
