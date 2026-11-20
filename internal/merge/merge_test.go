package merge

import (
	"math/rand"
	"reflect"
	"testing"

	"aquaprofile/internal/symbolize"
)

func fr(mod, fn string) symbolize.Frame { return symbolize.Frame{Module: mod, Func: fn} }

// bruteForce is a deliberately naive, independent re-implementation used
// only by tests: it walks every sample from scratch with plain loops and
// maps, sharing no code paths with Merge.
type bruteForce struct {
	samples []Sample
}

func (b *bruteForce) nodeTotals() map[pathKey]stat {
	// node totals keyed by the full prefix path (a node is an occurrence
	// in the tree, not a function).
	out := map[pathKey]stat{}
	for _, s := range b.samples {
		for i := range s.Frames {
			k := pathOf(s.Frames[:i+1])
			st := out[k]
			st.total += s.Weight
			st.count++
			if i == len(s.Frames)-1 {
				st.self += s.Weight
				st.selfCount++
			}
			out[k] = st
		}
	}
	return out
}

func (b *bruteForce) funcStats() map[symbolize.Frame]stat {
	out := map[symbolize.Frame]stat{}
	for _, s := range b.samples {
		seen := map[symbolize.Frame]bool{}
		for _, f := range s.Frames {
			if !seen[f] {
				seen[f] = true
				st := out[f]
				st.total += s.Weight
				st.count++
				out[f] = st
			}
		}
		leaf := s.Frames[len(s.Frames)-1]
		st := out[leaf]
		st.self += s.Weight
		st.selfCount++
		out[leaf] = st
	}
	return out
}

func (b *bruteForce) holes() (gap, unk map[symbolize.Frame]stat) {
	gap, unk = map[symbolize.Frame]stat{}, map[symbolize.Frame]stat{}
	for _, s := range b.samples {
		seenGap, seenUnk := map[symbolize.Frame]bool{}, map[symbolize.Frame]bool{}
		for _, f := range s.Frames {
			target := gap
			seen := seenGap
			if f.IsUnknown() {
				target, seen = unk, seenUnk
			} else if !f.IsGap() {
				continue
			}
			if !seen[f] {
				seen[f] = true
				st := target[f]
				st.total += s.Weight
				st.count++
				target[f] = st
			}
		}
	}
	return
}

type pathKey struct{ key string }

// pathOf renders a frame prefix into a collision-free comparable key.
// Backslashes are doubled and separators backslash-escaped so any frame
// names stay unambiguous.
func pathOf(frames []symbolize.Frame) pathKey {
	var b []byte
	enc := func(s string) {
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch c {
			case '\\':
				b = append(b, '\\', '\\')
			case '!', '|':
				b = append(b, '\\', c)
			default:
				b = append(b, c)
			}
		}
	}
	for _, f := range frames {
		enc(f.Module)
		b = append(b, '!')
		enc(f.Func)
		b = append(b, '|')
	}
	return pathKey{string(b)}
}

type stat struct {
	self, total, count, selfCount int64
}

// collectTree turns the merged tree into pathKey->stat maps for comparison.
func collectTree(n *Node, prefix []symbolize.Frame, totals map[pathKey]stat) {
	if len(prefix) > 0 {
		k := pathOf(prefix)
		totals[k] = stat{total: n.Total, count: n.Count, self: n.Self, selfCount: n.SelfCount}
	}
	for _, c := range n.Children {
		collectTree(c, append(prefix, c.Frame), totals)
	}
}

