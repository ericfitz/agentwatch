package wrapper

import "github.com/ericfitz/agentwatch/internal/wire"

type source uint8

const (
	sourceLocal source = iota + 1
	sourceRemote
)

var headlessSize = wire.TermSize{Cols: 80, Rows: 24}

// sizeArbiter decides the PTY size. The source that most recently typed or
// resized wins, which is tmux's "latest" window-size policy. It is pure; the
// session loop applies its answers.
type sizeArbiter struct {
	local, remote wire.TermSize // zero means unknown
	active        source
	applied       wire.TermSize
}

func newSizeArbiter(local wire.TermSize) sizeArbiter {
	a := sizeArbiter{local: local, active: sourceLocal, applied: local}
	if local == (wire.TermSize{}) {
		a.applied = headlessSize
	}
	return a
}

// observe records activity from src, with src's new size when it has one, and
// returns the size the PTY should have and whether that differs from applied.
func (a *sizeArbiter) observe(src source, size *wire.TermSize) (wire.TermSize, bool) {
	if size != nil && src == sourceLocal {
		a.local = *size
	}
	if size != nil && src == sourceRemote {
		a.remote = *size
	}
	a.active = src
	return a.settle()
}

// remoteLost forgets the remote size when the relay link drops.
func (a *sizeArbiter) remoteLost() (wire.TermSize, bool) {
	a.remote = wire.TermSize{}
	a.active = sourceLocal
	return a.settle()
}

func (a *sizeArbiter) settle() (wire.TermSize, bool) {
	want, fallback := a.local, a.remote
	if a.active == sourceRemote {
		want, fallback = a.remote, a.local
	}
	if want == (wire.TermSize{}) {
		want = fallback
	}
	if want == (wire.TermSize{}) {
		want = a.applied
	}
	changed := want != a.applied
	a.applied = want
	return want, changed
}
