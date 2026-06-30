package livetrack

// ChildrenOf returns the IDs of every session whose ParentID equals
// the supplied parentID. The slice is a fresh copy so callers can
// hand it off without risking concurrent mutation. Returns nil when
// no children are tracked for the parent.
//
// Parent close does NOT cascade through this package. Adopters that
// want cascade semantics call ForceCloseMatching with a predicate
// that walks the tree below the parent. This keeps the registry
// neutral about cascade policy and avoids surprising side effects
// for the common case where parent and child have independent
// lifetimes (an HTTP server and one connection it accepted).
func (r *Registry[M]) ChildrenOf(parentID string) []string {
	if parentID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	children := r.parents[parentID]
	if len(children) == 0 {
		return nil
	}
	out := make([]string, len(children))
	copy(out, children)
	return out
}

// DescendantsOf walks the parent map breadth-first and returns every
// transitive descendant of parentID. Cycles are impossible because
// ParentID is set once at Register time, so the walk terminates
// after at most len(r.sessions) iterations.
func (r *Registry[M]) DescendantsOf(parentID string) []string {
	if parentID == "" {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0)
	queue := []string{parentID}
	visited := make(map[string]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for _, child := range r.parents[current] {
			if visited[child] {
				continue
			}
			visited[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}
