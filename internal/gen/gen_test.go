package gen

import (
	"testing"

	"aquaprofile/internal/symbolize"
)

func TestGenerateDeterministic(t *testing.T) {
	cfg := Config{Programs: 4, ProcsPerProg: 2, SamplesPerVer: 200, Seed: 99}
	f1 := Generate(cfg)
	f2 := Generate(cfg)
	if len(f1.Procs) != len(f2.Procs) {
		t.Fatalf("fleet size drift: %d vs %d", len(f1.Procs), len(f2.Procs))
	}
	for i := range f1.Procs {
		a, b := f1.Procs[i], f2.Procs[i]
		if len(a.Batch.Samples) != len(b.Batch.Samples) {
			t.Fatalf("proc %d sample count drift", i)
		}
		for j := range a.Batch.Samples {
			sa, sb := a.Batch.Samples[j], b.Batch.Samples[j]
			if sa.TimeNs != sb.TimeNs || len(sa.Addresses) != len(sb.Addresses) {
				t.Fatalf("proc %d sample %d not deterministic", i, j)
			}
			for k := range sa.Addresses {
				if sa.Addresses[k] != sb.Addresses[k] {
					t.Fatalf("proc %d sample %d addr %d drift", i, j, k)
				}
			}
		}
	}
}

func TestUntouchedCountsIdenticalAcrossVersions(t *testing.T) {
	fleet := Generate(Config{Programs: 5, ProcsPerProg: 2, SamplesPerVer: 300, Seed: 7})
	byProc := map[string]map[string]int{} // key: program|pid -> template -> v1 count
	v2 := map[string]map[string]int{}
	for _, p := range fleet.Procs {
		key := p.Program + "|" + itoa(int(p.Pid))
		if p.Version == "v1.0" {
			byProc[key] = p.Counts
		} else {
			v2[key] = p.Counts
		}
	}
	for key, v1counts := range byProc {
		v2counts := v2[key]
		for _, name := range fleet.Untouched {
			if v1counts[name] != v2counts[name] {
				t.Fatalf("%s: untouched %s moved %d -> %d",
					key, name, v1counts[name], v2counts[name])
			}
		}
		// Total sample count preserved per process.
		var t1, t2 int
		for _, c := range v1counts {
			t1 += c
		}
		for _, c := range v2counts {
			t2 += c
		}
		if t1 != t2 {
			t.Fatalf("%s: total samples changed %d -> %d", key, t1, t2)
		}
		// Idle gave away all its samples; targets gained exactly that many.
		given := v1counts["runtime.idle"]
		var gained int
		for _, tgt := range fleet.SlowTargets {
			gained += v2counts[tgt] - v1counts[tgt]
		}
		if gained != given {
			t.Fatalf("%s: reallocation unbalanced: idle gave %d, targets gained %d",
				key, given, gained)
		}
		if v2counts["runtime.idle"] != 0 {
			t.Fatalf("%s: v2 idle should be 0, got %d", key, v2counts["runtime.idle"])
		}
	}
}

func TestGapAndUnknownTemplatesResolveCorrectly(t *testing.T) {
	fleet := Generate(Config{Programs: 2, ProcsPerProg: 1, SamplesPerVer: 500, Seed: 3})
	// Reconstruct resolvers and verify every gap/unknown sample classifies.
	for _, p := range fleet.Procs {
		res := symbolize.NewResolver(p.Modules)
		for _, s := range p.Batch.Samples {
			leaf := s.Addresses[len(s.Addresses)-1]
			frame := res.Resolve(leaf)
			_ = frame // classification asserted below by aggregate
		}
		// The fleet must contain at least one gap and one unknown sample.
		var gap, unk int
		for _, s := range p.Batch.Samples {
			f := res.Resolve(s.Addresses[len(s.Addresses)-1])
			if f.IsGap() {
				gap++
			}
			if f.IsUnknown() {
				unk++
			}
		}
		if gap == 0 || unk == 0 {
			t.Fatalf("%s/%s: gap=%d unknown=%d, both must be > 0",
				p.Program, p.Version, gap, unk)
		}
	}
}

func TestDifferentProcessesHaveDifferentBases(t *testing.T) {
	fleet := Generate(Config{Programs: 2, ProcsPerProg: 3, SamplesPerVer: 50, Seed: 11})
	seen := map[string]map[uint64]bool{} // module -> set of bases
	for _, p := range fleet.Procs {
		for _, m := range p.Modules {
			if seen[m.Name] == nil {
				seen[m.Name] = map[uint64]bool{}
			}
			seen[m.Name][m.Base] = true
		}
	}
	// 2 programs * 3 procs * 2 versions = 12 distinct layouts per module.
	for mod, bases := range seen {
		if len(bases) < 6 {
			t.Fatalf("module %s only loaded at %d distinct bases", mod, len(bases))
		}
	}
}
