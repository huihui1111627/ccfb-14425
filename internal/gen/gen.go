// Package gen produces deterministic synthetic control-program workloads:
// dozens of programs, multiple processes per program with different sampling
// periods, symbol tables with deliberate holes, and two versions where a
// chosen set of functions is made slower by stealing idle samples.
//
// "Slower" here means the new version spends a larger share of its samples
// in the target functions while every untouched template keeps exactly the
// same number of samples — so the diff engine must rank the targets first
// and never mark an untouched function.
package gen

import (
	"fmt"
	"math/rand"
	"sort"

	"aquaprofile/internal/store"
	"aquaprofile/internal/symbolize"
)

// Config controls a synthetic fleet.
type Config struct {
	Programs      int // number of control programs
	ProcsPerProg  int // processes per program (different base addresses)
	SamplesPerVer int // samples per process per version
	Seed          int64
}

// DefaultConfig returns the fleet size used by the demo and the tests.
func DefaultConfig() Config {
	return Config{Programs: 30, ProcsPerProg: 3, SamplesPerVer: 400, Seed: 20261121}
}

// Module names shared by every program.
const (
	ModControl = "control.bin"
	ModDrivers = "drivers.so"
	ModRuntime = "libruntime.so"
)

// Function inventory per module. Symbol sizes are non-overlapping and the
// module span deliberately extends past the last symbol, creating a hole.
var functionInventory = map[string][]string{
	ModControl: {
		"ctrl.main_loop", "ctrl.schedule_tasks", "ctrl.pump_control",
		"ctrl.filter_cycle", "ctrl.heater_pid", "ctrl.feeder_dispense", "ctrl.sensor_poll",
	},
	ModDrivers: {
		"drv.pump_write", "drv.heater_write", "drv.feeder_step",
		"drv.adc_read", "drv.gpio_set", "drv.can_send",
	},
	ModRuntime: {
		"runtime.idle", "runtime.lock", "runtime.log_emit", "runtime.mem_alloc",
	},
}

// Function names that become slower in the new version.
var plantedSlow = []string{"ctrl.pump_control", "ctrl.heater_pid", "drv.can_send"}

// Template is one synthetic call-stack shape. Addrs are filled per process
// once the base addresses are known.
type Template struct {
	Name     string   // leaf function name (unique per template)
	Module   string   // module of the leaf
	Chain    []string // module!function pairs, root first, leaf last
	LeafSize uint64   // symbol size of the leaf (for address picking)
}

// buildTemplates defines the stack topology. Every leaf also appears on its
// chain, and runtime.idle is the work the new version gives away.
func buildTemplates() []Template {
	mk := func(mod, fn string) string { return mod + "!" + fn }
	tpls := []Template{
		{Name: "ctrl.sensor_poll", Module: ModControl, LeafSize: 0x120, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModControl, "ctrl.sensor_poll"), mk(ModDrivers, "drv.adc_read")}},
		{Name: "ctrl.pump_control", Module: ModControl, LeafSize: 0x200, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModControl, "ctrl.pump_control"), mk(ModDrivers, "drv.pump_write"),
			mk(ModDrivers, "drv.gpio_set")}},
		{Name: "ctrl.filter_cycle", Module: ModControl, LeafSize: 0x180, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModControl, "ctrl.filter_cycle"), mk(ModDrivers, "drv.pump_write")}},
		{Name: "ctrl.heater_pid", Module: ModControl, LeafSize: 0x1c0, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModControl, "ctrl.heater_pid"), mk(ModDrivers, "drv.heater_write")}},
		{Name: "ctrl.feeder_dispense", Module: ModControl, LeafSize: 0x160, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModControl, "ctrl.feeder_dispense"), mk(ModDrivers, "drv.feeder_step")}},
		{Name: "drv.can_send", Module: ModDrivers, LeafSize: 0x140, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModControl, "ctrl.schedule_tasks"),
			mk(ModDrivers, "drv.can_send")}},
		{Name: "runtime.lock", Module: ModRuntime, LeafSize: 0x90, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModRuntime, "runtime.lock"),
			mk(ModRuntime, "runtime.mem_alloc")}},
		{Name: "runtime.log_emit", Module: ModRuntime, LeafSize: 0xb0, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModRuntime, "runtime.log_emit")}},
		// Recursive stack: the same function appears twice; the merge
		// engine must count its cumulative time only once.
		{Name: "runtime.mem_alloc", Module: ModRuntime, LeafSize: 0xa0, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModRuntime, "runtime.mem_alloc"),
			mk(ModRuntime, "runtime.mem_alloc")}},
		// The idle template: present in both versions, fewer samples in v2.
		{Name: "runtime.idle", Module: ModRuntime, LeafSize: 0x80, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModRuntime, "runtime.idle")}},
		// A stack whose leaf address lands in a module symbol-table hole.
		{Name: symbolize.GapFunc, Module: ModDrivers, LeafSize: 0, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(ModDrivers, symbolize.GapFunc)}},
		// A stack whose leaf address belongs to no module at all. The
		// concrete frame name (hex address) is process-specific and gets
		// substituted when the layout is built.
		{Name: "unknown", Module: symbolize.UnknownModule, LeafSize: 0, Chain: []string{
			mk(ModControl, "ctrl.main_loop"), mk(symbolize.UnknownModule, unknownPlaceholder)}},
	}
	return tpls
}

