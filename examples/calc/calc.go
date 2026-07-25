// Package calc is fixture data for examples/repo.yaml. The bug is deliberate.
//
// Nothing here is ever modified by a run: `--in` CAPTURES this directory into the
// content-addressed tree store, and each step edits a scratch copy.
package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b // BUG: subtracts instead of adding
}
