package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/bitbeamer/dfs/internal/repository"
	"github.com/bitbeamer/dfs/internal/wakeup"
)

func TestLocalPinPersistsPolicyAndSchedulesBackgroundHydration(t *testing.T) {
	if err := repository.CheckDependencies(); err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, err := repository.Init(ctx, t.TempDir(), "pin-test", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	listener, err := wakeup.Listen(repo.Config.Repository)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	reasons := make(chan string, 1)
	go func() {
		reason, _ := listener.Receive()
		reasons <- reason
	}()
	var output bytes.Buffer
	app := &App{Out: &output, Err: &output, repo: repo.Config.Repository}
	command := app.pinCommand()
	command.SetArgs([]string{"Photos"})
	if err := command.ExecuteContext(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case reason := <-reasons:
		if reason != "pin policy changed" {
			t.Fatalf("wakeup reason = %q", reason)
		}
	case <-ctx.Done():
		t.Fatal("pin command did not wake the mount daemon")
	}
	pins, err := repo.Store.PinRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 1 || pins[0].Path != "Photos" || pins[0].Scope != "local" {
		t.Fatalf("saved pins = %+v", pins)
	}
	if !bytes.Contains(output.Bytes(), []byte("background hydration scheduled")) {
		t.Fatalf("pin output = %q", output.String())
	}
}
