// Command ccc runs Claude Code in a container, so that ~/.claude can be
// swapped per account without touching the host's real configuration.
package main

import (
	"fmt"
	"os"

	"github.com/lestrrat-go/ccc/internal/cli"
)

func main() {
	if err := cli.Run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "ccc: %s\n", err)
		os.Exit(1)
	}
}
