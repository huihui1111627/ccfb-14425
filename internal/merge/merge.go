// Package merge aggregates resolved stack samples into a call tree.
//
// Stacks are merged by prefix: each sample weight walks its frame sequence
// from the root and every node on the path accumulates the weight once. A
// frame that recurs within one stack still maps to the same node, so its
// cumulative ("total") time is never counted twice. Per-function totals are
// likewise computed over distinct frames per sample.
//
// All aggregation is over slices, not map iteration, and every output slice
// is sorted: merging the same multiset of samples always yields the same
// result regardless of ingestion order.
package merge

import (
	"sort"

	"aquaprofile/internal/symbolize"
)

// Sample is one resolved stack sample ready for merging.
type Sample struct {
	// Weight is the wall-time this sample represents (typically the
	// reporting process sampling period in nanoseconds). Samples from
	// processes with different frequencies carry different weights and
	// must never be combined by raw sample counts.
	Weight int64
	// Frames are resolved frames, outermost (root) call first.
	Frames []symbolize.Frame
}

// Node is one frame in the merged call tree.
type Node struct {
	Frame     symbolize.Frame
	Self      int64 // weight of samples that ended on this node
	Total     int64 // cumulative weight through this node (recursion counted once)
	Count     int64 // number of samples passing through this node
	SelfCount int64 // number of samples ending on this node
	Children  []*Node
}

// FunctionStat aggregates one function across all call sites.
type FunctionStat struct {
	Frame symbolize.Frame
	Self  int64
	// Total sums each sample's weight once if the function appears
	// anywhere in the stack, even when it recurs on the stack.
	Total int64
	// Count is the number of distinct samples containing the function.
	Count int64
	// SelfCount is the number of samples ending in the function.
	SelfCount int64
}

// HoleStat reports samples whose addresses fall outside the symbol table.
type HoleStat struct {
	Frame   symbolize.Frame
	Kind    string // "gap" (inside a module, outside symbols) or "unknown" (no module)
	Samples int64  // distinct samples containing this frame
	Weight  int64
}

// Result is the deterministic merge output.
type Result struct {
	SampleCount int64
	TotalWeight int64
	// Root is a synthetic node (empty frame) whose Total equals TotalWeight
	// and whose Children are the outermost stack frames.
	Root      *Node
	Functions []FunctionStat
	Gaps      []HoleStat
	Unknowns  []HoleStat
}

type rawNode struct {
	frame            symbolize.Frame
	self, total      int64
	count, selfCount int64
	children         map[symbolize.Frame]*rawNode
}

func newRawNode(f symbolize.Frame) *rawNode {
	return &rawNode{frame: f, children: make(map[symbolize.Frame]*rawNode)}
}

// Merge aggregates samples. When fold is true, every node narrower than
// 1/1000 of the root width is absorbed into a synthetic [folded] node
// (applied top-down); when false the full tree is retained, which the diff
// engine needs.
func Merge(samples []Sample, fold bool) *Result {
	root := newRawNode(symbolize.Frame{})

	type fnAgg struct {
		frame            symbolize.Frame
		self, total      int64
		count, selfCount int64
	}
	fnMap := make(map[symbolize.Frame]*fnAgg)
	gapMap := make(map[symbolize.Frame]int64)
	unkMap := make(map[symbolize.Frame]int64)
	gapCount := make(map[symbolize.Frame]int64)
	unkCount := make(map[symbolize.Frame]int64)

	var totalWeight, sampleCount int64

	for _, s := range samples {
		if s.Weight <= 0 || len(s.Frames) == 0 {
			continue
		}
		w := s.Weight
		totalWeight += w
		sampleCount++

		// Prefix walk: create/reuse tree nodes, charge total once per node.
		cur := root
		cur.total += w
		cur.count++
		seenFn := make(map[symbolize.Frame]bool, len(s.Frames))
		seenGap := make(map[symbolize.Frame]bool)
		seenUnk := make(map[symbolize.Frame]bool)
		for _, fr := range s.Frames {
			next, ok := cur.children[fr]
			if !ok {
				next = newRawNode(fr)
				cur.children[fr] = next
			}
			cur = next
			cur.total += w
			cur.count++

			// Distinct-frame function aggregation (recursion-safe).
			if !seenFn[fr] {
				seenFn[fr] = true
				a, ok := fnMap[fr]
				if !ok {
					a = &fnAgg{frame: fr}
					fnMap[fr] = a
				}
				a.total += w
				a.count++
			}
			switch {
			case fr.IsGap():
				seenGap[fr] = true
			case fr.IsUnknown():
				seenUnk[fr] = true
			}
		}

		// Self time lands on the leaf frame exactly once per sample even
		// if the leaf frame repeats on the stack (tail/self recursion).
		leaf := s.Frames[len(s.Frames)-1]
		cur.self += w
		cur.selfCount++
		a := fnMap[leaf] // exists: leaf was visited above
		a.self += w
		a.selfCount++

		for fr := range seenGap {
			gapMap[fr] += w
			gapCount[fr]++
		}
		for fr := range seenUnk {
			unkMap[fr] += w
			unkCount[fr]++
		}
	}

	res := &Result{
		SampleCount: sampleCount,
		TotalWeight: totalWeight,
		Root:        buildNode(root),
	}
	if fold && totalWeight > 0 {
		foldTree(res.Root)
	}

	// Deterministic function table.
	for _, a := range fnMap {
		res.Functions = append(res.Functions, FunctionStat{
			Frame: a.frame, Self: a.self, Total: a.total,
			Count: a.count, SelfCount: a.selfCount,
		})
	}
	sort.Slice(res.Functions, func(i, j int) bool {
		if res.Functions[i].Frame.Func != res.Functions[j].Frame.Func {
			return res.Functions[i].Frame.Func < res.Functions[j].Frame.Func
		}
		return res.Functions[i].Frame.Module < res.Functions[j].Frame.Module
	})

	res.Gaps = collectHoles(gapMap, gapCount, "gap")
	res.Unknowns = collectHoles(unkMap, unkCount, "unknown")
	return res
}

