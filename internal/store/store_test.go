package store

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"

	"aquaprofile/internal/merge"
	"aquaprofile/internal/symbolize"
)

func sample(t uint64, addrs ...uint64) Sample {
	return Sample{TimeNs: t, Addresses: addrs, Labels: map[string]string{"tank": "T01"}}
}

func testModules() []symbolize.Module {
	return []symbolize.Module{{
		Name: "m.bin", Base: 0x1000, Size: 0x1000,
		Symbols: []symbolize.Symbol{
			{Name: "f1", Addr: 0x1000, Size: 0x100},
			{Name: "f2", Addr: 0x1200, Size: 0x100},
		},
	}}
}

func TestQueryResolvesAndFilters(t *testing.T) {
	st := New()
	if err := st.PutModules(ModuleUpload{
		Program: "p", Version: "v1", Pid: 7, Modules: testModules(),
	}); err != nil {
		t.Fatal(err)
	}
	batch := Batch{
		Program: "p", Version: "v1", Pid: 7, PeriodNs: 500,
		Samples: []Sample{
			{TimeNs: 100, Addresses: []uint64{0x1010, 0x1210},
				Labels: map[string]string{"tank": "T01", "pump": "2"}},
			{TimeNs: 200, Addresses: []uint64{0x1020},
				Labels: map[string]string{"tank": "T02", "pump": "1"}},
			{TimeNs: 300, Addresses: []uint64{0x1900}, // gap
				Labels: map[string]string{"tank": "T01"}},
		},
	}
	if err := st.PutBatch(batch); err != nil {
		t.Fatal(err)
	}

	all := st.Query("p", "v1", Filter{})
	if len(all) != 3 {
		t.Fatalf("query returned %d samples", len(all))
	}
	if all[0].Weight != 500 {
		t.Fatalf("weight not taken from period: %d", all[0].Weight)
	}
	if all[0].Frames[0] != (symbolize.Frame{Module: "m.bin", Func: "f1"}) {
		t.Fatalf("resolution wrong: %v", all[0].Frames)
	}
	if !all[2].Frames[0].IsGap() {
		t.Fatalf("hole address not classified as gap: %v", all[2].Frames)
	}

	tank1 := st.Query("p", "v1", Filter{Labels: map[string]string{"tank": "T01"}})
	if len(tank1) != 2 {
		t.Fatalf("label filter returned %d, want 2", len(tank1))
	}
	both := st.Query("p", "v1", Filter{Labels: map[string]string{"tank": "T01", "pump": "2"}})
	if len(both) != 1 {
		t.Fatalf("multi-label filter returned %d, want 1", len(both))
	}
	ranged := st.Query("p", "v1", Filter{StartNs: 150, EndNs: 300})
	if len(ranged) != 1 || ranged[0].Frames[0].Func != "f1" {
		t.Fatalf("time range filter wrong: %+v", ranged)
	}
}

func TestQueryOrderIndependence(t *testing.T) {
	// Two batches with interleaved timestamps; query output must be
	// identical regardless of which batch landed first.
	mkBatch := func(pid int64, times ...uint64) Batch {
		b := Batch{Program: "p", Version: "v1", Pid: pid, PeriodNs: 10}
		for _, tm := range times {
			b.Samples = append(b.Samples, sample(tm, 0x1010))
		}
		return b
	}
	mods := ModuleUpload{Program: "p", Version: "v1", Pid: 1, Modules: testModules()}
	mods2 := ModuleUpload{Program: "p", Version: "v1", Pid: 2, Modules: testModules()}

	build := func(first, second Batch) []merge.Sample {
		st := New()
		_ = st.PutModules(mods)
		_ = st.PutModules(mods2)
		_ = st.PutBatch(first)
		_ = st.PutBatch(second)
		return st.Query("p", "v1", Filter{})
	}
	b1 := mkBatch(1, 1, 3, 5, 7)
	b2 := mkBatch(2, 2, 4, 6, 8)
	a := build(b1, b2)
	b := build(b2, b1)
	// Query groups by process then ingestion order; merge itself is what
	// must be order-independent, so compare the returned sample multisets.
	if !reflect.DeepEqual(sampleMultiset(a), sampleMultiset(b)) {
		t.Fatalf("query multiset differs by ingestion order")
	}
}

