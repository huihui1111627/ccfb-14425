package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"aquaprofile/internal/diff"
	"aquaprofile/internal/gen"
	"aquaprofile/internal/merge"
	"aquaprofile/internal/server"
	"aquaprofile/internal/store"
	"aquaprofile/internal/symbolize"
)

// bruteTruth is a fully independent, loop-based re-implementation of every
// aggregation the service promises. It shares no code with the production
// merge/diff packages beyond the Frame type and the HTTP DTOs. All numbers
// below are recomputed sample by sample.
type bruteTruth struct {
	// key: program|version -> samples (resolved, period-weighted)
	samples map[string][]bruteSample
}

type bruteSample struct {
	weight int64
	frames []symbolize.Frame
	labels map[string]string
	timeNs uint64
}

func buildBruteFleet(t *testing.T, fleet *gen.Fleet) *bruteTruth {
	t.Helper()
	bt := &bruteTruth{samples: map[string][]bruteSample{}}
	for _, p := range fleet.Procs {
		res := symbolize.NewResolver(p.Modules)
		key := p.Program + "|" + p.Version
		for _, s := range p.Batch.Samples {
			frames := make([]symbolize.Frame, len(s.Addresses))
			for i, a := range s.Addresses {
				frames[i] = res.Resolve(a)
			}
			bt.samples[key] = append(bt.samples[key], bruteSample{
				weight: p.Period, frames: frames,
				labels: s.Labels, timeNs: s.TimeNs,
			})
		}
	}
	return bt
}

func (bt *bruteTruth) pick(program, version string, f store.Filter) []bruteSample {
	var out []bruteSample
	for _, s := range bt.samples[program+"|"+version] {
		if f.StartNs != 0 && int64(s.timeNs) < f.StartNs {
			continue
		}
		if f.EndNs != 0 && int64(s.timeNs) >= f.EndNs {
			continue
		}
		ok := true
		for k, v := range f.Labels {
			if s.labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, s)
		}
	}
	return out
}

// bruteMergeHoles recomputes gap/unknown stats correctly per sample.
func bruteMergeHoles(samples []bruteSample) map[symbolize.Frame][2]int64 {
	out := map[symbolize.Frame][2]int64{}
	for _, s := range samples {
		seen := map[symbolize.Frame]bool{}
		for _, fr := range s.frames {
			if !fr.IsGap() && !fr.IsUnknown() {
				continue
			}
			if seen[fr] {
				continue
			}
			seen[fr] = true
			v := out[fr]
			v[0]++
			v[1] += s.weight
			out[fr] = v
		}
	}
	return out
}

type bruteFn struct{ total, self, count, selfCount int64 }

func bruteFunctions(samples []bruteSample) map[symbolize.Frame]bruteFn {
	out := map[symbolize.Frame]bruteFn{}
	for _, s := range samples {
		seen := map[symbolize.Frame]bool{}
		for _, fr := range s.frames {
			if !seen[fr] {
				seen[fr] = true
				v := out[fr]
				v.total += s.weight
				v.count++
				out[fr] = v
			}
		}
		// Self time is charged only on the leaf frame, once per sample.
		leaf := s.frames[len(s.frames)-1]
		v := out[leaf]
		v.self += s.weight
		v.selfCount++
		out[leaf] = v
	}
	return out
}

// bruteTree walks samples again to build the prefix tree (nodes keyed by path).
func bruteTree(samples []bruteSample) map[string]bruteFn {
	type node struct{ total, self, count, selfCount int64 }
	tree := map[string]*node{}
	for _, s := range samples {
		for i := range s.frames {
			key := pathKey(s.frames[:i+1])
			n := tree[key]
			if n == nil {
				n = &node{}
				tree[key] = n
			}
			n.total += s.weight
			n.count++
			if i == len(s.frames)-1 {
				n.self += s.weight
				n.selfCount++
			}
		}
	}
	out := map[string]bruteFn{}
	for k, n := range tree {
		out[k] = bruteFn{total: n.total, self: n.self, count: n.count, selfCount: n.selfCount}
	}
	return out
}

func pathKey(frames []symbolize.Frame) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f.Module)
		b.WriteByte('!')
		b.WriteString(f.Func)
		b.WriteByte('|')
	}
	return b.String()
}

func serviceTreeMap(n *serverNodeDTO, prefix string, out map[string]bruteFn) {
	for _, c := range n.Children {
		key := prefix + c.Module + "!" + c.Func + "|"
		out[key] = bruteFn{total: c.Total, self: c.Self, count: c.Count, selfCount: c.SelfCount}
		serviceTreeMap(c, key, out)
	}
}