func buildNode(rn *rawNode) *Node {
	n := &Node{
		Frame: rn.frame, Self: rn.self, Total: rn.total,
		Count: rn.count, SelfCount: rn.selfCount,
		Children: make([]*Node, 0, len(rn.children)),
	}
	for _, child := range rn.children {
		n.Children = append(n.Children, buildNode(child))
	}
	// Siblings sorted by function name (then module), never by cost, so
	// two flame graphs of the same code line up shape-for-shape.
	sort.Slice(n.Children, func(i, j int) bool {
		if n.Children[i].Frame.Func != n.Children[j].Frame.Func {
			return n.Children[i].Frame.Func < n.Children[j].Frame.Func
		}
		return n.Children[i].Frame.Module < n.Children[j].Frame.Module
	})
	return n
}

// foldTree collapses, at every level, children whose width is strictly less
// than 1/1000 of their PARENT's width into one synthetic [folded] leaf.
// The parent-relative rule keeps deep subtrees readable: a node that is
// tiny against the whole profile but substantial within its own caller is
// retained. Folding is top-down: folded subtrees are not traversed further.
func foldTree(root *Node) {
	var walk func(n *Node)
	walk = func(n *Node) {
		var kept, small []*Node
		for _, c := range n.Children {
			if n.Total > 0 && c.Total*1000 < n.Total {
				small = append(small, c)
			} else {
				kept = append(kept, c)
			}
		}
		if len(small) > 0 {
			f := &Node{Frame: symbolize.FoldedFrame()}
			for _, c := range small {
				// The whole small subtree collapses onto the folded leaf,
				// so all of its cumulative weight becomes folded self time.
				f.Total += c.Total
				f.Self += c.Total
				f.Count += c.Count
				f.SelfCount += c.Count
			}
			kept = append(kept, f)
		}
		sort.Slice(kept, func(i, j int) bool {
			if kept[i].Frame.Func != kept[j].Frame.Func {
				return kept[i].Frame.Func < kept[j].Frame.Func
			}
			return kept[i].Frame.Module < kept[j].Frame.Module
		})
		n.Children = kept
		for _, c := range n.Children {
			if !c.Frame.IsFolded() {
				walk(c)
			}
		}
	}
	walk(root)
}

func collectHoles(weight, count map[symbolize.Frame]int64, kind string) []HoleStat {
	out := make([]HoleStat, 0, len(weight))
	for fr, w := range weight {
		out = append(out, HoleStat{Frame: fr, Kind: kind, Samples: count[fr], Weight: w})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Weight != out[j].Weight {
			return out[i].Weight > out[j].Weight
		}
		if out[i].Frame.Module != out[j].Frame.Module {
			return out[i].Frame.Module < out[j].Frame.Module
		}
		return out[i].Frame.Func < out[j].Frame.Func
	})
	return out
}
