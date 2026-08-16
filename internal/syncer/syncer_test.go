package syncer

import (
	"context"
	"testing"
)

func TestSyncUntilConvergedStopsImmediatelyWhenUnchanged(t *testing.T) {
	passes := 0
	tree := "initial"
	got, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return tree, nil },
		func(context.Context) error { passes++; return nil },
	)
	if err != nil || got != 1 || passes != 1 {
		t.Fatalf("syncUntilConverged = passes %d/%d, error %v", got, passes, err)
	}
}

func TestSyncUntilConvergedRequiresTwoStablePassesAfterChange(t *testing.T) {
	sequence := []string{"initial", "remote-change", "remote-change", "remote-change"}
	position := 0
	got, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return sequence[position], nil },
		func(context.Context) error { position++; return nil },
	)
	if err != nil || got != 3 || position != 3 {
		t.Fatalf("syncUntilConverged = passes %d, position %d, error %v", got, position, err)
	}
}

func TestSyncUntilConvergedFailsWhenTreeKeepsChanging(t *testing.T) {
	position := 0
	_, err := syncUntilConverged(context.Background(), 3, 2,
		func(context.Context) (string, error) { return string(rune('a' + position)), nil },
		func(context.Context) error { position++; return nil },
	)
	if err == nil {
		t.Fatal("syncUntilConverged unexpectedly accepted an unstable tree")
	}
}
