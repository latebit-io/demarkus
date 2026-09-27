package protocol

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"math"
	"strings"
	"testing"
)

func TestParseCursorRoundTrip(t *testing.T) {
	tests := []struct {
		in   string
		want Cursor
		ok   bool
	}{
		{"world-a:0", Cursor{Epoch: "world-a", Seq: 0}, true},
		{"world-a:42", Cursor{Epoch: "world-a", Seq: 42}, true},
		{"01HZX.local_1:18446744073709551615", Cursor{Epoch: "01HZX.local_1", Seq: 18446744073709551615}, true},
		{"", Cursor{}, false},
		{"world-a", Cursor{}, false},
		{":1", Cursor{}, false},
		{"world-a:", Cursor{}, false},
		{"world-a:01", Cursor{}, false},
		{"world-a:-1", Cursor{}, false},
		{"world-a:+1", Cursor{}, false},
		{"world-a:18446744073709551616", Cursor{}, false},
		{"world a:1", Cursor{}, false},
		{"a:b:1", Cursor{}, false},
		{strings.Repeat("e", maxCursorEpochLength+1) + ":1", Cursor{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseCursor(tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseCursor(%q) err = %v, want ok=%v", tt.in, err, tt.ok)
			}
			if !tt.ok {
				if !errors.Is(err, ErrInvalidCursor) {
					t.Fatalf("err = %v, want ErrInvalidCursor", err)
				}
				return
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
			if got.String() != tt.in {
				t.Fatalf("String() = %q, want %q", got.String(), tt.in)
			}
		})
	}
	if !(Cursor{}).IsZero() || (Cursor{Epoch: "w"}).IsZero() {
		t.Fatal("IsZero: epoch decides")
	}
}

// readAll drains a stream of blocks until EOF or a terminal error.
func readAll(t *testing.T, wire string) ([]WatchBlock, error) {
	t.Helper()
	r := NewWatchReader(strings.NewReader(wire))
	var blocks []WatchBlock
	for {
		b, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) && !errors.Is(err, ErrMalformedWatchBlock) {
				return blocks, nil
			}
			return blocks, err
		}
		blocks = append(blocks, b)
	}
}

