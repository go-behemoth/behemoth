package utils

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func alphabetical(a, b string) bool { return a < b }

// A graph maps each node to the nodes that depend on it.
func TestKahnSortOrdersDependenciesFirst(t *testing.T) {
	graph := map[string][]string{
		"core":  {"plugin", "store"},
		"store": {"plugin"},
		"util":  {},
	}

	order, cycle, ok := KahnSort(graph, alphabetical)
	assert.True(t, ok)
	assert.Nil(t, cycle)
	assert.Equal(t, []string{"core", "store", "plugin", "util"}, order)
}

// less breaks the ties among nodes that are ready at the same step, and a
// node that becomes ready joins them in order.
func TestKahnSortBreaksTiesWithLess(t *testing.T) {
	graph := map[string][]string{"a": {"d"}, "b": {}, "c": {}}
	reverse := func(a, b string) bool { return a > b }

	order, _, ok := KahnSort(graph, reverse)
	assert.True(t, ok)
	assert.Equal(t, []string{"c", "b", "a", "d"}, order)
}

// The report lists the nodes of one cycle in dependency order, from and back
// to its first node.
func TestKahnSortReportsTheCycle(t *testing.T) {
	for name, tc := range map[string]struct {
		graph map[string][]string
		want  []string
	}{
		"two nodes":   {map[string][]string{"a": {"b"}, "b": {"a"}}, []string{"a", "b", "a"}},
		"three nodes": {map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"a"}}, []string{"a", "c", "b", "a"}},
		"self":        {map[string][]string{"a": {"a"}}, []string{"a", "a"}},
		// d depends on a, which resolves, and on m, which is in a cycle with
		// n. The walk starts at d and follows only the dependencies that
		// never resolved, so it reaches the cycle and not a dead end at a.
		"downstream of a cycle": {
			map[string][]string{"a": {"d"}, "m": {"d", "n"}, "n": {"m"}},
			[]string{"m", "n", "m"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			// The report must not depend on the order of map iteration.
			for range 20 {
				order, cycle, ok := KahnSort(tc.graph, alphabetical)
				assert.False(t, ok)
				assert.Nil(t, order)
				assert.Equal(t, tc.want, cycle)
			}
		})
	}
}
