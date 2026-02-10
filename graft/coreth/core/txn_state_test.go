package core

import (
	"testing"

	"github.com/ava-labs/libevm/common"
	"github.com/holiman/uint256"
)

func TestTxnStateReadOwnWrites(t *testing.T) {
	tx := NewTxnState(nil, common.HexToHash("0x1234"), 2)
	addr := common.HexToAddress("0xabc")
	slot := common.HexToHash("0x2")
	slot2 := common.HexToHash("0x3")

	tx.SetBalance(addr, uint256.NewInt(99))
	tx.SetNonce(addr, 7)
	tx.SetCode(addr, []byte{0x60, 0x00})
	tx.SetState(addr, slot, common.HexToHash("0x55"))
	tx.SetState(addr, slot2, common.HexToHash("0x66"))

	if got := tx.GetBalance(addr); got.Cmp(uint256.NewInt(99)) != 0 {
		t.Fatalf("unexpected balance: %s", got)
	}
	if got := tx.GetNonce(addr); got != 7 {
		t.Fatalf("unexpected nonce: %d", got)
	}
	if got := tx.GetState(addr, slot); got != common.HexToHash("0x55") {
		t.Fatalf("unexpected state value: %s", got)
	}
	if got := tx.GetState(addr, slot2); got != common.HexToHash("0x66") {
		t.Fatalf("unexpected state value for second slot: %s", got)
	}
	if got := tx.GetCodeSize(addr); got != 2 {
		t.Fatalf("unexpected code size: %d", got)
	}
	if len(tx.readSet) != 0 {
		t.Fatalf("expected no base reads when reading own writes, got %d", len(tx.readSet))
	}
	if got := len(tx.storageWrites[addr]); got != 2 {
		t.Fatalf("expected two tracked storage entries, got %d", got)
	}
}

func TestTxnStateValidatePhase1AlwaysTrue(t *testing.T) {
	tx := NewTxnState(nil, common.HexToHash("0x1"), 1)
	if !tx.Validate() {
		t.Fatalf("expected Validate() to return true in phase-1 direct wrapper")
	}
}
