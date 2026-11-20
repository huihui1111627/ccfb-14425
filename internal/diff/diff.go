// Package diff compares two merged profiles (typically two program versions).
//
// Normalization rule (requirement 5): before comparison each version is
// normalized by its own total sample count, so a function that keeps the same
// share of work shows zero movement even when sample totals differ. We use
// integer rational arithmetic, cross-multiplying against the opposite
// version's total, so comparisons are exact and order-independent; floating
// point shares are only materialized for display.
package diff

import (
	"sort"

	"aquaprofile/internal/merge"
	"aquaprofile/internal/symbolize"
)

// Direction labels a function-level change.
type Direction string

const (
	Slower Direction = "slower" // normalized share grew in the new version
	Faster Direction = "faster" // normalized share shrank
	Same   Direction = "same"
)

// Row is one function in the version comparison.
type Row struct {
	Frame symbolize.Frame
	// Raw per-version statistics.
	OldCount, NewCount           int64
	OldTotalCount, NewTotalCount int64
	// OldNorm/NewNorm are the normalized shares, equal to
	// count*1e9/total with integer rounding.
	OldNorm, NewNorm int64
	// DeltaNorm is NewNorm - OldNorm in the same 1e9-scaled units.
	DeltaNorm int64
	Direction Direction
}

// TreeNode is one frame in the merged two-version call tree.
type TreeNode struct {
	Frame                        symbolize.Frame
	OldCount, NewCount           int64
	OldTotalCount, NewTotalCount int64
	DeltaNorm                    int64
	Children                     []*TreeNode
}

// Result holds both the ranked function table and the aligned tree.
type Result struct {
	OldTotal int64
	NewTotal int64
	Rows     []Row
	Tree     *TreeNode
}

const normScale = 1_000_000_000

func scaled(count, total int64) int64 {
	if total == 0 {
		return 0
	}
	return count * normScale / total
}

// Build compares two unfolded merged results. Tree nodes narrower than
// 1/1000 of the root in *both* versions are folded together.
func Build(oldRes, newRes *merge.Result) *Result {
	r := &Result{
		OldTotal: oldRes.SampleCount,
		NewTotal: newRes.SampleCount,
	}

	// Function-level table: join the two deterministic function slices by
	// frame (both are sorted by frame name).
	r.Rows = joinFunctions(oldRes, newRes)

	// Tree-level union with per-side counts and integer normalized delta.
	oldTree := pruneFold(oldRes.Root, oldRes.SampleCount)
	newTree := pruneFold(newRes.Root, newRes.SampleCount)
	r.Tree = unionNodes(oldTree, newTree, oldRes.SampleCount, newRes.SampleCount)
	return r
}

func joinFunctions(a, b *merge.Result) []Row {
	idx := make(map[symbolize.Frame]merge.FunctionStat, len(b.Functions))
	for _, f := range b.Functions {
		idx[f.Frame] = f
	}
	seen := make(map[symbolize.Frame]bool, len(b.Functions))
	rows := make([]Row, 0, len(a.Functions)+len(b.Functions))
	for _, of := range a.Functions {
		seen[of.Frame] = true
		nf := idx[of.Frame]
		rows = append(rows, makeRow(of.Frame, of.Count, nf.Count, a.SampleCount, b.SampleCount))
	}
	for _, nf := range b.Functions {
		if seen[nf.Frame] {
			continue
		}
		rows = append(rows, makeRow(nf.Frame, 0, nf.Count, a.SampleCount, b.SampleCount))
	}
	// Ranked by absolute normalized movement so the slowest/fastest
	// movers lead; ties break by frame name for determinism.
	sort.Slice(rows, func(i, j int) bool {
		ai, aj := abs64(rows[i].DeltaNorm), abs64(rows[j].DeltaNorm)
		if ai != aj {
			return ai > aj
		}
		if rows[i].Frame.Func != rows[j].Frame.Func {
			return rows[i].Frame.Func < rows[j].Frame.Func
		}
		return rows[i].Frame.Module < rows[j].Frame.Module
	})
	return rows
}

