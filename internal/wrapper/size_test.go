package wrapper

import (
	"testing"

	"github.com/ericfitz/agentwatch/internal/wire"
)

func ts(cols, rows uint16) *wire.TermSize { return &wire.TermSize{Cols: cols, Rows: rows} }

func TestSizeArbiter(t *testing.T) {
	type step struct {
		name    string
		apply   func(*sizeArbiter) (wire.TermSize, bool)
		want    wire.TermSize
		changed bool
	}
	for _, tc := range []struct {
		name  string
		local wire.TermSize
		steps []step
	}{
		{"latest active source wins", *ts(120, 40), []step{
			{"viewer resizes", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, ts(100, 30)) }, *ts(100, 30), true},
			{"viewer types", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, nil) }, *ts(100, 30), false},
			{"local user types", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceLocal, nil) }, *ts(120, 40), true},
			{"viewer types again", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, nil) }, *ts(100, 30), true},
			{"local window grows", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceLocal, ts(150, 50)) }, *ts(150, 50), true},
		}},
		{"a link drop hands the PTY back to local", *ts(120, 40), []step{
			{"viewer resizes", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, ts(100, 30)) }, *ts(100, 30), true},
			{"link drops", (*sizeArbiter).remoteLost, *ts(120, 40), true},
			{"viewer types after reconnect, size unknown", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, nil) }, *ts(120, 40), false},
		}},
		{"headless starts at 80x24 and keeps the last remote size", wire.TermSize{}, []step{
			{"viewer types first", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, nil) }, *ts(80, 24), false},
			{"viewer resizes", func(a *sizeArbiter) (wire.TermSize, bool) { return a.observe(sourceRemote, ts(100, 30)) }, *ts(100, 30), true},
			{"link drops", (*sizeArbiter).remoteLost, *ts(100, 30), false},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newSizeArbiter(tc.local)
			if tc.local == (wire.TermSize{}) && a.applied != *ts(80, 24) {
				t.Fatalf("headless initial size %v, want 80x24", a.applied)
			}
			for _, st := range tc.steps {
				got, changed := st.apply(&a)
				if got != st.want || changed != st.changed {
					t.Fatalf("%s: got %v changed=%v, want %v changed=%v", st.name, got, changed, st.want, st.changed)
				}
			}
		})
	}
}
