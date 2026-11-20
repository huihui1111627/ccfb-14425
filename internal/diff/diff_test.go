package diff

import (
	"testing"

	"aquaprofile/internal/merge"
	"aquaprofile/internal/symbolize"
)

func fr(mod, fn string) symbolize.Frame { return symbolize.Frame{Module: mod, Func: fn} }

func TestNormalizationAndDirection(t *testing.T) {
	// Old: 1000 samples. New: 2000 samples, same shares everywhere.
	// Counts double but normalized shares must be identical -> all Same.
	oldSamples := []merge.Sample{
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "a"), fr("m", "x")}}, // 700
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "a"), fr("m", "y")}}, // 300
	}
	expand := func(base []merge.Sample, times int) []merge.Sample {
		var out []merge.Sample
		for i := 0; i < times; i++ {
			out = append(out, base...)
		}
		return out
	}
	oldS := expand(oldSamples, 500)
	newS := expand(oldSamples, 1000)
	d := Build(merge.Merge(oldS, false), merge.Merge(newS, false))
	for _, row := range d.Rows {
		if row.Direction != Same || row.DeltaNorm != 0 {
			t.Fatalf("equal shares flagged as changed: %+v", row)
		}
		if row.OldNorm != row.NewNorm {
			t.Fatalf("normalization failed: %+v", row)
		}
	}
}

func TestSlowerFasterRanking(t *testing.T) {
	// Old shares: slow 20%, fast 30%, idle 50%.
	// New shares: slow 60%, fast 10%, idle 30%.
	oldS := make([]merge.Sample, 0, 1000)
	newS := make([]merge.Sample, 0, 1000)
	add := func(dst *[]merge.Sample, f symbolize.Frame, n int) {
		for i := 0; i < n; i++ {
			*dst = append(*dst, merge.Sample{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), f}})
		}
	}
	add(&oldS, fr("m", "slow"), 200)
	add(&oldS, fr("m", "fast"), 300)
	add(&oldS, fr("m", "idle"), 500)
	add(&newS, fr("m", "slow"), 600)
	add(&newS, fr("m", "fast"), 100)
	add(&newS, fr("m", "idle"), 300)

	d := Build(merge.Merge(oldS, false), merge.Merge(newS, false))
	dir := map[string]Row{}
	for _, r := range d.Rows {
		dir[r.Frame.Func] = r
	}
	if dir["slow"].Direction != Slower || dir["fast"].Direction != Faster || dir["idle"].Direction != Faster {
		t.Fatalf("directions wrong: %+v", dir)
	}
	// slow moves +40 percentage points, fast -20, idle -20: slow must lead.
	if d.Rows[0].Frame.Func != "slow" {
		t.Fatalf("top mover = %s, want slow", d.Rows[0].Frame.Func)
	}
	// Integer exactness: 200/1000 vs 600/1000 -> scaled values.
	if dir["slow"].OldNorm != 200_000_000 || dir["slow"].NewNorm != 600_000_000 {
		t.Fatalf("scaled norms wrong: %+v", dir["slow"])
	}
}

func TestDifferentTotalsNormalized(t *testing.T) {
	// 100 old samples, 10000 new samples; function a keeps a 10% share.
	oldS := []merge.Sample{
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "a")}},
	}
	oldS = repeat(oldS, 10)
	oldS = append(oldS, repeat([]merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "b")}}}, 90)...)
	newA := repeat([]merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "a")}}}, 1000)
	newB := repeat([]merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "b")}}}, 9000)
	d := Build(merge.Merge(oldS, false), merge.Merge(append(newA, newB...), false))
	for _, r := range d.Rows {
		if r.DeltaNorm != 0 {
			t.Fatalf("10%% share across x100 sample totals moved: %+v", r)
		}
	}
}

