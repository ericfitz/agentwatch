// Package inbox receives browser uploads into one directory. A file becomes
// visible under a real name only after its SHA-256 matches the offer.
package inbox

import (
	"crypto/sha256"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"time"

	"github.com/ericfitz/agentwatch/internal/wire"
)

type Config struct {
	Dir      string // absolute; "" means every Offer fails with FailDisabled
	MaxBytes int64  // > 0 when Dir is set
}

// Saved is a verified file under its final name. Handle returns one the first
// time a transfer reaches done, and never again for that id.
type Saved struct {
	Path, Name string
	Size       int64
	SHA256     wire.Digest
}

// Inbox is owned by the wrapper's session loop and is not safe for concurrent use.
type Inbox struct {
	dir      string
	maxBytes int64
	active   map[wire.TransferID]*transfer
	done     map[wire.TransferID]wire.TransferStatus
}

type transfer struct {
	offer    wire.Offer // Name already replaced by safeName(Name)
	tmp      *os.File   // <dir>/.agentwatch-<id>.part, O_CREATE|O_EXCL, 0600
	hash     hash.Hash  // sha256 over every byte written, in order
	received int64
	touched  time.Time
}

const (
	maxActive   = 4
	idleTimeout = 30 * time.Second
	staleAfter  = 10 * time.Minute
)

// Open validates cfg and, if Dir exists, removes .agentwatch-*.part files whose
// mtime is older than staleAfter. A live transfer touches its file at least
// every idleTimeout, so an old mtime is proof that the writer is gone, even
// when another wrapper shares the directory or a previous wrapper crashed
// mid-transfer.
func Open(cfg Config) (*Inbox, error) {
	if cfg.Dir != "" {
		if !filepath.IsAbs(cfg.Dir) {
			return nil, fmt.Errorf("inbox: %q is not an absolute path", cfg.Dir)
		}
		if cfg.MaxBytes <= 0 {
			return nil, fmt.Errorf("inbox: max file size %d is not positive", cfg.MaxBytes)
		}
		if err := sweepStale(cfg.Dir, time.Now()); err != nil {
			return nil, fmt.Errorf("inbox: %w", err)
		}
	}
	return &Inbox{
		dir:      cfg.Dir,
		maxBytes: cfg.MaxBytes,
		active:   map[wire.TransferID]*transfer{},
		done:     map[wire.TransferID]wire.TransferStatus{},
	}, nil
}

// Info is what the wrapper advertises in Hello.
func (in *Inbox) Info() wire.InboxInfo {
	return wire.InboxInfo{Enabled: in.dir != "", MaxBytes: in.maxBytes}
}

// Handle applies one transfer message and returns the status to publish, plus
// the saved file the first time a transfer reaches done.
func (in *Inbox) Handle(m wire.TransferMessage, now time.Time) (wire.TransferStatus, *Saved) {
	switch m := m.(type) {
	case wire.Offer:
		return in.offer(m, now)
	case wire.Chunk:
		return in.chunk(m, now)
	case wire.Cancel:
		t := in.active[m.ID]
		if t == nil {
			return failed(wire.TransferStatus{ID: m.ID}, wire.FailCancelled), nil
		}
		return in.abort(t, wire.FailCancelled), nil
	}
	panic(fmt.Sprintf("inbox: %T is not a transfer message", m))
}

func (in *Inbox) offer(o wire.Offer, now time.Time) (wire.TransferStatus, *Saved) {
	o.Name = safeName(o.Name)
	t := &transfer{offer: o, hash: sha256.New(), touched: now}
	if prev, ok := in.done[o.ID]; ok {
		if *prev.SHA256 == o.SHA256 {
			return prev, nil
		}
		return failed(t.status(), wire.FailProtocol), nil
	}
	if reason, ok := in.refuse(o); ok {
		return failed(t.status(), reason), nil
	}
	if err := os.MkdirAll(in.dir, 0o700); err != nil {
		return failed(t.status(), wire.FailIO), nil
	}
	tmp, err := os.OpenFile(in.partPath(o.ID), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return failed(t.status(), wire.FailIO), nil
	}
	t.tmp = tmp
	in.active[o.ID] = t
	if o.Size == 0 {
		return in.finish(t)
	}
	return t.status(), nil
}

