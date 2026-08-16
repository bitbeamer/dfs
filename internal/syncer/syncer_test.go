package syncer

import (
	"context"
	"testing"
)

func TestSyncUntilConvergedStopsImmediatelyWhenUnchanged(t *testing.T) {
	passes := 0
	tree := "initial"
	got, paths, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return tree, nil },
		func(context.Context, string, string) ([]string, error) { return nil, nil },
		func(context.Context) error { passes++; return nil },
	)
	if err != nil || got != 1 || passes != 1 || len(paths) != 0 {
		t.Fatalf("syncUntilConverged = passes %d/%d, error %v", got, passes, err)
	}
}

func TestSyncUntilConvergedRequiresTwoStablePassesAfterChange(t *testing.T) {
	sequence := []string{"initial", "remote-change", "remote-change", "remote-change"}
	position := 0
	got, paths, err := syncUntilConverged(context.Background(), 4, 2,
		func(context.Context) (string, error) { return sequence[position], nil },
		func(_ context.Context, before, after string) ([]string, error) {
			return []string{before + "-to-" + after}, nil
		},
		func(context.Context) error { position++; return nil },
	)
	if err != nil || got != 3 || position != 3 || len(paths) != 1 || paths[0] != "initial-to-remote-change" {
		t.Fatalf("syncUntilConverged = passes %d, position %d, error %v", got, position, err)
	}
}

func TestSyncUntilConvergedFailsWhenTreeKeepsChanging(t *testing.T) {
	position := 0
	_, _, err := syncUntilConverged(context.Background(), 3, 2,
		func(context.Context) (string, error) { return string(rune('a' + position)), nil },
		func(context.Context, string, string) ([]string, error) { return nil, nil },
		func(context.Context) error { position++; return nil },
	)
	if err == nil {
		t.Fatal("syncUntilConverged unexpectedly accepted an unstable tree")
	}
}