// templateWeights gives v1 sampling probabilities; they sum to 1.0 in
// permille units to stay integer.
var templatePermille = map[string]int{
	"ctrl.sensor_poll":     170,
	"ctrl.pump_control":    130,
	"ctrl.filter_cycle":    120,
	"ctrl.heater_pid":      100,
	"ctrl.feeder_dispense": 90,
	"drv.can_send":         70,
	"runtime.lock":         80,
	"runtime.log_emit":     60,
	"runtime.mem_alloc":    50,
	"runtime.idle":         100,
	symbolize.GapFunc:      20,
	"unknown":              10,
}

// slowdownPermille is how many of the idle samples each planted function
// absorbs in the new version.
var slowdownPermille = map[string]int{
	"ctrl.pump_control": 40,
	"ctrl.heater_pid":   35,
	"drv.can_send":      25,
}

const (
	versionOld = "v1.0"
	versionNew = "v2.0"
)

// PlantedSlow is the sorted list of functions expected to lead the diff.
func PlantedSlow() []string {
	out := append([]string(nil), plantedSlow...)
	sort.Strings(out)
	return out
}

// Versions returns the two generated version labels.
func Versions() (string, string) { return versionOld, versionNew }

// unknownPlaceholder marks the chain slot that becomes the process-specific
// unknown-address frame once a layout exists.
const unknownPlaceholder = "__unknown__"

// Proc is one generated process with everything needed to upload it.
type Proc struct {
	Program string
	Version string
	Pid     int64
	Period  int64
	Modules []symbolize.Module
	Batch   store.Batch
	// Expected counts per template leaf name after generation.
	Counts map[string]int
}

// Fleet is the full generated dataset plus expectations.
type Fleet struct {
	Procs []Proc
	// SlowTargets are the functions that must lead every diff.
	SlowTargets []string
	// Untouched are template leaf names whose sample counts are identical
	// between versions (per process) and must never be flagged.
	Untouched []string
	Templates []Template
}

type procLayout struct {
	mods    []symbolize.Module
	symAddr map[string]uint64 // "module!function" -> in-process address
	gapAddr map[string]uint64 // module -> hole address
	unknown uint64
}

// periods rotate so different processes sample at different frequencies;
// merging must weight by these, not by raw sample counts.
var periods = []int64{5_000_000, 10_000_000, 20_000_000}