func TestWatchStreamRoundTrip(t *testing.T) {
	head := Cursor{Epoch: "world-a", Seq: 7}
	event := WatchEvent{
		Cursor:  Cursor{Epoch: "world-a", Seq: 8},
		Path:    "/agents/me/inbox/01HZX.md",
		Version: 1,
		Hash:    "sha256-" + strings.Repeat("ab", 32),
		Op:      OpPublish,
		Agent:   "claude-code",
	}
	archive := WatchEvent{Cursor: Cursor{Epoch: "world-a", Seq: 9}, Path: "/a.md", Version: 3, Op: OpArchive}
	var wire bytes.Buffer
	for _, b := range []WatchBlock{
		WatchControl(StatusOK, head),
		event.Block(),
		WatchControl(StatusOK, event.Cursor),
		archive.Block(),
		WatchControl(StatusClosing, archive.Cursor),
	} {
		if _, err := b.WriteTo(&wire); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	blocks, err := readAll(t, wire.String())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(blocks) != 5 {
		t.Fatalf("read %d blocks, want 5", len(blocks))
	}
	if blocks[0].Status != StatusOK {
		t.Fatalf("ack status = %q", blocks[0].Status)
	}
	if c, err := blocks[0].Cursor(); err != nil || c != head {
		t.Fatalf("ack cursor = %v, %v; want %v", c, err, head)
	}
	got, err := blocks[1].Event()
	if err != nil {
		t.Fatalf("event: %v", err)
	}
	if got != event {
		t.Fatalf("event = %+v, want %+v", got, event)
	}
	if blocks[2].Status != StatusOK {
		t.Fatalf("heartbeat status = %q", blocks[2].Status)
	}
	if got, err := blocks[3].Event(); err != nil || got != archive {
		t.Fatalf("archive event = %+v, %v; want %+v", got, err, archive)
	}
	if blocks[4].Status != StatusClosing {
		t.Fatalf("terminal status = %q, want closing", blocks[4].Status)
	}
	if _, err := blocks[0].Event(); !errors.Is(err, ErrMalformedWatchBlock) {
		t.Fatalf("Event() on a control block: err = %v, want ErrMalformedWatchBlock", err)
	}
}

// The wire form is what a server writes; pin it so the SDK's fixtures and the
// server agree byte for byte.
func TestWatchBlockWireForm(t *testing.T) {
	var wire bytes.Buffer
	if _, err := (WatchEvent{Cursor: Cursor{Epoch: "w", Seq: 1}, Path: "/a.md", Version: 2, Op: OpAppend}).Block().WriteTo(&wire); err != nil {
		t.Fatal(err)
	}
	want := "---\ncursor: w:1\nop: append\npath: /a.md\nversion: \"2\"\n---\n"
	if wire.String() != want {
		t.Fatalf("wire = %q, want %q", wire.String(), want)
	}
	wire.Reset()
	if _, err := WatchControl(StatusUnauthorized, Cursor{}).WriteTo(&wire); err != nil {
		t.Fatal(err)
	}
	if want := "---\nstatus: unauthorized\n---\n"; wire.String() != want {
		t.Fatalf("wire = %q, want %q", wire.String(), want)
	}
}

func TestWatchReaderIgnoresUnknownKeysAndOps(t *testing.T) {
	wire := "---\ncursor: w:3\npath: /a.md\nversion: \"1\"\nop: rename\nsection: intro\nbody: inline\n---\n"
	blocks, err := readAll(t, wire)
	if err != nil || len(blocks) != 1 {
		t.Fatalf("read: %d blocks, %v", len(blocks), err)
	}
	e, err := blocks[0].Event()
	if err != nil {
		t.Fatalf("event with unknown keys: %v", err)
	}
	if e.Op != "rename" || IsKnownOp(e.Op) {
		t.Fatalf("op = %q, want the unknown op kept and reported unknown", e.Op)
	}
	if blocks[0].Metadata["section"] != "intro" {
		t.Fatal("unknown key dropped by the reader")
	}
	for _, op := range []string{OpPublish, OpAppend, OpArchive} {
		if !IsKnownOp(op) {
			t.Fatalf("IsKnownOp(%q) = false", op)
		}
	}
}

func TestWatchReaderRejectsOversizeBlock(t *testing.T) {
	long := "---\nagent: " + strings.Repeat("x", MaxWatchBlockLength) + "\n---\n"
	_, err := readAll(t, long)
	if !errors.Is(err, ErrMalformedWatchBlock) {
		t.Fatalf("oversize block: err = %v, want ErrMalformedWatchBlock", err)
	}
	many := "---\n"
	for i := 0; many != "" && len(many) <= MaxWatchBlockLength; i++ {
		many += "k" + strings.Repeat("0", 60) + ": v\n"
	}
	many += "---\n"
	if _, err := readAll(t, many); !errors.Is(err, ErrMalformedWatchBlock) {
		t.Fatalf("block of many lines over the limit: err = %v, want ErrMalformedWatchBlock", err)
	}
	var wire bytes.Buffer
	_, err = (WatchBlock{Metadata: map[string]string{"agent": strings.Repeat("x", MaxWatchBlockLength)}}).WriteTo(&wire)
	if !errors.Is(err, ErrMalformedWatchBlock) || wire.Len() != 0 {
		t.Fatalf("WriteTo over the limit: err = %v, wrote %d bytes; want refused and nothing written", err, wire.Len())
	}
}

func TestWatchReaderMalformed(t *testing.T) {
	tests := []struct {
		name string
		wire string
		want error
	}{
		{"cut inside a block", "---\ncursor: w:1\n", io.ErrUnexpectedEOF},
		{"no opening fence", "cursor: w:1\n---\n", ErrMalformedWatchBlock},
		{"body after a block", "---\nstatus: ok\n---\n# Not a block\n", ErrMalformedWatchBlock},
		{"yaml that is not a map", "---\n- a\n- b\n---\n", ErrMalformedWatchBlock},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := readAll(t, tt.wire); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if blocks, err := readAll(t, ""); err != nil || len(blocks) != 0 {
		t.Fatalf("empty stream: %d blocks, %v", len(blocks), err)
	}
}

func TestWatchEventValidation(t *testing.T) {
	good := map[string]string{"cursor": "w:1", "path": "/a.md", "version": "1", "op": OpPublish}
	tests := []struct {
		name  string
		key   string
		value string
	}{
		{"missing cursor", "cursor", ""},
		{"bad cursor", "cursor", "w"},
		{"relative path", "path", "a.md"},
		{"missing path", "path", ""},
		{"version zero", "version", "0"},
		{"version text", "version", "one"},
		{"missing op", "op", ""},
		{"bad hash", "hash", "sha256-nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := maps.Clone(good)
			m[tt.key] = tt.value
			if _, err := (WatchBlock{Metadata: m}).Event(); !errors.Is(err, ErrMalformedWatchBlock) {
				t.Fatalf("err = %v, want ErrMalformedWatchBlock", err)
			}
		})
	}
	if _, err := (WatchBlock{Metadata: map[string]string{"bad key": "v"}}).WriteTo(io.Discard); !errors.Is(err, ErrMalformedWatchBlock) {
		t.Fatalf("WriteTo with an invalid key: err = %v", err)
	}
	if _, err := (WatchBlock{Metadata: map[string]string{"agent": "a\n---\nb"}}).WriteTo(io.Discard); !errors.Is(err, ErrMalformedWatchBlock) {
		t.Fatalf("WriteTo with a newline in a value: err = %v", err)
	}
}

func TestWatchIsAValidVerb(t *testing.T) {
	req, err := ParseRequest(strings.NewReader("WATCH /agents/me/inbox/\n---\nsince: w:41\n---\n"))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Verb != VerbWatch || req.Metadata["since"] != "w:41" {
		t.Fatalf("parsed %+v", req)
	}
	if !IsValidVerb(VerbWatch) {
		t.Fatal("IsValidVerb(WATCH) = false")
	}
}

func FuzzWatchReader(f *testing.F) {
	f.Add([]byte("---\nstatus: ok\ncursor: w:1\n---\n"))
	f.Add([]byte("---\ncursor: w:2\npath: /a.md\nversion: \"1\"\nop: publish\n---\n---\nstatus: closing\ncursor: w:2\n---\n"))
	f.Add([]byte("---\n"))
	f.Add([]byte("no fence"))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewWatchReader(bytes.NewReader(data))
		for {
			b, err := r.Next()
			if err != nil {
				if !errors.Is(err, io.EOF) && !errors.Is(err, ErrMalformedWatchBlock) {
					t.Fatalf("unexpected error class: %v", err)
				}
				return
			}
			if b.Metadata == nil {
				t.Fatal("nil metadata on success")
			}
			// A decoded event must re-encode within the limit and read back equal.
			if b.Status != "" {
				continue
			}
			e, err := b.Event()
			if err != nil {
				continue
			}
			var wire bytes.Buffer
			if _, err := e.Block().WriteTo(&wire); err != nil {
				// Values the wire accepted may still breach the metadata grammar.
				continue
			}
			back, err := NewWatchReader(&wire).Next()
			if err != nil {
				t.Fatalf("re-read: %v", err)
			}
			got, err := back.Event()
			if err != nil || got != e {
				t.Fatalf("event round trip: %+v, %v; want %+v", got, err, e)
			}
		}
	})
}

