// Command cli is the Findly developer tool: model spike, and later index, search, eval and bench.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "spike":
		err = runSpike(os.Args[2:])
	case "compare":
		err = runCompare(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cli <command> [flags]")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  spike   Phase 0: embed test text/images/audio and print similarities")
	fmt.Fprintln(os.Stderr, "  compare Compare two spike vector files (CPU vs GPU check)")
}