// Generate builds the whole fleet.
func Generate(cfg Config) *Fleet {
	if cfg.Programs <= 0 {
		cfg = DefaultConfig()
	}
	tpls := buildTemplates()
	rng := rand.New(rand.NewSource(cfg.Seed))

	fleet := &Fleet{SlowTargets: PlantedSlow(), Templates: tpls}
	for _, t := range tpls {
		if _, planted := slowdownPermille[t.Name]; !planted && t.Name != "runtime.idle" &&
			t.Name != symbolize.GapFunc && t.Name != "unknown" {
			fleet.Untouched = append(fleet.Untouched, t.Name)
		}
	}
	sort.Strings(fleet.Untouched)

	for p := 0; p < cfg.Programs; p++ {
		program := programName(p)
		for procIdx := 0; procIdx < cfg.ProcsPerProg; procIdx++ {
			pid := int64(1000 + p*10 + procIdx)
			period := periods[procIdx%len(periods)]

			// One RNG stream per process: v1 counts are random draws, v2
			// counts are derived from v1 so untouched templates keep
			// exactly the same sample count in both versions.
			procRng := rand.New(rand.NewSource(cfg.Seed + int64(p)*1000 + int64(procIdx)))
			v1Counts := make([]int, len(tpls))
			weightedDraws(procRng, cfg.SamplesPerVer, tpls, v1Permille(tpls), v1Counts)
			v2Counts := deriveV2(tpls, v1Counts)

			for vi, version := range []string{versionOld, versionNew} {
				counts := v1Counts
				if vi == 1 {
					counts = v2Counts
				}
				// Different base addresses per process AND per version:
				// resolution by module offset must still line up.
				layout := buildLayout(rng, int64(p), int64(procIdx), int64(vi))
				batch := store.Batch{
					Program: program, Version: version, Pid: pid,
					PeriodNs: period, Samples: make([]store.Sample, 0, cfg.SamplesPerVer),
				}
				expected := make(map[string]int, len(tpls))
				baseTime := uint64(1_700_000_000_000_000_000)
				bucketSpan := uint64(1_000_000) // 1 ms between time buckets
				sIdx := 0
				for ti, t := range tpls {
					expected[t.Name] += counts[ti]
					for k := 0; k < counts[ti]; k++ {
						addrs := make([]uint64, len(t.Chain))
						for ci, frame := range t.Chain {
							addrs[ci] = layout.addressOf(frame, t, ci == len(t.Chain)-1, rng)
						}
						batch.Samples = append(batch.Samples, store.Sample{
							TimeNs:    baseTime + uint64(sIdx%cfg.SamplesPerVer)*bucketSpan,
							Addresses: addrs,
							Labels:    sampleLabels(rng, p),
						})
						sIdx++
					}
				}
				fleet.Procs = append(fleet.Procs, Proc{
					Program: program, Version: version, Pid: pid, Period: period,
					Modules: layout.mods, Batch: batch, Counts: expected,
				})
			}
		}
	}
	return fleet
}