type serverNodeDTO struct {
	Module    string
	Func      string
	Self      int64 `json:"self_ns"`
	Total     int64 `json:"total_ns"`
	Count     int64
	SelfCount int64 `json:"self_count"`
	Children  []*serverNodeDTO
}

func TestEndToEndFleetAgainstBruteForce(t *testing.T) {
	cfg := gen.Config{Programs: 8, ProcsPerProg: 3, SamplesPerVer: 300, Seed: 12345}
	fleet := gen.Generate(cfg)
	st := store.New()
	if err := fleet.Upload(st); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	defer srv.Close()

	bt := buildBruteFleet(t, fleet)
	client := &http.Client{}
	get := func(path string, dst any) {
		t.Helper()
		resp, err := client.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s -> %s", path, resp.Status)
		}
		if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
			t.Fatal(err)
		}
	}

	programs := st.Programs()
	if len(programs) != cfg.Programs {
		t.Fatalf("program count = %d, want %d", len(programs), cfg.Programs)
	}
	filters := []store.Filter{
		{},
		{Labels: map[string]string{"tank": "T03"}},
		{Labels: map[string]string{"pump": "2", "rack": "1"}},
		{StartNs: 1_700_000_000_050_000_000, EndNs: 1_700_000_000_250_000_000},
	}

	for _, program := range programs {
		oldV, newV := gen.Versions()
		for _, version := range []string{oldV, newV} {
			for fi, f := range filters {
				path := fmt.Sprintf("/api/flame?program=%s&version=%s", program, version)
				for k, v := range f.Labels {
					path += "&label." + k + "=" + v
				}
				if f.StartNs != 0 {
					path += fmt.Sprintf("&start_ns=%d&end_ns=%d", f.StartNs, f.EndNs)
				}
				var flame struct {
					SampleCount int64 `json:"sample_count"`
					TotalWeight int64 `json:"total_weight_ns"`
					Root        *serverNodeDTO
					Functions   []struct {
						Module, Func string
						Self         int64 `json:"self_ns"`
						Total        int64 `json:"total_ns"`
						Count        int64
						SelfCount    int64 `json:"self_count"`
					}
					Gaps []struct {
						Module, Func, Kind string
						Samples            int64
						Weight             int64 `json:"weight_ns"`
					}
					Unknowns []struct {
						Module, Func, Kind string
						Samples            int64
						Weight             int64 `json:"weight_ns"`
					}
				}
				get(path, &flame)

				wantSamples := bt.pick(program, version, f)
				if flame.SampleCount != int64(len(wantSamples)) {
					t.Fatalf("%s/%s filter %d: sample count %d != brute %d",
						program, version, fi, flame.SampleCount, len(wantSamples))
				}
				var wantWeight int64
				for _, s := range wantSamples {
					wantWeight += s.weight
				}
				if flame.TotalWeight != wantWeight || flame.Root.Total != wantWeight {
					t.Fatalf("%s/%s filter %d: weight %d/%d != brute %d",
						program, version, fi, flame.TotalWeight, flame.Root.Total, wantWeight)
				}
				wantTree := bruteTree(wantSamples)
				gotTree := map[string]bruteFn{}
				serviceTreeMap(flame.Root, "", gotTree)
				// The service tree may contain a [folded] node; remove it
				// from the exact-match comparison.
				for k := range gotTree {
					if strings.Contains(k, symbolize.FoldedFunc) {
						delete(gotTree, k)
					}
				}
				// Folding only removes sub-threshold nodes, so every node
				// the service emits must match the brute tree exactly.
				for key, got := range gotTree {
					want, ok := wantTree[key]
					if !ok {
						t.Fatalf("service node %s absent from brute tree", key)
					}
					if got != want {
						t.Fatalf("node %s mismatch: service %+v brute %+v", key, got, want)
					}
				}

				// Function table vs brute function aggregation. Folding only
				// affects the tree, so all functions must match exactly.
				wantFn := bruteFunctions(wantSamples)
				if len(flame.Functions) != len(wantFn) {
					t.Fatalf("%s/%s f%d: function count %d != brute %d",
						program, version, fi, len(flame.Functions), len(wantFn))
				}
				for _, row := range flame.Functions {
					frame := symbolize.Frame{Module: row.Module, Func: row.Func}
					want := wantFn[frame]
					if row.Total != want.total || row.Self != want.self ||
						row.Count != want.count || row.SelfCount != want.selfCount {
						t.Fatalf("%s/%s f%d func %s: service (%d,%d,%d,%d) brute (%d,%d,%d,%d)",
							program, version, fi, frame,
							row.Self, row.Total, row.Count, row.SelfCount,
							want.self, want.total, want.count, want.selfCount)
					}
				}

				// Gap/unknown accounting vs brute.
				wantHoles := bruteMergeHoles(wantSamples)
				type holeRow struct {
					Module, Func, Kind string
					Samples            int64
					Weight             int64 `json:"weight_ns"`
				}
				checkHoles := func(name string, rows []holeRow) {
					for _, h := range rows {
						want := wantHoles[symbolize.Frame{Module: h.Module, Func: h.Func}]
						if h.Samples != want[0] || h.Weight != want[1] {
							t.Fatalf("%s/%s f%d %s %s!%s: service (%d,%d) brute (%d,%d)",
								program, version, fi, name, h.Module, h.Func,
								h.Samples, h.Weight, want[0], want[1])
						}
					}
				}
				var gapRows, unkRows []holeRow
				for _, h := range flame.Gaps {
					gapRows = append(gapRows, holeRow{h.Module, h.Func, h.Kind, h.Samples, h.Weight})
				}
				for _, h := range flame.Unknowns {
					unkRows = append(unkRows, holeRow{h.Module, h.Func, h.Kind, h.Samples, h.Weight})
				}
				checkHoles("gap", gapRows)
				checkHoles("unknown", unkRows)
				var holeSamples int64
				for _, h := range wantHoles {
					holeSamples += h[0]
				}
				var servedHoleSamples int64
				for _, h := range flame.Gaps {
					servedHoleSamples += h.Samples
				}
				for _, h := range flame.Unknowns {
					servedHoleSamples += h.Samples
				}
				if holeSamples != servedHoleSamples {
					t.Fatalf("hole sample total %d != served %d", holeSamples, servedHoleSamples)
				}
			}
		}
	}
}

