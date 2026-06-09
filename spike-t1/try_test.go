package trymod

import "testing"

// TestCatchFires is THE key test: drive the wasm2go-translated C++ module
// through a real throw and a real non-throw, with the Emscripten exception ABI
// implemented in Go (host.go). The C++ try_it returns 1 if the catch fired,
// 0 if no exception was thrown.
func TestCatchFires(t *testing.T) {
	cases := []struct {
		name string
		in   int32
		want int32
	}{
		{"odd_throws_catch_fires", 3, 1},
		{"even_no_throw", 4, 0},
		{"odd_throws_again", 7, 1},
		{"zero_no_throw", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &host{}
			m := New(h) // New calls h.Init(m) via the hook
			m.X_initialize()

			got := m.Xtry_it(tc.in)

			for _, l := range h.log {
				t.Logf("  abi: %s", l)
			}
			if got != tc.want {
				t.Fatalf("try_it(%d) = %d, want %d", tc.in, got, tc.want)
			}
			t.Logf("try_it(%d) = %d (OK)", tc.in, got)
		})
	}
}
