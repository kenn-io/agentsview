package stringutil

import "unique"

// Interner caches global string handles within one read.
type Interner map[string]unique.Handle[string]

func (interned Interner) Intern(v string) unique.Handle[string] {
	h, ok := interned[v]
	if !ok {
		h = unique.Make(v)
		interned[v] = h
	}
	return h
}
