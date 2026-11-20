// Package symbolize resolves raw code addresses to module+function frames.
//
// The same module is loaded at a different base address in every process, so
// resolution is always performed against the module-internal offset
// (addr - module base). Addresses that fall inside a module span but outside
// every known symbol are classified as a per-module "[gap]" frame; addresses
// that do not belong to any module become "[unknown]" frames.
package symbolize

import (
	"fmt"
	"sort"
)

const (
	// GapFunc marks an address that lies inside a module but in a symbol
	// table hole. The frame module is the containing module.
	GapFunc = "[gap]"
	// UnknownModule marks addresses not covered by any loaded module.
	UnknownModule = "[unknown]"
	// FoldedModule/FoldedFunc name the synthetic node that absorbs
	// sub-threshold frames during flame graph folding.
	FoldedModule = "[folded]"
	FoldedFunc   = "[folded]"
)

// Frame is a resolved stack frame: a function inside a module.
type Frame struct {
	Module string
	Func   string
}

// GapFrame returns the gap frame for the given module.
func GapFrame(module string) Frame { return Frame{Module: module, Func: GapFunc} }

// UnknownFrame returns the frame used when no module contains addr.
func UnknownFrame(addr uint64) Frame {
	return Frame{Module: UnknownModule, Func: fmt.Sprintf("0x%x", addr)}
}

// FoldedFrame is the synthetic node collecting folded-away children.
func FoldedFrame() Frame { return Frame{Module: FoldedModule, Func: FoldedFunc} }

func (f Frame) IsGap() bool     { return f.Func == GapFunc }
func (f Frame) IsUnknown() bool { return f.Module == UnknownModule }
func (f Frame) IsFolded() bool  { return f.Module == FoldedModule }

// String renders the frame as "module!function".
func (f Frame) String() string {
	if f.Module == "" {
		return f.Func
	}
	return f.Module + "!" + f.Func
}

// Symbol is one function entry in a module symbol table. Addr is the
// absolute process address at which the function starts in this process;
// the resolver converts it to a module offset internally.
type Symbol struct {
	Name string `json:"name"`
	Addr uint64 `json:"addr"`
	Size uint64 `json:"size"` // 0 means the symbol only matches Addr itself
}

// Module is one loaded module inside one process.
type Module struct {
	Name    string   `json:"name"`
	Base    uint64   `json:"base"`
	Size    uint64   `json:"size"` // module span: [Base, Base+Size)
	Symbols []Symbol `json:"symbols"`
}

type symEntry struct {
	off  uint64 // offset relative to module base
	size uint64
	name string
}

type modIndex struct {
	mod  Module
	syms []symEntry // sorted by offset
}

// Resolver holds the processed module tables of one process and resolves
// addresses deterministically.
type Resolver struct {
	// Sorted by Base descending so that when modules overlap the one with
	// the highest base wins; ties break on module name ascending so the
	// result never depends on map/slice iteration order.
	mods []modIndex
}

// NewResolver builds a resolver from the raw module tables of one process.
func NewResolver(mods []Module) *Resolver {
	r := &Resolver{mods: make([]modIndex, 0, len(mods))}
	for _, m := range mods {
		if m.Size == 0 {
			continue
		}
		idx := modIndex{mod: m, syms: make([]symEntry, 0, len(m.Symbols))}
		for _, s := range m.Symbols {
			if s.Addr < m.Base || s.Addr >= m.Base+m.Size {
				continue // symbol outside its own module span: ignore
			}
			idx.syms = append(idx.syms, symEntry{
				off: s.Addr - m.Base, size: s.Size, name: s.Name,
			})
		}
		sort.Slice(idx.syms, func(i, j int) bool {
			if idx.syms[i].off != idx.syms[j].off {
				return idx.syms[i].off < idx.syms[j].off
			}
			return idx.syms[i].name < idx.syms[j].name
		})
		r.mods = append(r.mods, idx)
	}
	sort.Slice(r.mods, func(i, j int) bool {
		if r.mods[i].mod.Base != r.mods[j].mod.Base {
			return r.mods[i].mod.Base > r.mods[j].mod.Base
		}
		return r.mods[i].mod.Name < r.mods[j].mod.Name
	})
	return r
}

// Resolve maps one absolute address to a Frame.
func (r *Resolver) Resolve(addr uint64) Frame {
	for i := range r.mods {
		m := &r.mods[i]
		if addr < m.mod.Base || addr >= m.mod.Base+m.mod.Size {
			continue
		}
		off := addr - m.mod.Base
		// Last symbol starting at or before off.
		k := sort.Search(len(m.syms), func(n int) bool { return m.syms[n].off > off }) - 1
		if k >= 0 {
			s := &m.syms[k]
			if s.size == 0 {
				if off == s.off {
					return Frame{Module: m.mod.Name, Func: s.name}
				}
			} else if off < s.off+s.size {
				return Frame{Module: m.mod.Name, Func: s.name}
			}
		}
		// Inside the module span but outside every symbol: a table hole.
		return GapFrame(m.mod.Name)
	}
	return UnknownFrame(addr)
}
