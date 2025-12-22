package main

import (
	"testing"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/network"
)

func TestNewDegrader(t *testing.T) {
	d := network.NewDegrader()
	if d == nil {
		t.Fatal("expected non-nil degrader")
	}
}

func TestApplyPacketLossValidation(t *testing.T) {
	d := network.NewDegrader()

	tests := []int{-1, 101}
	for _, loss := range tests {
		if err := d.ApplyPacketLoss(loss, 0); err == nil {
			t.Fatalf("expected error for loss=%d", loss)
		}
	}
}

func TestApplyLatencyDoesNotPanic(t *testing.T) {
	d := network.NewDegrader()

	// We don't assert success - only that validation & locking work
	_ = d.ApplyLatency(100, 10, 0)
}

func TestApplyBandwidthDoesNotPanic(t *testing.T) {
	d := network.NewDegrader()

	_ = d.ApplyBandwidthLimit(1000, 0)
}

func TestIsActiveInitiallyFalse(t *testing.T) {
	d := network.NewDegrader()

	if d.IsActive() {
		t.Fatal("expected degrader to be inactive initially")
	}
}

func TestRemoveAllDoesNotPanic(t *testing.T) {
	d := network.NewDegrader()

	if err := d.RemoveAll(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