func programName(p int) string {
	names := []string{"pump-rack", "filter-bank", "thermal-loop", "feeder-line", "sensor-hub"}
	return fmt.Sprintf("%s-%c-%02d", names[p%len(names)], rune('A'+p/len(names)), p)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// buildLayout assigns each module a distinct base (different for every
// process/version), packs symbols, and leaves a trailing hole.
func buildLayout(rng *rand.Rand, progIdx, procIdx, verIdx int64) procLayout {
	lay := procLayout{
		symAddr: make(map[string]uint64),
		gapAddr: make(map[string]uint64),
	}
	// Bases are spread far apart and jittered by process/version so the
	// same module never sits at the same address twice.
	bases := []uint64{
		0x400000 + uint64(progIdx)*0x1_0000_0000 + uint64(procIdx)*0x80_0000 + uint64(verIdx)*0x40_0000,
		0x7f00_0000_0000 + uint64(progIdx)*0x10_0000_0000 + uint64(procIdx)*0x200_0000 + uint64(verIdx)*0x100_0000,
		0x7f80_0000_0000 + uint64(progIdx)*0x8_0000_0000 + uint64(procIdx)*0x100_0000 + uint64(verIdx)*0x80_0000,
	}
	modNames := []string{ModControl, ModDrivers, ModRuntime}
	for mi, modName := range modNames {
		base := bases[mi]
		mod := symbolize.Module{Name: modName, Base: base}
		off := uint64(0x1000)
		for _, fn := range functionInventory[modName] {
			size := symbolSize(modName, fn)
			mod.Symbols = append(mod.Symbols, symbolize.Symbol{
				Name: fn, Addr: base + off, Size: size,
			})
			lay.symAddr[modName+"!"+fn] = base + off
			off += size + 0x40 // small inter-symbol gaps exist too
		}
		// Trailing hole inside the module span but past every symbol.
		mod.Size = off + 0x400
		lay.gapAddr[modName] = base + off + 0x100
		lay.mods = append(lay.mods, mod)
	}
	// Address outside every module span.
	lay.unknown = 0x0000_dead_0000 + uint64(procIdx)*0x1000
	return lay
}

func symbolSize(mod, fn string) uint64 {
	for _, t := range buildTemplates() {
		if t.Module == mod && t.Name == fn {
			return t.LeafSize
		}
	}
	return 0x100
}

func (l procLayout) addressOf(frame string, t Template, isLeaf bool, rng *rand.Rand) uint64 {
	// Split "module!function".
	si := -1
	for i := 0; i < len(frame); i++ {
		if frame[i] == '!' {
			si = i
			break
		}
	}
	mod, fn := frame[:si], frame[si+1:]
	if fn == symbolize.GapFunc {
		return l.gapAddr[mod]
	}
	if mod == symbolize.UnknownModule {
		return l.unknown
	}
	addr := l.symAddr[frame]
	if isLeaf {
		// Pick an offset strictly inside the symbol so size-0 and sized
		// symbols both resolve correctly.
		if t.LeafSize > 1 {
			addr += uint64(rng.Int63n(int64(t.LeafSize / 2)))
		}
	}
	return addr
}

// weightedDraws fills counts with exactly n draws from integer permille
// weights (the wheel sums to 1000).
func weightedDraws(rng *rand.Rand, n int, tpls []Template, weights []int, counts []int) {
	total := 0
	for _, w := range weights {
		total += w
	}
	for k := 0; k < n; k++ {
		roll := rng.Intn(total)
		acc := 0
		for i, w := range weights {
			acc += w
			if roll < acc {
				counts[i]++
				break
			}
		}
	}
}

func v1Permille(tpls []Template) []int {
	w := make([]int, len(tpls))
	for i, t := range tpls {
		w[i] = templatePermille[t.Name]
	}
	return w
}

// deriveV2 builds the new-version counts deterministically from v1: idle
// samples are reallocated to the planted slow functions, every other
// template keeps its exact v1 count, and the total sample count is
// preserved. Reallocation is integer so no rounding drift reaches
// untouched functions.
func deriveV2(tpls []Template, v1 []int) []int {
	v2 := append([]int(nil), v1...)
	idleIdx := -1
	type target struct{ idx, share int }
	var targets []target
	shareSum := 0
	for i, t := range tpls {
		if t.Name == "runtime.idle" {
			idleIdx = i
		}
		if share, ok := slowdownPermille[t.Name]; ok {
			targets = append(targets, target{i, share})
			shareSum += share
		}
	}
	budget := v1[idleIdx]
	assigned := 0
	for k, tg := range targets {
		give := budget * tg.share / shareSum
		if k == len(targets)-1 {
			give = budget - assigned // last target absorbs the remainder
		}
		v2[tg.idx] += give
		assigned += give
	}
	v2[idleIdx] -= assigned // == v1 idle minus the full budget → 0
	return v2
}

func sampleLabels(rng *rand.Rand, progIdx int) map[string]string {
	tanks := []string{"T01", "T02", "T03", "T04", "T05", "T06", "T07", "T08"}
	gears := []string{"0", "1", "2", "3"}
	return map[string]string{
		"tank": tanks[rng.Intn(len(tanks))],
		"pump": gears[rng.Intn(len(gears))],
		"rack": itoa(progIdx % 5),
	}
}

// Upload pushes the whole fleet into a store.
func (f *Fleet) Upload(st interface {
	PutModules(store.ModuleUpload) error
	PutBatch(store.Batch) error
}) error {
	// Modules first per process, then samples — but shuffle nothing: the
	// order-independence tests do their own shuffling at the store level.
	for _, p := range f.Procs {
		if err := st.PutModules(store.ModuleUpload{
			Program: p.Program, Version: p.Version, Pid: p.Pid, Modules: p.Modules,
		}); err != nil {
			return err
		}
	}
	for _, p := range f.Procs {
		if err := st.PutBatch(p.Batch); err != nil {
			return err
		}
	}
	return nil
}