func TestDiffFindsPlantedSlowdowns(t *testing.T) {
	cfg := gen.Config{Programs: 10, ProcsPerProg: 2, SamplesPerVer: 400, Seed: 777}
	fleet := gen.Generate(cfg)
	st := store.New()
	if err := fleet.Upload(st); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(st))
	defer srv.Close()

	for _, program := range st.Programs() {
		url := fmt.Sprintf("%s/api/diff?program=%s&old_version=%s&new_version=%s",
			srv.URL, program, "v1.0", "v2.0")
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var dto struct {
			Rows []struct {
				Module, Func, Direction string
				OldCount, NewCount      int64
				DeltaShare              float64 `json:"delta_share"`
			}
		}
		if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()

		// The diff uses cumulative ("samples passing through") counts, so
		// ancestors of the planted leaf functions legitimately move too.
		// The invariant that matters: among LEAF templates, exactly the
		// planted functions are slower, runtime.idle is the only faster
		// leaf, and every other leaf template stays exactly unchanged.
		targets := map[string]bool{}
		for _, name := range fleet.SlowTargets {
			targets[name] = true
		}
		leafSet := map[string]bool{}
		for _, tpl := range fleet.Templates {
			leafSet[tpl.Name] = true
		}
		var slowerLeaves, fasterLeaves []string
		for _, row := range dto.Rows {
			if !leafSet[row.Func] {
				continue // ancestor frame: cumulative movement is expected
			}
			switch row.Direction {
			case "slower":
				slowerLeaves = append(slowerLeaves, row.Func)
			case "faster":
				fasterLeaves = append(fasterLeaves, row.Func)
			case "same":
				if row.DeltaShare != 0 || row.OldCount != row.NewCount {
					t.Fatalf("%s: leaf %s labeled same but drifted: %+v",
						program, row.Func, row)
				}
			}
		}
		if len(slowerLeaves) != len(targets) {
			t.Fatalf("%s: slower leaves = %v, want exactly %v",
				program, slowerLeaves, fleet.SlowTargets)
		}
		for _, name := range slowerLeaves {
			if !targets[name] {
				t.Fatalf("%s: non-target leaf %s marked slower", program, name)
			}
		}
		if len(fasterLeaves) != 1 || fasterLeaves[0] != "runtime.idle" {
			t.Fatalf("%s: faster leaves = %v, want only runtime.idle",
				program, fasterLeaves)
		}
		// Idle gives away all its samples in the new version.
		for _, row := range dto.Rows {
			if row.Func == "runtime.idle" && row.NewCount != 0 {
				t.Fatalf("%s: idle new count = %d, want 0", program, row.NewCount)
			}
		}
		// Among leaf templates, the planted targets must occupy the top
		// |targets| slots of the absolute-movement ranking (idle aside),
		// i.e. no non-target leaf outranks a planted one.
		var leafRanks []struct {
			name string
			rank int
		}
		for i, row := range dto.Rows {
			if leafSet[row.Func] {
				leafRanks = append(leafRanks, struct {
					name string
					rank int
				}{row.Func, i})
			}
		}
		// Sort leaf rows by their global rank, which already reflects
		// |delta| descending. The first |targets| non-idle leaves must be
		// exactly the planted set.
		var topLeafMovers []string
		for _, lr := range leafRanks {
			if lr.name == "runtime.idle" {
				continue
			}
			topLeafMovers = append(topLeafMovers, lr.name)
		}
		if len(topLeafMovers) < len(targets) {
			t.Fatalf("%s: not enough leaf movers", program)
		}
		for i := 0; i < len(targets); i++ {
			if !targets[topLeafMovers[i]] {
				t.Fatalf("%s: non-target leaf %q outranks planted targets %v",
					program, topLeafMovers[i], topLeafMovers[:len(targets)])
			}
		}
	}
}

