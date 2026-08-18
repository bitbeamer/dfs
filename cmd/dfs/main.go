package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/bitbeamer/dfs/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		var jsonError *cli.JSONCommandError
		if !errors.As(err, &jsonError) {
			fmt.Fprintln(os.Stderr, "dfs:", err)
		}
		os.Exit(1)
	}
}
