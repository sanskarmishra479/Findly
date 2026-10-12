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
	case "index":
		err = runIndex(os.Args[2:])
	case "search":
		err = runSearch(os.Args[2:])
	case "stats":
		err = runStats(os.Args[2:])
	case "remove":
		err = runRemove(os.Args[2:])
	case "eval":
		err = runEval(os.Args[2:])
	case "reset":
		err = runReset(os.Args[2:])
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
	fmt.Fprintln(os.Stderr, "  index   <folder>      add a folder and index it")
	fmt.Fprintln(os.Stderr, "  search  \"<query>\"     search by words and meaning")
	fmt.Fprintln(os.Stderr, "  stats                 show what is indexed")
	fmt.Fprintln(os.Stderr, "  remove  <folder>      remove a folder and its index data")
	fmt.Fprintln(os.Stderr, "  reset                 delete a damaged index so it can be rebuilt")
	fmt.Fprintln(os.Stderr, "  eval                  run evals/queries.jsonl on the eval testbed")
	fmt.Fprintln(os.Stderr, "  spike   Phase 0: embed test text/images/audio and print similarities")
	fmt.Fprintln(os.Stderr, "  compare Compare two spike vector files (CPU vs GPU check)")
}
