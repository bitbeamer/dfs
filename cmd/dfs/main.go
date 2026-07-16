package main

import (
	"fmt"
	"os"

	"github.com/bitbeamer/dfs/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "dfs:", err)
		os.Exit(1)
	}
}