func TestMergeMatchesBruteForce(t *testing.T) {
	samples := []Sample{
		{Weight: 5, Frames: []symbolize.Frame{fr("c", "main"), fr("c", "work"), fr("d", "io")}},
		{Weight: 10, Frames: []symbolize.Frame{fr("c", "main"), fr("c", "work"), fr("d", "io")}},
		{Weight: 7, Frames: []symbolize.Frame{fr("c", "main"), fr("c", "work"), fr("r", "alloc")}},
		// recursion: alloc appears twice on one stack
		{Weight: 3, Frames: []symbolize.Frame{fr("c", "main"), fr("r", "alloc"), fr("r", "alloc")}},
		{Weight: 4, Frames: []symbolize.Frame{fr("c", "main"), symbolize.GapFrame("d")}},
		{Weight: 6, Frames: []symbolize.Frame{fr("c", "main"), symbolize.UnknownFrame(0xdead)}},
		{Weight: 2, Frames: []symbolize.Frame{fr("c", "main"), fr("c", "work")}}, // ends inside work
	}
	bf := &bruteForce{samples}
	res := Merge(samples, false)

	// 1. tree node totals vs brute-force prefix paths
	gotTotals := map[pathKey]stat{}
	collectTree(res.Root, nil, gotTotals)
	wantTotals := bf.nodeTotals()
	if len(gotTotals) != len(wantTotals) {
		t.Fatalf("tree node count = %d, brute force %d", len(gotTotals), len(wantTotals))
	}
	for k, want := range wantTotals {
		got, ok := gotTotals[k]
		if !ok {
			t.Fatalf("merged tree missing path %q", k.key)
		}
		if got != want {
			t.Fatalf("node %q: merged %+v, brute %+v", k.key, got, want)
		}
	}

	// 2. function totals vs brute-force distinct-frame aggregation
	wantFn := bf.funcStats()
	if len(res.Functions) != len(wantFn) {
		t.Fatalf("function count = %d, brute %d", len(res.Functions), len(wantFn))
	}
	for _, f := range res.Functions {
		want := wantFn[f.Frame]
		got := stat{self: f.Self, total: f.Total, count: f.Count, selfCount: f.SelfCount}
		if got != want {
			t.Fatalf("func %v: merged %+v, brute %+v", f.Frame, got, want)
		}
	}

	// The recursive frame must be charged exactly once for its sample.
	alloc := findFunc(res, fr("r", "alloc"))
	if alloc.Total != 7+3 { // one non-recursive sample + the recursive one
		t.Fatalf("recursive total double-counted: got %d want 10", alloc.Total)
	}
	if alloc.Count != 2 {
		t.Fatalf("recursive count = %d, want 2 distinct samples", alloc.Count)
	}

	// 3. gaps and unknowns vs brute force
	wantGap, wantUnk := bf.holes()
	checkHoles := func(name string, got []HoleStat, want map[symbolize.Frame]stat) {
		if len(got) != len(want) {
			t.Fatalf("%s count = %d, brute %d", name, len(got), len(want))
		}
		for _, h := range got {
			w := want[h.Frame]
			if h.Weight != w.total || h.Samples != w.count {
				t.Fatalf("%s %v: merged (%d,%d), brute (%d,%d)",
					name, h.Frame, h.Weight, h.Samples, w.total, w.count)
			}
		}
	}
	checkHoles("gap", res.Gaps, wantGap)
	checkHoles("unknown", res.Unknowns, wantUnk)

	// 4. root bookkeeping
	var totalW int64
	for _, s := range samples {
		totalW += s.Weight
	}
	if res.TotalWeight != totalW || res.SampleCount != int64(len(samples)) ||
		res.Root.Total != totalW || res.Root.Count != int64(len(samples)) {
		t.Fatalf("root totals wrong: %+v", res)
	}
}

func findFunc(res *Result, f symbolize.Frame) FunctionStat {
	for _, x := range res.Functions {
		if x.Frame == f {
			return x
		}
	}
	return FunctionStat{}
}

func TestMergeOrderIndependence(t *testing.T) {
	base := []Sample{
		{Weight: 5, Frames: []symbolize.Frame{fr("c", "a"), fr("d", "x")}},
		{Weight: 8, Frames: []symbolize.Frame{fr("c", "a"), fr("d", "y")}},
		{Weight: 3, Frames: []symbolize.Frame{fr("c", "b"), fr("d", "x")}},
		{Weight: 9, Frames: []symbolize.Frame{fr("c", "a"), fr("d", "x"), fr("r", "z")}},
	}
	rng := rand.New(rand.NewSource(42))
	reference := Merge(base, false)
	for trial := 0; trial < 20; trial++ {
		shuffled := append([]Sample(nil), base...)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		got := Merge(shuffled, false)
		if !reflect.DeepEqual(got, reference) {
			t.Fatalf("merge result depends on ingestion order (trial %d)", trial)
		}
	}
}

