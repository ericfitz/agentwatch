package inbox_test

import (
	"crypto/sha256"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ericfitz/agentwatch/internal/inbox"
	"github.com/ericfitz/agentwatch/internal/wire"
)

var now = time.Date(2026, 10, 21, 5, 3, 0, 0, time.UTC)

func openInbox(t *testing.T) (*inbox.Inbox, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "agentwatch-inbox")
	in, err := inbox.Open(inbox.Config{Dir: dir, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	return in, dir
}

func tid(n byte) wire.TransferID { return wire.TransferID{0xaa, n} }

func offer(id wire.TransferID, name string, data []byte) wire.Offer {
	return wire.Offer{ID: id, Name: name, Size: int64(len(data)), SHA256: sha256.Sum256(data)}
}

func upload(t *testing.T, in *inbox.Inbox, id wire.TransferID, name string, data []byte) (wire.TransferStatus, *inbox.Saved) {
	t.Helper()
	st, saved := in.Handle(offer(id, name, data), now)
	if st.State != wire.TransferReceiving {
		return st, saved
	}
	return in.Handle(wire.Chunk{ID: id, Data: data}, now)
}

func listDir(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func partFiles(t *testing.T, dir string) []string {
	t.Helper()
	var parts []string
	for _, n := range listDir(t, dir) {
		if strings.HasSuffix(n, ".part") {
			parts = append(parts, n)
		}
	}
	return parts
}

func wantFailed(t *testing.T, st wire.TransferStatus, saved *inbox.Saved, id wire.TransferID, reason wire.FailReason) {
	t.Helper()
	if st.ID != id || st.State != wire.TransferFailed || st.Reason != reason || saved != nil {
		t.Errorf("status = %+v, saved = %v; want failed(%s) for %s", st, saved, reason, id)
	}
}

func TestThreeChunkTransfer(t *testing.T) {
	in, dir := openInbox(t)
	data := []byte("hello, agentwatch!")
	id := tid(1)
	digest := wire.Digest(sha256.Sum256(data))

	st, saved := in.Handle(offer(id, "hello.txt", data), now)
	if st.State != wire.TransferReceiving || st.Received != 0 || saved != nil {
		t.Fatalf("offer: %+v, %v", st, saved)
	}
	for i := 0; i < 3; i++ {
		st, saved = in.Handle(wire.Chunk{ID: id, Offset: int64(6 * i), Data: data[6*i : 6*i+6]}, now)
		if i < 2 && (st.State != wire.TransferReceiving || st.Received != int64(6*(i+1)) || saved != nil) {
			t.Fatalf("chunk %d: %+v, %v", i, st, saved)
		}
	}

	path := filepath.Join(dir, "hello.txt")
	wantStatus := wire.TransferStatus{ID: id, State: wire.TransferDone, Name: "hello.txt", Size: 18, Received: 18, SHA256: &digest, Path: path}
	if !reflect.DeepEqual(st, wantStatus) {
		t.Errorf("done status\n got %+v\nwant %+v", st, wantStatus)
	}
	if want := (&inbox.Saved{Path: path, Name: "hello.txt", Size: 18, SHA256: digest}); !reflect.DeepEqual(saved, want) {
		t.Errorf("saved = %+v, want %+v", saved, want)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(data) {
		t.Errorf("file at %s = %q, %v", path, got, err)
	}
	if got := listDir(t, dir); !reflect.DeepEqual(got, []string{"hello.txt"}) {
		t.Errorf("dir = %v, want only hello.txt", got)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("file mode = %v, want 0600", fi.Mode())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %v, want 0700", fi.Mode())
	}
}

func TestDigestMismatchLeavesNothing(t *testing.T) {
	in, dir := openInbox(t)
	o := offer(tid(1), "a.txt", []byte("expected"))
	in.Handle(o, now)
	st, saved := in.Handle(wire.Chunk{ID: o.ID, Data: []byte("tampered")}, now)
	wantFailed(t, st, saved, o.ID, wire.FailMismatch)
	if got := listDir(t, dir); len(got) != 0 {
		t.Errorf("dir = %v, want empty", got)
	}
}

func TestNamesLandInsideDir(t *testing.T) {
	in, dir := openInbox(t)
	cases := []struct{ raw, want string }{
		{"../../x", "x"},
		{"..", "upload"},
		{".bashrc", "bashrc"},
		{`a/b\c.txt`, "c.txt"},
		{"", "upload-1"},
		{strings.Repeat("a", 296) + ".txt", strings.Repeat("a", 116) + ".txt"},
		{"x" + strings.Repeat("中", 100) + ".md", "x" + strings.Repeat("中", 38) + ".md"},
		{strings.Repeat("a", 100) + "." + strings.Repeat("b", 30), strings.Repeat("a", 100) + "." + strings.Repeat("b", 19)},
		{"résumé 2026 ✓.pdf", "résumé_2026__.pdf"},
		{"$(rm -rf ~)`;|&.sh", "__rm_-rf" + strings.Repeat("_", 7) + ".sh"},
		{"a\xffb\x00.txt", "a_b_.txt"},
	}
	for i, c := range cases {
		st, saved := upload(t, in, tid(byte(i)), c.raw, []byte{byte(i)})
		if st.State != wire.TransferDone || saved == nil {
			t.Errorf("%q: %+v", c.raw, st)
			continue
		}
		if want := filepath.Join(dir, c.want); st.Path != want || saved.Path != want || saved.Name != c.want {
			t.Errorf("%q landed at %q (saved %+v), want %q", c.raw, st.Path, saved, want)
		}
		if len(c.want) > 120 {
			t.Errorf("%q: expected name is %d bytes, test is wrong", c.raw, len(c.want))
		}
	}
	for _, name := range listDir(t, dir) {
		if strings.HasPrefix(name, ".") {
			t.Errorf("hidden file %q in inbox", name)
		}
	}
}

func TestSecondCopyIsNumbered(t *testing.T) {
	in, dir := openInbox(t)
	upload(t, in, tid(1), "a.txt", []byte("first"))
	st, saved := upload(t, in, tid(2), "a.txt", []byte("second"))
	if st.Path != filepath.Join(dir, "a-1.txt") || saved.Name != "a-1.txt" || st.Name != "a-1.txt" {
		t.Errorf("second a.txt: status %+v, saved %+v", st, saved)
	}
	upload(t, in, tid(3), "b.tar.gz", []byte("x"))
	if st, _ := upload(t, in, tid(4), "b.tar.gz", []byte("y")); st.Path != filepath.Join(dir, "b.tar-1.gz") {
		t.Errorf("second b.tar.gz at %q", st.Path)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "first" {
		t.Errorf("a.txt overwritten: %q", b)
	}
}

func TestPlantedSymlinkIsNotFollowed(t *testing.T) {
	in, dir := openInbox(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "precious")
	missing := filepath.Join(outside, "missing")
	if err := os.WriteFile(target, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(missing, filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}

	if st, _ := upload(t, in, tid(1), "a.txt", []byte("payload")); st.Path != filepath.Join(dir, "a-1.txt") {
		t.Errorf("a.txt landed at %q", st.Path)
	}
	if st, _ := upload(t, in, tid(2), "b.txt", []byte("payload")); st.Path != filepath.Join(dir, "b-1.txt") {
		t.Errorf("b.txt landed at %q", st.Path)
	}
	if b, _ := os.ReadFile(target); string(b) != "precious" {
		t.Errorf("symlink target rewritten: %q", b)
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Errorf("dangling symlink target was created: %v", err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "a.txt")); got != target {
		t.Errorf("planted symlink now points at %q", got)
	}
}

func TestFailuresRemovePartFile(t *testing.T) {
	t.Run("oversize", func(t *testing.T) {
		in, dir := openInbox(t)
		st, saved := in.Handle(wire.Offer{ID: tid(1), Name: "big", Size: 1<<20 + 1}, now)
		wantFailed(t, st, saved, tid(1), wire.FailTooLarge)
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v", got)
		}
	})

	t.Run("fifth concurrent, then AbortAll", func(t *testing.T) {
		in, dir := openInbox(t)
		for i := byte(0); i < 4; i++ {
			if st, _ := in.Handle(offer(tid(i), "f", []byte("0123456789")), now); st.State != wire.TransferReceiving {
				t.Fatalf("offer %d: %+v", i, st)
			}
		}
		st, saved := in.Handle(offer(tid(4), "f", []byte("0123456789")), now)
		wantFailed(t, st, saved, tid(4), wire.FailBusy)
		st, saved = in.Handle(wire.Offer{ID: tid(5), Name: "big", Size: 1<<20 + 1}, now)
		wantFailed(t, st, saved, tid(5), wire.FailTooLarge)
		if got := partFiles(t, dir); len(got) != 4 {
			t.Errorf(".part files = %v, want 4", got)
		}

		aborted := in.AbortAll(wire.FailCancelled)
		ids := map[wire.TransferID]bool{}
		for _, st := range aborted {
			wantFailed(t, st, nil, st.ID, wire.FailCancelled)
			ids[st.ID] = true
		}
		if len(ids) != 4 {
			t.Errorf("AbortAll failed %d distinct transfers, want 4", len(ids))
		}
		if again := in.AbortAll(wire.FailCancelled); len(again) != 0 {
			t.Errorf("second AbortAll = %v, want nothing", again)
		}
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v, want empty", got)
		}
	})

	t.Run("offset gap", func(t *testing.T) {
		in, dir := openInbox(t)
		in.Handle(offer(tid(1), "f", []byte("0123456789")), now)
		st, saved := in.Handle(wire.Chunk{ID: tid(1), Offset: 3, Data: []byte("345")}, now)
		wantFailed(t, st, saved, tid(1), wire.FailProtocol)
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v", got)
		}
	})

	t.Run("overrun", func(t *testing.T) {
		in, dir := openInbox(t)
		in.Handle(offer(tid(1), "f", []byte("abcd")), now)
		st, saved := in.Handle(wire.Chunk{ID: tid(1), Data: []byte("abcde")}, now)
		wantFailed(t, st, saved, tid(1), wire.FailProtocol)
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v", got)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		in, dir := openInbox(t)
		in.Handle(offer(tid(1), "f", []byte("0123456789")), now)
		in.Handle(wire.Chunk{ID: tid(1), Data: []byte("012")}, now)
		st, saved := in.Handle(wire.Cancel{ID: tid(1)}, now)
		wantFailed(t, st, saved, tid(1), wire.FailCancelled)
		if st.Received != 3 {
			t.Errorf("cancelled status reports %d received, want 3", st.Received)
		}
		st, saved = in.Handle(wire.Cancel{ID: tid(1)}, now)
		wantFailed(t, st, saved, tid(1), wire.FailCancelled)
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v", got)
		}
	})

	t.Run("expire", func(t *testing.T) {
		in, dir := openInbox(t)
		in.Handle(offer(tid(1), "f", []byte("0123456789")), now)
		in.Handle(wire.Chunk{ID: tid(1), Data: []byte("012")}, now.Add(10*time.Second))
		if got := in.Expire(now.Add(35 * time.Second)); len(got) != 0 {
			t.Fatalf("expired after 25 s idle: %v", got)
		}
		got := in.Expire(now.Add(41 * time.Second))
		if len(got) != 1 {
			t.Fatalf("Expire after 31 s idle = %v, want one timeout", got)
		}
		wantFailed(t, got[0], nil, tid(1), wire.FailTimeout)
		if got := listDir(t, dir); len(got) != 0 {
			t.Errorf("dir = %v", got)
		}
		st, saved := in.Handle(wire.Chunk{ID: tid(1), Offset: 3, Data: []byte("345")}, now.Add(42*time.Second))
		wantFailed(t, st, saved, tid(1), wire.FailProtocol)
	})
}

func TestZeroByteFileIsDoneOnOffer(t *testing.T) {
	in, dir := openInbox(t)
	st, saved := in.Handle(offer(tid(1), "empty.txt", nil), now)
	path := filepath.Join(dir, "empty.txt")
	if st.State != wire.TransferDone || st.Path != path || saved == nil || saved.Size != 0 {
		t.Fatalf("zero-byte offer: %+v, %+v", st, saved)
	}
	if b, err := os.ReadFile(path); err != nil || len(b) != 0 {
		t.Errorf("empty.txt = %q, %v", b, err)
	}

	bad := offer(tid(2), "bad.txt", nil)
	bad.SHA256[0] ^= 1
	st, saved = in.Handle(bad, now)
	wantFailed(t, st, saved, tid(2), wire.FailMismatch)
	if got := listDir(t, dir); !reflect.DeepEqual(got, []string{"empty.txt"}) {
		t.Errorf("dir = %v", got)
	}
}

func TestResentOfferAfterDone(t *testing.T) {
	in, dir := openInbox(t)
	data := []byte("once")
	first, saved := upload(t, in, tid(1), "a.txt", data)
	if first.State != wire.TransferDone || saved == nil {
		t.Fatalf("first upload: %+v, %v", first, saved)
	}
	in.AbortAll(wire.FailCancelled)

	again, saved := in.Handle(offer(tid(1), "a.txt", data), now)
	if !reflect.DeepEqual(again, first) || saved != nil {
		t.Errorf("resent Offer = %+v, %v; want the stored done status and no Saved", again, saved)
	}
	if st, saved := in.Handle(wire.Chunk{ID: tid(1), Data: data}, now); st.State != wire.TransferFailed || saved != nil {
		t.Errorf("chunk after replayed done = %+v, %v", st, saved)
	}

	st, saved := in.Handle(offer(tid(1), "a.txt", []byte("different")), now)
	wantFailed(t, st, saved, tid(1), wire.FailProtocol)

	if got := listDir(t, dir); !reflect.DeepEqual(got, []string{"a.txt"}) {
		t.Errorf("dir = %v, want only a.txt", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "once" {
		t.Errorf("a.txt = %q", b)
	}
}

func TestDuplicateActiveOfferKeepsFirstTransfer(t *testing.T) {
	in, dir := openInbox(t)
	data := []byte("abcdef")
	in.Handle(offer(tid(1), "a.txt", data), now)
	in.Handle(wire.Chunk{ID: tid(1), Data: data[:3]}, now)
	st, saved := in.Handle(offer(tid(1), "a.txt", data), now)
	wantFailed(t, st, saved, tid(1), wire.FailProtocol)
	st, saved = in.Handle(wire.Chunk{ID: tid(1), Offset: 3, Data: data[3:]}, now)
	if st.State != wire.TransferDone || saved == nil {
		t.Fatalf("first transfer did not survive the duplicate Offer: %+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "abcdef" {
		t.Errorf("a.txt = %q", b)
	}
}

func TestUnknownChunkChangesNothing(t *testing.T) {
	in, dir := openInbox(t)
	st, saved := in.Handle(wire.Chunk{ID: tid(9), Data: []byte("x")}, now)
	wantFailed(t, st, saved, tid(9), wire.FailProtocol)
	if got := listDir(t, dir); len(got) != 0 {
		t.Errorf("dir = %v", got)
	}
}

func TestNoFreeNameFailsWithIOError(t *testing.T) {
	in, dir := openInbox(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 1000; i++ {
		if err := os.WriteFile(filepath.Join(dir, "a-"+strconv.Itoa(i)+".txt"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	st, saved := upload(t, in, tid(1), "a.txt", []byte("no room"))
	wantFailed(t, st, saved, tid(1), wire.FailIO)
	if got := partFiles(t, dir); len(got) != 0 {
		t.Errorf(".part files left: %v", got)
	}
	if got := len(listDir(t, dir)); got != 1000 {
		t.Errorf("dir has %d entries, want the 1000 pre-existing files", got)
	}
}

func TestStalePartSweep(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-11 * time.Minute)
	files := map[string]time.Time{
		".agentwatch-" + tid(1).String() + ".part": old,
		".agentwatch-" + tid(2).String() + ".part": time.Now(),
		"notes.txt": old,
	}
	for name, mtime := range files {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := inbox.Open(inbox.Config{Dir: dir, MaxBytes: 1}); err != nil {
		t.Fatal(err)
	}
	want := []string{".agentwatch-" + tid(2).String() + ".part", "notes.txt"}
	if got := listDir(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("after Open, dir = %v, want %v", got, want)
	}
}

func TestDisabledInbox(t *testing.T) {
	in, err := inbox.Open(inbox.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if info := in.Info(); info.Enabled {
		t.Errorf("Info = %+v, want disabled", info)
	}
	st, saved := in.Handle(offer(tid(1), "a.txt", []byte("x")), now)
	wantFailed(t, st, saved, tid(1), wire.FailDisabled)
}

func TestOpenValidatesConfig(t *testing.T) {
	for name, cfg := range map[string]inbox.Config{
		"relative dir":   {Dir: "agentwatch-inbox", MaxBytes: 1},
		"zero max bytes": {Dir: t.TempDir(), MaxBytes: 0},
	} {
		if _, err := inbox.Open(cfg); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	in, err := inbox.Open(inbox.Config{Dir: t.TempDir(), MaxBytes: 42})
	if err != nil {
		t.Fatal(err)
	}
	if info := in.Info(); !info.Enabled || info.MaxBytes != 42 {
		t.Errorf("Info = %+v", info)
	}
}
