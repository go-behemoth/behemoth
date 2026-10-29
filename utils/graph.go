package utils

import "sort"

// KahnSort performs topological sorting on a given Directed Acyclic Graph.
// The graph map should have nodes as keys and their dependants as lists.
// For eg. If A and C depend on B,
// B: {A, C}
// Ties among nodes with in-degree zero at the same step are broken by name, ascending
func KahnSort(graph map[string][]string, less func(a, b string) bool) (order []string, cyclePath []string, ok bool) {

	// Collect every node
	allNodes := make(map[string]bool)
	for node, dependants := range graph {
		allNodes[node] = true
		for _, d := range dependants {
			allNodes[d] = true
		}
	}

	// Use len(allNodes) instead of N since some nodes might only be listed as dependants
	N := len(allNodes)
	indegree := make(map[string]int, N)
	for n := range allNodes {
		indegree[n] = 0
	}
	for _, dependants := range graph {
		for _, dep := range dependants {
			indegree[dep]++
		}
	}

	queue := make([]string, 0)
	for node := range graph {
		if indegree[node] == 0 {
			queue = append(queue, node)
		}
	}
	sort.Slice(queue, func(i, j int) bool { return less(queue[i], queue[j]) })

	order = make([]string, 0, N)

	front := 0
	for len(queue) > 0 {
		// Pop the alphabetically-first ready node
		curr := queue[front]
		queue = queue[1:]
		order = append(order, curr)

		// new eligible nodes
		var newEntries []string
		for _, dep := range graph[curr] {
			indegree[dep]--
			if indegree[dep] == 0 {
				newEntries = append(newEntries, dep)
			}
		}

		sort.Strings(newEntries)
		sort.Slice(newEntries, func(i, j int) bool { return less(newEntries[i], newEntries[j]) })
		queue = mergeSorted(queue, newEntries, less) // keep 'queue' sorted as new nodes join
	}

	if len(order) != N {
		// return remaining indegree nodes as cycles
		return nil, findCyclePath(graph, indegree), false
	}

	return order, nil, true
}

func mergeSorted(a, b []string, less func(a, b string) bool) []string {
	if len(b) == 0 {
		return a
	}
	out := make([]string, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if !less(b[j], a[i]) { // a[i] <= b[j]
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	return append(append(out, a[i:]...), b[j:]...)
}

// findCyclePath returns one cycle among the nodes KahnSort could not order,
// those whose in-degree stayed above zero, as a path from a node through its
// dependencies back to itself. Each such node waits for a dependency that is
// unordered too, so a walk that follows only those dependencies never ends,
// and the first node it reaches twice closes a cycle. Following a dependency
// that was ordered would end the walk at a node that is in no cycle. The walk
// starts at the first unordered node by name and takes the first dependency
// by name, so a graph always gets the same report.
func findCyclePath(graph map[string][]string, remainingInDegree map[string]int) []string {
	// The graph lists the dependants of each node, and the walk goes the
	// other way.
	dependsOn := make(map[string][]string)
	for node, dependants := range graph {
		if remainingInDegree[node] == 0 {
			continue
		}
		for _, d := range dependants {
			dependsOn[d] = append(dependsOn[d], node)
		}
	}

	var unordered []string
	for n, deg := range remainingInDegree {
		if deg > 0 {
			unordered = append(unordered, n)
		}
	}
	if len(unordered) == 0 {
		return nil // KahnSort calls it only when a node was left unordered
	}
	sort.Strings(unordered)

	start := unordered[0]
	at := map[string]int{start: 0} // each node of the path, and its index
	path := []string{start}
	for current := start; ; {
		next := dependsOn[current]
		if len(next) == 0 {
			return path // an unordered node always has an unordered dependency
		}
		sort.Strings(next)
		current = next[0]
		if i, seen := at[current]; seen {
			return append(path[i:], current)
		}
		at[current] = len(path)
		path = append(path, current)
	}
}