func TestSiblingsSortedByName(t *testing.T) {
	samples := []Sample{
		{Weight: 100, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "zeta"), fr("m", "leaf1")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "alpha"), fr("m", "leaf2")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "mid"), fr("m", "leaf3")}},
	}
	res := Merge(samples, false)
	kids := res.Root.Children[0].Children // children of "root"
	want := []string{"alpha", "mid", "zeta"}
	for i, k := range kids {
		if k.Frame.Func != want[i] {
			t.Fatalf("sibling order = %v, want name-sorted %v", names(kids), want)
		}
	}
}

func names(ns []*Node) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.Frame.Func
	}
	return out
}

func TestFoldingThreshold(t *testing.T) {
	// Parent width 2000: the 1/1000 boundary is weight 2. A child at
	// exactly 2 is KEPT (the rule is strictly-less-than); weight-1
	// children are folded together.
	samples := []Sample{
		{Weight: 1996, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "big"), fr("m", "l")}},
		{Weight: 2, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "exact"), fr("m", "l")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyA"), fr("m", "l")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyB"), fr("m", "l")}},
	}
	res := Merge(samples, true)
	rootKids := res.Root.Children[0].Children // children of "root"
	var hasFolded, hasExact, hasTiny bool
	var folded *Node
	for _, k := range rootKids {
		switch k.Frame.Func {
		case symbolize.FoldedFunc:
			hasFolded = true
			folded = k
		case "exact":
			hasExact = true
		case "tinyA", "tinyB":
			hasTiny = true
		}
	}
	if !hasFolded {
		t.Fatalf("expected a [folded] node among %v", names(rootKids))
	}
	if !hasExact {
		t.Fatalf("node at exactly 1/1000 must not be folded")
	}
	if hasTiny {
		t.Fatalf("sub-threshold nodes should have been absorbed")
	}
	// Weight conservation: the folded node carries both collapsed subtrees
	// and is a leaf.
	if len(folded.Children) != 0 || folded.Total != 2 || folded.Self != 2 {
		t.Fatalf("folded node bookkeeping wrong: %+v", folded)
	}
	// Folded output must still match brute-force totals at the kept level.
	keptWeight := int64(0)
	for _, k := range rootKids {
		keptWeight += k.Total
	}
	if keptWeight != 2000 {
		t.Fatalf("weight lost in folding: %d/2000", keptWeight)
	}
}

func TestDifferentWeightsNotRawCounts(t *testing.T) {
	// Two processes: same number of samples, different periods. The merge
	// must reflect period-weighted time, never equal halves.
	samples := []Sample{
		{Weight: 20, Frames: []symbolize.Frame{fr("a", "slow")}},
		{Weight: 20, Frames: []symbolize.Frame{fr("a", "slow")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("a", "fast")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("a", "fast")}},
	}
	res := Merge(samples, false)
	slow := findFunc(res, fr("a", "slow"))
	fast := findFunc(res, fr("a", "fast"))
	if slow.Total != 40 || fast.Total != 2 {
		t.Fatalf("weights ignored: slow=%d fast=%d", slow.Total, fast.Total)
	}
}

func TestEmptyAndDegenerateInputs(t *testing.T) {
	res := Merge(nil, true)
	if res.TotalWeight != 0 || res.SampleCount != 0 || len(res.Root.Children) != 0 {
		t.Fatalf("empty merge not empty: %+v", res)
	}
	bad := Merge([]Sample{
		{Weight: 0, Frames: []symbolize.Frame{fr("m", "x")}},
		{Weight: 5, Frames: nil},
		{Weight: 5, Frames: []symbolize.Frame{fr("m", "y")}},
	}, true)
	if bad.SampleCount != 1 || bad.TotalWeight != 5 {
		t.Fatalf("invalid samples not skipped: %+v", bad)
	}
}
