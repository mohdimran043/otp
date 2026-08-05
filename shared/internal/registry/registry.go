// Package registry is the plug-in table behind the platform's three pluggable
// surfaces: optical encodings, compressors, and error-correcting codes.
//
// All three need the same thing — look up a codec by the wire id a frame carries,
// look it up by the name a configuration file uses, and enumerate them for the UI
// and the generated documentation — and all three are safety-critical in the same
// way: a duplicate id means frames get decoded by the wrong codec, which surfaces
// as data corruption rather than as an error. Writing that once means the three
// surfaces cannot drift into behaving differently.
package registry

import (
	"fmt"
	"sort"
	"sync"
)

// Item is what a registry holds: anything with a wire id and a configuration name.
type Item interface {
	ID() uint8
	Name() string
}

// Registry maps ids and names to codecs.
type Registry[T Item] struct {
	kind    string
	unknown error

	mu     sync.RWMutex
	byID   map[uint8]T
	byName map[string]T
}

// New returns an empty registry. kind names the thing being registered, for error
// messages; unknown is the sentinel returned for a lookup that misses, so callers
// can match it with errors.Is.
func New[T Item](kind string, unknown error) *Registry[T] {
	return &Registry[T]{
		kind:    kind,
		unknown: unknown,
		byID:    map[uint8]T{},
		byName:  map[string]T{},
	}
}

// Register adds a codec.
//
// It panics on a duplicate id or name. That is deliberate: a registry is built
// from package initialisers, so a collision is a programming error present in
// every run rather than a condition to handle — and the alternative is shipping a
// build where some frames are silently decoded by the wrong codec.
func (r *Registry[T]) Register(v T) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prev, dup := r.byID[v.ID()]; dup {
		panic(fmt.Sprintf("registry: %s id %d already registered to %q", r.kind, v.ID(), prev.Name()))
	}
	if prev, dup := r.byName[v.Name()]; dup {
		panic(fmt.Sprintf("registry: %s name %q already registered to id %d", r.kind, v.Name(), prev.ID()))
	}
	r.byID[v.ID()] = v
	r.byName[v.Name()] = v
}

// ByID returns the codec with that wire identifier.
func (r *Registry[T]) ByID(id uint8) (T, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.byID[id]
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w: id %d", r.unknown, id)
	}
	return v, nil
}

// ByName returns the codec with that configuration name.
func (r *Registry[T]) ByName(name string) (T, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.byName[name]
	if !ok {
		var zero T
		return zero, fmt.Errorf("%w: %q", r.unknown, name)
	}
	return v, nil
}

// All returns every registered codec, ordered by id. The order is by id rather
// than by name so that listings — the profile UI, the generated documentation —
// present codecs in the order they were added to the protocol, which is also
// roughly increasing sophistication.
func (r *Registry[T]) All() []T {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]T, 0, len(r.byID))
	for _, v := range r.byID {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Names returns every registered name, ordered by id.
func (r *Registry[T]) Names() []string {
	all := r.All()
	out := make([]string, len(all))
	for i, v := range all {
		out[i] = v.Name()
	}
	return out
}