func FuzzWatchBlockRoundTrip(f *testing.F) {
	f.Add("ok", "cursor", "w:1")
	f.Add("", "path", "/a.md")
	f.Add("resync", "agent", "claude-code")
	f.Fuzz(func(t *testing.T, status, key, value string) {
		if !IsValidMetaKey(key) || !IsValidMetaValue(value) || !IsValidMetaValue(status) || key == "status" {
			t.Skip()
		}
		in := WatchBlock{Status: status, Metadata: map[string]string{key: value}}
		var wire bytes.Buffer
		if _, err := in.WriteTo(&wire); err != nil {
			if errors.Is(err, ErrMalformedWatchBlock) && wire.Len() == 0 {
				return
			}
			t.Fatalf("write: %v", err)
		}
		r := NewWatchReader(&wire)
		out, err := r.Next()
		if err != nil {
			t.Fatalf("read back %q: %v", wire.String(), err)
		}
		if out.Status != in.Status || out.Metadata[key] != value || len(out.Metadata) != 1 {
			t.Fatalf("round trip: got %+v, want %+v", out, in)
		}
		if _, err := r.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("after one block: err = %v, want EOF", err)
		}
	})
}

func FuzzParseCursor(f *testing.F) {
	f.Add("w:1")
	f.Add("world-a:18446744073709551615")
	f.Add(":")
	f.Fuzz(func(t *testing.T, s string) {
		c, err := ParseCursor(s)
		if err != nil {
			return
		}
		if c.String() != s {
			t.Fatalf("ParseCursor(%q).String() = %q", s, c.String())
		}
	})
}

// Every field a store can commit fits one block: the request path and
// metadata limits compose below the block limit, so a server never has to
// truncate an event.
func TestWatchBlockFitsEveryStoredField(t *testing.T) {
	event := WatchEvent{
		Cursor:  Cursor{Epoch: strings.Repeat("e", maxCursorEpochLength), Seq: math.MaxUint64},
		Path:    "/" + strings.Repeat("p", MaxRequestPathLength-1),
		Version: math.MaxInt,
		Hash:    "sha256-" + strings.Repeat("f", 64),
		Op:      OpPublish,
		Agent:   strings.Repeat("a", MaxMetaBytes),
	}
	var buf bytes.Buffer
	if _, err := event.Block().WriteTo(&buf); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	block, err := NewWatchReader(&buf).Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	got, err := block.Event()
	if err != nil {
		t.Fatalf("Event: %v", err)
	}
	if got != event {
		t.Fatalf("round trip changed the event:\n got %+v\nwant %+v", got, event)
	}
}
