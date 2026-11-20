// Package store ingests symbol tables and raw samples and serves
// order-independent, filterable queries.
//
// Determinism: samples are kept in ingestion slices but Query re-orders
// nothing itself — callers feed the slice straight into merge.Merge, whose
// output depends only on the sample multiset. Tests shuffle ingestion order
// and assert identical results.
package store

import (
	"errors"
	"sort"
	"sync"

	"aquaprofile/internal/merge"
	"aquaprofile/internal/symbolize"
)

// Filter selects samples by time range and label subset. All requested
// labels must match; empty filters match everything.
type Filter struct {
	StartNs int64 // inclusive; 0 means unbounded
	EndNs   int64 // exclusive; 0 means unbounded
	Labels  map[string]string
}

// Sample is an unresolved, raw stack sample as reported by a process.
type Sample struct {
	TimeNs    uint64            `json:"time_ns"`
	Addresses []uint64          `json:"addresses"` // root call first
	Labels    map[string]string `json:"labels"`
}

// Batch is one upload from one process of one program/version.
type Batch struct {
	Program string `json:"program"`
	Version string `json:"version"`
	Pid     int64  `json:"pid"`
	// PeriodNs is the time one sample represents for this process. The
	// service trusts the reporter: merging weights by period keeps
	// different sampling frequencies comparable.
	PeriodNs int64    `json:"period_ns"`
	Samples  []Sample `json:"samples"`
}

// ModuleUpload reports the load layout of one process.
type ModuleUpload struct {
	Program string             `json:"program"`
	Version string             `json:"version"`
	Pid     int64              `json:"pid"`
	Modules []symbolize.Module `json:"modules"`
}

type procKey struct {
	program string
	version string
	pid     int64
}

// ProcInfo is one reporting process shown on the status tab.
type ProcInfo struct {
	Program  string
	Version  string
	Pid      int64
	PeriodNs int64
	Samples  int64
	FirstNs  uint64
	LastNs   uint64
	Labels   map[string]struct{}
	Modules  int
}

type procState struct {
	period   int64
	resolver *symbolize.Resolver
	samples  []storedSample
	first    uint64
	last     uint64
	labelSet map[string]struct{}
	mods     int
}

type storedSample struct {
	timeNs    uint64
	addresses []uint64
	labels    map[string]string
	period    int64
}

// Store is the in-memory ingestion database.
type Store struct {
	mu    sync.RWMutex
	procs map[procKey]*procState
}

// New creates an empty store.
func New() *Store {
	return &Store{procs: make(map[procKey]*procState)}
}

// PutModules installs a (possibly updated) module table for a process.
func (s *Store) PutModules(u ModuleUpload) error {
	if u.Program == "" || u.Version == "" {
		return errors.New("program and version are required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := procKey{u.Program, u.Version, u.Pid}
	st, ok := s.procs[k]
	if !ok {
		st = &procState{labelSet: make(map[string]struct{})}
		s.procs[k] = st
	}
	st.resolver = symbolize.NewResolver(u.Modules)
	st.mods = len(u.Modules)
	return nil
}

// PutBatch appends samples from one process.
func (s *Store) PutBatch(b Batch) error {
	if b.Program == "" || b.Version == "" {
		return errors.New("program and version are required")
	}
	if b.PeriodNs <= 0 {
		return errors.New("period_ns must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := procKey{b.Program, b.Version, b.Pid}
	st, ok := s.procs[k]
	if !ok {
		st = &procState{labelSet: make(map[string]struct{})}
		s.procs[k] = st
	}
	st.period = b.PeriodNs
	for _, sm := range b.Samples {
		if len(sm.Addresses) == 0 {
			continue
		}
		labels := make(map[string]string, len(sm.Labels))
		for k, v := range sm.Labels {
			labels[k] = v
			st.labelSet[k+"="+v] = struct{}{}
		}
		st.samples = append(st.samples, storedSample{
			timeNs: sm.TimeNs, addresses: append([]uint64(nil), sm.Addresses...),
			labels: labels, period: b.PeriodNs,
		})
		if st.first == 0 || sm.TimeNs < st.first {
			st.first = sm.TimeNs
		}
		if sm.TimeNs > st.last {
			st.last = sm.TimeNs
		}
	}
	return nil
}

// Query resolves and filters all samples for one program+version. Samples
// whose process never uploaded a symbol table resolve entirely to
// [unknown] frames, which is still meaningful for gap accounting.
func (s *Store) Query(program, version string, f Filter) []merge.Sample {
	s.mu.RLock()
	defer s.mu.RUnlock()

	keys := make([]procKey, 0)
	for k := range s.procs {
		if k.program == program && k.version == version {
			keys = append(keys, k)
		}
	}
	// Stable process order; merge itself is order-independent, but a
	// fixed iteration order keeps every layer deterministic.
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].pid != keys[j].pid {
			return keys[i].pid < keys[j].pid
		}
		return keys[i].program < keys[j].program
	})

	var out []merge.Sample
	for _, k := range keys {
		st := s.procs[k]
		var res *symbolize.Resolver
		if st.resolver != nil {
			res = st.resolver
		} else {
			res = symbolize.NewResolver(nil)
		}
		for _, sm := range st.samples {
			if !f.matches(sm.timeNs, sm.labels) {
				continue
			}
			frames := make([]symbolize.Frame, len(sm.addresses))
			for i, addr := range sm.addresses {
				frames[i] = res.Resolve(addr)
			}
			out = append(out, merge.Sample{Weight: sm.period, Frames: frames})
		}
	}
	return out
}

func (f Filter) matches(t uint64, labels map[string]string) bool {
	if f.StartNs != 0 && int64(t) < f.StartNs {
		return false
	}
	if f.EndNs != 0 && int64(t) >= f.EndNs {
		return false
	}
	for k, v := range f.Labels {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// Status returns a sorted snapshot of every reporting process.
func (s *Store) Status() []ProcInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProcInfo, 0, len(s.procs))
	for k, st := range s.procs {
		labels := make(map[string]struct{}, len(st.labelSet))
		for l := range st.labelSet {
			labels[l] = struct{}{}
		}
		out = append(out, ProcInfo{
			Program: k.program, Version: k.version, Pid: k.pid,
			PeriodNs: st.period, Samples: int64(len(st.samples)),
			FirstNs: st.first, LastNs: st.last,
			Labels: labels, Modules: st.mods,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Program != out[j].Program {
			return out[i].Program < out[j].Program
		}
		if out[i].Version != out[j].Version {
			return out[i].Version < out[j].Version
		}
		return out[i].Pid < out[j].Pid
	})
	return out
}

// Programs lists known program names sorted.
func (s *Store) Programs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	for k := range s.procs {
		seen[k.program] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Versions lists versions of one program sorted.
func (s *Store) Versions(program string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	seen := make(map[string]struct{})
	for k := range s.procs {
		if k.program == program {
			seen[k.version] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for v := range seen {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