// refuse checks an Offer against the inbox's limits. Permanent refusals come
// before busy, so a sender never retries an Offer that can never succeed.
func (in *Inbox) refuse(o wire.Offer) (wire.FailReason, bool) {
	switch {
	case in.dir == "":
		return wire.FailDisabled, true
	case in.active[o.ID] != nil:
		return wire.FailProtocol, true
	case o.Size > in.maxBytes:
		return wire.FailTooLarge, true
	case len(in.active) >= maxActive:
		return wire.FailBusy, true
	}
	return "", false
}

func (in *Inbox) chunk(c wire.Chunk, now time.Time) (wire.TransferStatus, *Saved) {
	t := in.active[c.ID]
	if t == nil {
		return failed(wire.TransferStatus{ID: c.ID}, wire.FailProtocol), nil
	}
	if c.Offset != t.received || t.received+int64(len(c.Data)) > t.offer.Size {
		return in.abort(t, wire.FailProtocol), nil
	}
	if _, err := t.tmp.Write(c.Data); err != nil {
		return in.abort(t, wire.FailIO), nil
	}
	t.hash.Write(c.Data)
	t.received += int64(len(c.Data))
	t.touched = now
	if t.received == t.offer.Size {
		return in.finish(t)
	}
	return t.status(), nil
}

func (in *Inbox) finish(t *transfer) (wire.TransferStatus, *Saved) {
	var sum wire.Digest
	copy(sum[:], t.hash.Sum(nil))
	if sum != t.offer.SHA256 {
		return in.abort(t, wire.FailMismatch), nil
	}
	if err := t.tmp.Close(); err != nil {
		return in.abort(t, wire.FailIO), nil
	}
	path, err := placeNoClobber(t.tmp.Name(), in.dir, t.offer.Name)
	if err != nil {
		return in.abort(t, wire.FailIO), nil
	}
	delete(in.active, t.offer.ID)
	st := t.status()
	st.State = wire.TransferDone
	st.Name = filepath.Base(path)
	st.SHA256 = &sum
	st.Path = path
	in.done[t.offer.ID] = st
	return st, &Saved{Path: path, Name: st.Name, Size: st.Size, SHA256: sum}
}

// Expire fails transfers idle longer than idleTimeout.
func (in *Inbox) Expire(now time.Time) []wire.TransferStatus {
	var out []wire.TransferStatus
	for _, t := range in.active {
		if now.Sub(t.touched) > idleTimeout {
			out = append(out, in.abort(t, wire.FailTimeout))
		}
	}
	return out
}

// AbortAll fails every active transfer. The wrapper calls it when the relay link
// drops and at exit. Calling it twice returns nothing the second time.
func (in *Inbox) AbortAll(reason wire.FailReason) []wire.TransferStatus {
	var out []wire.TransferStatus
	for _, t := range in.active {
		out = append(out, in.abort(t, reason))
	}
	return out
}

// abort is the only way out of receiving other than done, and it always
// removes the .part file, so a failed transfer leaves nothing behind.
func (in *Inbox) abort(t *transfer, reason wire.FailReason) wire.TransferStatus {
	t.tmp.Close()
	os.Remove(t.tmp.Name())
	delete(in.active, t.offer.ID)
	return failed(t.status(), reason)
}

func (in *Inbox) partPath(id wire.TransferID) string {
	return filepath.Join(in.dir, partPrefix+id.String()+partSuffix)
}

func (t *transfer) status() wire.TransferStatus {
	return wire.TransferStatus{
		ID:       t.offer.ID,
		State:    wire.TransferReceiving,
		Name:     t.offer.Name,
		Size:     t.offer.Size,
		Received: t.received,
	}
}

func failed(st wire.TransferStatus, reason wire.FailReason) wire.TransferStatus {
	st.State = wire.TransferFailed
	st.Reason = reason
	return st
}
