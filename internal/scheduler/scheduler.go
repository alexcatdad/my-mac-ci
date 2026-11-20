// Package scheduler accounts for job demand and the resources held by MMC containers.
package scheduler

import (
	"cmp"
	"errors"
	"maps"
	"slices"
	"sync"
)

type Key struct {
	Repo, Label string
}

type Resources struct {
	CPUs   int
	Memory int64 // bytes
}

type Label struct {
	Resources Resources
	Max       int // zero means the global cap alone limits starts
}

type reservation struct {
	key       Key
	resources Resources
	completed bool
}

type Scheduler struct {
	mu       sync.Mutex
	cap      Resources
	labels   map[Key]Label
	order    []Key
	last     Key
	assigned map[Key]int
	runners  map[string]reservation
}

func compare(a, b Key) int {
	return cmp.Or(cmp.Compare(a.Repo, b.Repo), cmp.Compare(a.Label, b.Label))
}

func New(cap Resources, labels map[Key]Label) *Scheduler {
	s := &Scheduler{assigned: make(map[Key]int), runners: make(map[string]reservation)}
	s.Reload(cap, labels)
	return s
}

// Reload applies a validated configuration. Existing reservations keep their
// original resource allocation until their containers exit.
func (s *Scheduler) Reload(cap Resources, labels map[Key]Label) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.cap = cap
	s.labels = maps.Clone(labels)
	s.order = slices.SortedFunc(maps.Keys(labels), compare)
	for key := range s.assigned {
		if _, exists := labels[key]; !exists {
			delete(s.assigned, key)
		}
	}
}

// Update applies completions and a demand snapshot together. A message with
// no statistics preserves the most recent demand.
func (s *Scheduler) Update(key Key, assigned *int, completed []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.labels[key]; exists && assigned != nil {
		s.assigned[key] = *assigned
	}
	for _, name := range completed {
		if r, ok := s.runners[name]; ok && r.key == key {
			r.completed = true
			s.runners[name] = r
		}
	}
}

// Reserve chooses the next eligible label and reserves resources before the
// caller launches its runner. Release the reservation after a failed start or
// a confirmed exit. eligible is optional and runs under the scheduler lock.
func (s *Scheduler) Reserve(name string, eligible func(Key) bool) (Key, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.runners[name]; name == "" || exists {
		return Key{}, false
	}
	free := s.cap
	active := make(map[Key]int)
	for _, r := range s.runners {
		if r.resources.CPUs > free.CPUs || r.resources.Memory > free.Memory {
			return Key{}, false
		}
		free.CPUs -= r.resources.CPUs
		free.Memory -= r.resources.Memory
		if !r.completed {
			active[r.key]++
		}
	}

	next, found := slices.BinarySearchFunc(s.order, s.last, compare)
	if found {
		next++
	}
	for i := range s.order {
		key := s.order[(next+i)%len(s.order)]
		label := s.labels[key]
		if active[key] >= s.assigned[key] || (label.Max > 0 && active[key] >= label.Max) {
			continue
		}
		r := label.Resources
		if r.CPUs <= 0 || r.Memory <= 0 || r.CPUs > free.CPUs || r.Memory > free.Memory {
			continue
		}
		if eligible != nil && !eligible(key) {
			continue
		}
		s.runners[name] = reservation{key: key, resources: r}
		s.last = key
		return key, true
	}
	return Key{}, false
}

// Adopt accounts for a surviving MMC container, even when its label has been
// removed or its resources exceed the new cap. Repeat observations are safe.
func (s *Scheduler) Adopt(name string, key Key, resources Resources) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if name == "" || resources.CPUs <= 0 || resources.Memory <= 0 {
		return errors.New("invalid surviving runner allocation")
	}
	if previous, exists := s.runners[name]; exists {
		if previous.key != key || previous.resources != resources {
			return errors.New("conflicting surviving runner allocation")
		}
		return nil
	}
	s.runners[name] = reservation{key: key, resources: resources}
	return nil
}

func (s *Scheduler) Release(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runners, name)
}
