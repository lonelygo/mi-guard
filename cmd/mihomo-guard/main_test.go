package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestInterruptReportsFailedRestoration(t *testing.T) {
	cancelled := fmt.Errorf("measurement: %w", context.Canceled)
	if !cleanInterrupt(errors.Join(cancelled, context.Canceled)) {
		t.Fatal("plain cancellation should exit cleanly")
	}
	if cleanInterrupt(errors.Join(cancelled, errors.New("could not restore selector"))) {
		t.Fatal("restoration failure hidden by Ctrl+C")
	}
}