func sampleMultiset(xs []merge.Sample) map[string]int {
	out := map[string]int{}
	for _, s := range xs {
		key := fmt.Sprintf("w=%d|frames=%v", s.Weight, s.Frames)
		out[key]++
	}
	return out
}

func TestIngestionShuffleMergeEquivalence(t *testing.T) {
	// The real requirement: feeding the same sample multiset in shuffled
	// ingestion order must produce equal merged weights. Recreate the
	// batches many times with randomized order and compare per-frame totals.
	totalsAt := func(seed int64) map[string]int64 {
		st := New()
		_ = st.PutModules(ModuleUpload{
			Program: "p", Version: "v1", Pid: 1, Modules: testModules(),
		})
		samples := []Sample{
			{TimeNs: 1, Addresses: []uint64{0x1010, 0x1210}, Labels: map[string]string{"tank": "T01"}},
			{TimeNs: 2, Addresses: []uint64{0x1020}, Labels: map[string]string{"tank": "T01"}},
			{TimeNs: 3, Addresses: []uint64{0x1030, 0x1900}, Labels: map[string]string{"tank": "T01"}},
			{TimeNs: 4, Addresses: []uint64{0x1210}, Labels: map[string]string{"tank": "T01"}},
		}
		rng := rand.New(rand.NewSource(seed))
		rng.Shuffle(len(samples), func(i, j int) { samples[i], samples[j] = samples[j], samples[i] })
		_ = st.PutBatch(Batch{Program: "p", Version: "v1", Pid: 1, PeriodNs: 7, Samples: samples})
		got := st.Query("p", "v1", Filter{})
		totals := map[string]int64{}
		for _, s := range got {
			for _, f := range s.Frames {
				totals[f.String()] += s.Weight
			}
		}
		return totals
	}
	ref := totalsAt(1)
	for seed := int64(2); seed < 20; seed++ {
		if !reflect.DeepEqual(totalsAt(seed), ref) {
			t.Fatalf("merged totals depend on ingestion order at seed %d", seed)
		}
	}
}

func TestValidation(t *testing.T) {
	st := New()
	if err := st.PutBatch(Batch{Program: "", Version: "v1", PeriodNs: 1}); err == nil {
		t.Fatal("expected error for missing program")
	}
	if err := st.PutBatch(Batch{Program: "p", Version: "v1", PeriodNs: 0}); err == nil {
		t.Fatal("expected error for non-positive period")
	}
	if err := st.PutModules(ModuleUpload{Program: "p"}); err == nil {
		t.Fatal("expected error for missing version")
	}
}

func TestStatusAndCatalog(t *testing.T) {
	st := New()
	_ = st.PutModules(ModuleUpload{Program: "p2", Version: "v1", Pid: 1, Modules: testModules()})
	_ = st.PutBatch(Batch{Program: "p2", Version: "v1", Pid: 1, PeriodNs: 10,
		Samples: []Sample{sample(5, 0x1010)}})
	_ = st.PutBatch(Batch{Program: "p1", Version: "v9", Pid: 2, PeriodNs: 20,
		Samples: []Sample{sample(9, 0x1010), sample(3, 0x1010)}})

	progs := st.Programs()
	wantProgs := []string{"p1", "p2"}
	if !reflect.DeepEqual(progs, wantProgs) {
		t.Fatalf("programs = %v, want %v", progs, wantProgs)
	}
	if vs := st.Versions("p1"); len(vs) != 1 || vs[0] != "v9" {
		t.Fatalf("versions = %v", vs)
	}
	infos := st.Status()
	if len(infos) != 2 || infos[0].Program != "p1" {
		t.Fatalf("status order wrong: %+v", infos)
	}
	if infos[0].Samples != 2 || infos[0].FirstNs != 3 || infos[0].LastNs != 9 {
		t.Fatalf("status aggregates wrong: %+v", infos[0])
	}
}

func TestLateSamplesWithoutModulesResolveUnknown(t *testing.T) {
	st := New()
	_ = st.PutBatch(Batch{Program: "p", Version: "v1", Pid: 1, PeriodNs: 3,
		Samples: []Sample{sample(1, 0x5000)}})
	got := st.Query("p", "v1", Filter{})
	if len(got) != 1 || !got[0].Frames[0].IsUnknown() {
		t.Fatalf("missing module table should resolve unknown: %+v", got)
	}
}