func TestDiffMatchesBruteForceRationalMath(t *testing.T) {
	// Small hand-built fleet where expected rational deltas are exact.
	st := store.New()
	mods := []symbolize.Module{{
		Name: "m", Base: 0x1000, Size: 0x2000,
		Symbols: []symbolize.Symbol{
			{Name: "a", Addr: 0x1000, Size: 0x100},
			{Name: "b", Addr: 0x1200, Size: 0x100},
		},
	}}
	for _, v := range []string{"v1.0", "v2.0"} {
		_ = st.PutModules(store.ModuleUpload{Program: "p", Version: v, Pid: 1, Modules: mods})
	}
	push := func(version string, addr uint64, n int) {
		b := store.Batch{Program: "p", Version: version, Pid: 1, PeriodNs: 1}
		for i := 0; i < n; i++ {
			b.Samples = append(b.Samples, store.Sample{
				TimeNs: uint64(i + 1), Addresses: []uint64{addr},
				Labels: map[string]string{"tank": "T01"},
			})
		}
		_ = st.PutBatch(b)
	}
	// v1: a=80, b=20 (total 100). v2: a=60, b=140 (total 200).
	push("v1.0", 0x1010, 80)
	push("v1.0", 0x1210, 20)
	push("v2.0", 0x1010, 60)
	push("v2.0", 0x1210, 140)

	srv := httptest.NewServer(server.New(st))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/diff?program=p&old_version=v1.0&new_version=v2.0")
	if err != nil {
		t.Fatal(err)
	}
	var dto struct {
		Rows []struct {
			Func       string
			OldShare   float64 `json:"old_share"`
			NewShare   float64 `json:"new_share"`
			DeltaShare float64 `json:"delta_share"`
			Direction  string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	want := map[string][3]float64{
		// func: {oldShare, newShare, deltaShare}
		"a": {0.8, 0.3, -0.5},
		"b": {0.2, 0.7, 0.5},
	}
	for _, row := range dto.Rows {
		w := want[row.Func]
		if !floatEq(row.OldShare, w[0]) || !floatEq(row.NewShare, w[1]) || !floatEq(row.DeltaShare, w[2]) {
			t.Fatalf("%s shares = (%f,%f,%f), want (%f,%f,%f)",
				row.Func, row.OldShare, row.NewShare, row.DeltaShare, w[0], w[1], w[2])
		}
		wantDir := "faster"
		if row.DeltaShare > 0 {
			wantDir = "slower"
		}
		if row.Direction != wantDir {
			t.Fatalf("%s direction = %s, want %s", row.Func, row.Direction, wantDir)
		}
	}

	// Cross-check the production diff package directly against independent
	// rational comparisons on the same data.
	oldSamples := st.Query("p", "v1.0", store.Filter{})
	newSamples := st.Query("p", "v2.0", store.Filter{})
	d := diff.Build(merge.Merge(oldSamples, false), merge.Merge(newSamples, false))
	for _, r := range d.Rows {
		oldRat := r.OldCount * 1_000_000_000 / r.OldTotalCount
		newRat := r.NewCount * 1_000_000_000 / r.NewTotalCount
		if r.OldNorm != oldRat || r.NewNorm != newRat || r.DeltaNorm != newRat-oldRat {
			t.Fatalf("rational math mismatch on %s", r.Frame)
		}
	}
}

func floatEq(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

func TestFoldingConservesWeightAndThreshold(t *testing.T) {
	// Direct unit-level check through the HTTP response: every folded
	// node's weight plus its kept siblings equals the parent total at
	// every level, and no kept node is below 1/1000 of the root.
	fleet := gen.Generate(gen.Config{Programs: 3, ProcsPerProg: 2, SamplesPerVer: 500, Seed: 55})
	st := store.New()
	_ = fleet.Upload(st)
	srv := httptest.NewServer(server.New(st))
	defer srv.Close()

	for _, program := range st.Programs() {
		resp, err := http.Get(fmt.Sprintf("%s/api/flame?program=%s&version=v1.0", srv.URL, program))
		if err != nil {
			t.Fatal(err)
		}
		var flame struct {
			TotalWeight int64 `json:"total_weight_ns"`
			Root        *serverNodeDTO
		}
		if err := json.NewDecoder(resp.Body).Decode(&flame); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var walk func(n *serverNodeDTO)
		walk = func(n *serverNodeDTO) {
			var childSum int64
			for _, c := range n.Children {
				childSum += c.Total
				// Threshold is relative to the parent width, not the root.
				if !strings.Contains(c.Func, symbolize.FoldedFunc) &&
					n.Total > 0 && c.Total*1000 < n.Total {
					t.Fatalf("%s: kept node %s!%s below 1/1000 of parent",
						program, c.Module, c.Func)
				}
				if c.Func == symbolize.FoldedFunc && len(c.Children) != 0 {
					t.Fatalf("folded node must be a leaf")
				}
				walk(c)
			}
			if len(n.Children) > 0 && childSum != n.Total {
				t.Fatalf("%s node %s!%s: children sum %d != total %d",
					program, n.Module, n.Func, childSum, n.Total)
			}
		}
		walk(flame.Root)
	}
}

func TestShuffledIngestionGivesIdenticalAPIOutput(t *testing.T) {
	cfg := gen.Config{Programs: 4, ProcsPerProg: 2, SamplesPerVer: 200, Seed: 31337}
	serve := func(seed int64) []byte {
		fleet := gen.Generate(cfg)
		rng := rand.New(rand.NewSource(seed))
		// Shuffle process upload order and sample order within batches.
		rng.Shuffle(len(fleet.Procs), func(i, j int) {
			fleet.Procs[i], fleet.Procs[j] = fleet.Procs[j], fleet.Procs[i]
		})
		st := store.New()
		for _, p := range fleet.Procs {
			_ = st.PutModules(store.ModuleUpload{
				Program: p.Program, Version: p.Version, Pid: p.Pid, Modules: p.Modules,
			})
		}
		for _, p := range fleet.Procs {
			rng.Shuffle(len(p.Batch.Samples), func(i, j int) {
				p.Batch.Samples[i], p.Batch.Samples[j] = p.Batch.Samples[j], p.Batch.Samples[i]
			})
			_ = st.PutBatch(p.Batch)
		}
		srv := httptest.NewServer(server.New(st))
		defer srv.Close()
		resp, err := http.Get(srv.URL + "/api/flame?program=" + st.Programs()[0] + "&version=v1.0")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(resp.Body); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	ref := serve(1)
	for seed := int64(2); seed <= 6; seed++ {
		got := serve(seed)
		if !bytes.Equal(got, ref) {
			t.Fatalf("API output differs under shuffled ingestion (seed %d)", seed)
		}
	}
}

func TestStatusEndpoint(t *testing.T) {
	fleet := gen.Generate(gen.Config{Programs: 2, ProcsPerProg: 2, SamplesPerVer: 50, Seed: 88})
	st := store.New()
	_ = fleet.Upload(st)
	srv := httptest.NewServer(server.New(st))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	var dto struct {
		TotalSamples int64 `json:"total_samples"`
		Processes    []struct {
			Program, Version string
			Pid, Samples     int64
			Labels           []string
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	wantProcs := 2 * 2 * 2 // programs * procs * versions
	if len(dto.Processes) != wantProcs {
		t.Fatalf("status processes = %d, want %d", len(dto.Processes), wantProcs)
	}
	var sum int64
	for _, p := range dto.Processes {
		sum += p.Samples
		if !sort.StringsAreSorted(p.Labels) {
			t.Fatalf("labels not sorted: %v", p.Labels)
		}
	}
	if sum != dto.TotalSamples {
		t.Fatalf("total samples %d != sum %d", dto.TotalSamples, sum)
	}
}