func makeRow(fr symbolize.Frame, oldCount, newCount, oldTotal, newTotal int64) Row {
	oldN := scaled(oldCount, oldTotal)
	newN := scaled(newCount, newTotal)
	d := newN - oldN
	dir := Same
	switch {
	case d > 0:
		dir = Slower
	case d < 0:
		dir = Faster
	}
	return Row{
		Frame: fr, OldCount: oldCount, NewCount: newCount,
		OldTotalCount: oldTotal, NewTotalCount: newTotal,
		OldNorm: oldN, NewNorm: newN, DeltaNorm: d, Direction: dir,
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// pruneFold returns a copy of the tree with a [folded] node inserted
// wherever children are sub-threshold relative to their own parent's count
// for this version.
func pruneFold(root *merge.Node, total int64) *merge.Node {
	var walk func(n *merge.Node) *merge.Node
	walk = func(n *merge.Node) *merge.Node {
		out := &merge.Node{
			Frame: n.Frame, Self: n.Self, Total: n.Total,
			Count: n.Count, SelfCount: n.SelfCount,
			Children: make([]*merge.Node, 0, len(n.Children)),
		}
		var small []*merge.Node
		for _, c := range n.Children {
			if n.Count > 0 && c.Count*1000 < n.Count {
				small = append(small, c)
			} else {
				out.Children = append(out.Children, walk(c))
			}
		}
		if len(small) > 0 {
			f := &merge.Node{Frame: symbolize.FoldedFrame()}
			for _, c := range small {
				f.Total += c.Total
				f.Self += c.Total
				f.Count += c.Count
				f.SelfCount += c.Count
			}
			out.Children = append(out.Children, f)
		}
		sort.Slice(out.Children, func(i, j int) bool {
			if out.Children[i].Frame.Func != out.Children[j].Frame.Func {
				return out.Children[i].Frame.Func < out.Children[j].Frame.Func
			}
			return out.Children[i].Frame.Module < out.Children[j].Frame.Module
		})
		return out
	}
	return walk(root)
}

func unionNodes(a, b *merge.Node, oldTotal, newTotal int64) *TreeNode {
	out := &TreeNode{
		Frame:    a.Frame,
		OldCount: a.Count, NewCount: b.Count,
		OldTotalCount: oldTotal, NewTotalCount: newTotal,
		DeltaNorm: scaled(b.Count, newTotal) - scaled(a.Count, oldTotal),
	}
	ai := make(map[symbolize.Frame]*merge.Node, len(a.Children))
	for _, c := range a.Children {
		ai[c.Frame] = c
	}
	bi := make(map[symbolize.Frame]*merge.Node, len(b.Children))
	for _, c := range b.Children {
		bi[c.Frame] = c
	}
	seen := make(map[symbolize.Frame]bool)
	// Iterate the old side in its already name-sorted order first so the
	// output stays name-sorted after new-only frames are inserted.
	for _, ac := range a.Children {
		seen[ac.Frame] = true
		if bc, ok := bi[ac.Frame]; ok {
			out.Children = append(out.Children, unionNodes(ac, bc, oldTotal, newTotal))
		} else {
			out.Children = append(out.Children, unionNodes(ac, &merge.Node{Frame: ac.Frame}, oldTotal, newTotal))
		}
	}
	var newOnly []*merge.Node
	for _, bc := range b.Children {
		if !seen[bc.Frame] {
			newOnly = append(newOnly, bc)
		}
	}
	sort.Slice(newOnly, func(i, j int) bool {
		if newOnly[i].Frame.Func != newOnly[j].Frame.Func {
			return newOnly[i].Frame.Func < newOnly[j].Frame.Func
		}
		return newOnly[i].Frame.Module < newOnly[j].Frame.Module
	})
	for _, bc := range newOnly {
		out.Children = append(out.Children,
			unionNodes(&merge.Node{Frame: bc.Frame}, bc, oldTotal, newTotal))
	}
	return out
}
