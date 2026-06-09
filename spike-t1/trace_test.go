package trymod

import "testing"

// hostTrace logs every invoke + whether the threw-flag was set, to see which
// path actually drives the catch.
type hostTrace struct {
	host
	suppressSetThrew bool
}

func (h *hostTrace) Xinvoke_iii(index, a0, a1 int32) int32 {
	var ret int32
	threw := h.call(index, func() {
		f := h.table()[index].(func(int32, int32) int32)
		ret = f(a0, a1)
	})
	h.logf("invoke_iii(idx=%d) threw=%v ret=%d threwFlagBefore=%d", index, threw, ret, 0)
	return ret
}

func TestTrace(t *testing.T) {
	h := &hostTrace{}
	m := New(h)
	m.X_initialize()
	got := m.Xtry_it(3)
	for _, l := range h.log {
		t.Logf("  %s", l)
	}
	// read threw flag memory after
	t.Logf("try_it(3)=%d", got)
}