func repeat(s []merge.Sample, n int) []merge.Sample {
	out := make([]merge.Sample, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return out
}

func TestDiffTreeUnionAndFolding(t *testing.T) {
	// Common path plus a new-only branch and a sub-threshold branch.
	oldS := []merge.Sample{
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "common"), fr("m", "leafA")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "common"), fr("m", "leafA")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyOld")}},
	}
	newS := []merge.Sample{
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "common"), fr("m", "leafA")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "common"), fr("m", "leafB")}},
		{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyNew")}},
	}
	d := Build(merge.Merge(oldS, false), merge.Merge(newS, false))
	root := d.Tree
	if root.Frame.Func != "" || root.OldCount != 3 || root.NewCount != 3 {
		t.Fatalf("diff root wrong: %+v", root)
	}
	// Name-sorted children; new-only branch present with zero old count.
	index := map[string]*TreeNode{}
	for _, c := range root.Children[0].Children { // children of "root"
		index[c.Frame.Func] = c
	}
	common, ok := index["common"]
	if !ok || common.OldCount != 2 || common.NewCount != 2 {
		t.Fatalf("common branch wrong: %+v", index)
	}
	leafIndex := map[string]*TreeNode{}
	for _, c := range common.Children {
		leafIndex[c.Frame.Func] = c
	}
	if leafIndex["leafB"].OldCount != 0 || leafIndex["leafB"].NewCount != 1 {
		t.Fatalf("new-only branch wrong: %+v", leafIndex)
	}
	if leafIndex["leafA"].OldCount != 2 || leafIndex["leafA"].NewCount != 1 {
		t.Fatalf("old branch counts wrong: %+v", leafIndex)
	}
	// A genuinely sub-threshold branch: 1 of 1001 samples (< 1/1000 of
	// the parent's 1001). The folded nodes may differ per side, so build
	// each version with its own tiny branch and check folding exists.
	bigOld := repeat([]merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "big")}}}, 1000)
	tinyOld := []merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyOld")}}}
	bigNew := repeat([]merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "big")}}}, 1000)
	tinyNew := []merge.Sample{{Weight: 1, Frames: []symbolize.Frame{fr("m", "root"), fr("m", "tinyNew")}}}
	d2 := Build(merge.Merge(append(bigOld, tinyOld...), false),
		merge.Merge(append(bigNew, tinyNew...), false))
	var foundFolded bool
	for _, c := range d2.Tree.Children[0].Children { // children of "root"
		if c.Frame.IsFolded() {
			foundFolded = true
			if c.OldCount != 1 || c.NewCount != 1 {
				t.Fatalf("folded counts wrong: %+v", c)
			}
		}
	}
	if !foundFolded {
		t.Fatalf("sub-1/1000 node not folded in diff tree")
	}
}

func TestDiffDeterministic(t *testing.T) {
	s := []merge.Sample{
		{Weight: 2, Frames: []symbolize.Frame{fr("m", "a"), fr("m", "x")}},
		{Weight: 3, Frames: []symbolize.Frame{fr("m", "a"), fr("m", "y")}},
	}
	d1 := Build(merge.Merge(s, false), merge.Merge(reverse(s), false))
	d2 := Build(merge.Merge(reverse(s), false), merge.Merge(s, false))
	if d1.Rows[0].Frame != d1.Rows[0].Frame { // sanity
		t.Fatal("impossible")
	}
	// Re-run must be byte-identical (map inputs are the same multiset).
	d1b := Build(merge.Merge(s, false), merge.Merge(reverse(s), false))
	if len(d1b.Rows) != len(d1.Rows) {
		t.Fatal("row count drift")
	}
	for i := range d1.Rows {
		if d1.Rows[i] != d1b.Rows[i] {
			t.Fatalf("diff output not deterministic at row %d", i)
		}
	}
	// And swapping versions must flip every delta sign.
	for i := range d1.Rows {
		if d1.Rows[i].DeltaNorm != -d2.Rows[len(d2.Rows)-1-i].DeltaNorm &&
			d1.Rows[i].Frame == d2.Rows[len(d2.Rows)-1-i].Frame {
			t.Fatalf("version swap did not invert delta at row %d", i)
		}
	}
}

func reverse(s []merge.Sample) []merge.Sample {
	out := append([]merge.Sample(nil), s...)
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}
