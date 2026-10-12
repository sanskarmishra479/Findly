package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// runCompare prints the cosine between matching vectors in two spike output files.
// Used to check that a GPU backend gives the same vectors as the CPU reference (plan 14.2).
func runCompare(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: cli compare <reference.json> <other.json>")
	}
	ref, err := loadVectors(args[0])
	if err != nil {
		return err
	}
	other, err := loadVectors(args[1])
	if err != nil {
		return err
	}

	if len(ref) == 0 {
		return fmt.Errorf("%s has no vectors", args[0])
	}
	names := make([]string, 0, len(ref))
	for name := range ref {
		names = append(names, name)
	}
	sort.Strings(names)

	worst := 1.0
	for _, name := range names {
		v, ok := other[name]
		if !ok {
			return fmt.Errorf("%s missing from %s", name, args[1])
		}
		if len(v) != len(ref[name]) { // vectors from different settings or a damaged file
			return fmt.Errorf("%s: %d values in %s but %d in %s", name, len(ref[name]), args[0], len(v), args[1])
		}
		c := cosine(ref[name], v)
		worst = min(worst, c)
		fmt.Printf("  %-12s %.6f\n", name, c)
	}

	const threshold = 0.999
	if worst < threshold {
		return fmt.Errorf("worst cosine %.6f is below %.3f: backends do not match", worst, threshold)
	}
	fmt.Printf("  [PASS] worst cosine %.6f >= %.3f\n", worst, threshold)
	return nil
}

func loadVectors(path string) (map[string][]float32, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Vectors map[string][]float32 `json:"vectors"`
	}
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f.Vectors, nil
}
