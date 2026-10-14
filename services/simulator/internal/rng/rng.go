// Package rng gives every simulated agent its own deterministic random source.
//
// One shared random source would make a run depend on goroutine scheduling: whichever agent
// asked first would get the next number. Instead each agent (courier 17, the Shibuya customers,
// store 3 in Minato, ...) gets a source derived from the scenario seed and its own name, so the
// same seed always produces the same choices for that agent, and adding a courier does not
// change what the others do.
package rng

import (
	"hash/fnv"
	"math/rand/v2"
)

// New returns the random source for one agent. The same seed and name always give the same
// sequence; different names give independent sequences.
func New(seed uint64, name string) *rand.Rand {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name)) // writing to a hash never fails
	// PCG takes two 64-bit seed words: the scenario seed and a hash of the agent's name.
	return rand.New(rand.NewPCG(seed, h.Sum64())) //nolint:gosec // simulation randomness, not security
}
