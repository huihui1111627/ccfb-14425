package symbolize

import "testing"

func testModules() []Module {
	return []Module{
		{
			Name: "drivers.so", Base: 0x1000, Size: 0x1000,
			Symbols: []Symbol{
				{Name: "open", Addr: 0x1000, Size: 0x100},
				{Name: "read", Addr: 0x1200, Size: 0x80},
				// zero-size symbol: exact match only
				{Name: "trap", Addr: 0x1500, Size: 0},
			},
		},
		{
			// Overlaps the first module's span; higher base must win.
			Name: "control.bin", Base: 0x1800, Size: 0x400,
			Symbols: []Symbol{
				{Name: "loop", Addr: 0x1800, Size: 0x200},
			},
		},
	}
}

func TestResolveByModuleOffset(t *testing.T) {
	// Same content, different base: offset resolution must agree.
	r1 := NewResolver(testModules())
	shifted := []Module{
		{
			Name: "drivers.so", Base: 0x7f00_0000, Size: 0x1000,
			Symbols: []Symbol{
				{Name: "open", Addr: 0x7f00_0000, Size: 0x100},
				{Name: "read", Addr: 0x7f00_0200, Size: 0x80},
				{Name: "trap", Addr: 0x7f00_0500, Size: 0},
			},
		},
		{
			Name: "control.bin", Base: 0x7f00_0800, Size: 0x400,
			Symbols: []Symbol{{Name: "loop", Addr: 0x7f00_0800, Size: 0x200}},
		},
	}
	r2 := NewResolver(shifted)

	cases := []struct {
		name string
		a, b uint64
		want Frame
	}{
		{"inside open", 0x1010, 0x7f00_0010, Frame{"drivers.so", "open"}},
		{"inside read", 0x127f, 0x7f00_027f, Frame{"drivers.so", "read"}},
		{"zero-size exact", 0x1500, 0x7f00_0500, Frame{"drivers.so", "trap"}},
		{"zero-size neighbor is a gap", 0x1504, 0x7f00_0504, GapFrame("drivers.so")},
		{"hole between symbols", 0x1400, 0x7f00_0400, GapFrame("drivers.so")},
		{"trailing module hole", 0x1f00, 0x7f00_0f00, GapFrame("drivers.so")},
		{"overlap picks higher base", 0x1900, 0x7f00_0900, Frame{"control.bin", "loop"}},
		{"no module", 0xdead, 0xdead, UnknownFrame(0xdead)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got1 := r1.Resolve(tc.a)
			got2 := r2.Resolve(tc.b)
			if got1 != tc.want {
				t.Fatalf("r1.Resolve(%#x) = %v, want %v", tc.a, got1, tc.want)
			}
			// Unknown frames embed the address, so compare func/module separately.
			if got1.Module != got2.Module || (tc.want != UnknownFrame(tc.a) && got1 != got2) {
				t.Fatalf("offset mismatch across load bases: %v vs %v", got1, got2)
			}
		})
	}
}

func TestGapAndUnknownClassifiers(t *testing.T) {
	r := NewResolver(testModules())
	if f := r.Resolve(0x1300); !f.IsGap() || f.Module != "drivers.so" {
		t.Fatalf("want gap frame in drivers.so, got %v", f)
	}
	if f := r.Resolve(0x9999); !f.IsUnknown() {
		t.Fatalf("want unknown frame, got %v", f)
	}
	if f := FoldedFrame(); !f.IsFolded() {
		t.Fatalf("folded classifier broken: %v", f)
	}
}

func TestResolverDeterministicWithShuffledInputs(t *testing.T) {
	mods := testModules()
	r1 := NewResolver(mods)
	r2 := NewResolver([]Module{mods[1], mods[0]})
	addrs := []uint64{0x1010, 0x127f, 0x1400, 0x1500, 0x1900, 0x1f00, 0xdead}
	for _, a := range addrs {
		if r1.Resolve(a) != r2.Resolve(a) {
			t.Fatalf("resolution of %#x depends on module input order", a)
		}
	}
}

func TestSymbolOutsideModuleIsIgnored(t *testing.T) {
	r := NewResolver([]Module{{
		Name: "x.so", Base: 0x100, Size: 0x10,
		Symbols: []Symbol{
			{Name: "in", Addr: 0x100, Size: 0x8},
			{Name: "out", Addr: 0x9000, Size: 0x10}, // dropped
		},
	}})
	if got := r.Resolve(0x104); got.Func != "in" {
		t.Fatalf("valid symbol lost: %v", got)
	}
	if got := r.Resolve(0x10c); !got.IsGap() {
		t.Fatalf("out-of-span symbol must not cover module hole: %v", got)
	}
}
