package protocol

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// A WATCH stream (§6.8) is a sequence of frontmatter blocks with no bodies:
// an acknowledgement, then events and heartbeats, until a terminal status.
// Events are hints; a client fetches content by hash.
const (
	// MaxWatchBlockLength bounds one block, fences included, so a client reads
	// a stream with a fixed buffer.
	MaxWatchBlockLength = 8192

	// WatchHeartbeatInterval is how often an idle stream carries a heartbeat,
	// so a client can tell a quiet stream from a stalled one.
	WatchHeartbeatInterval = 20 * time.Second

	// OpPublish, OpAppend and OpArchive are the event operations. A client
	// skips an op it does not know rather than failing the stream.
	OpPublish = "publish"
	OpAppend  = "append"
	OpArchive = "archive"

	// maxCursorEpochLength bounds the epoch part of a cursor.
	maxCursorEpochLength = 64
)

// ErrMalformedWatchBlock marks a block that breaks the block grammar or a
// block whose keys do not decode to what its kind requires.
var ErrMalformedWatchBlock = errors.New("malformed watch block")

// ErrInvalidCursor marks a cursor that is not <epoch>:<seq>.
var ErrInvalidCursor = errors.New("invalid cursor")

// IsKnownOp reports an event operation this version of the protocol defines.
func IsKnownOp(op string) bool {
	switch op {
	case OpPublish, OpAppend, OpArchive:
		return true
	default:
		return false
	}
}

// Cursor is a position in a world's change sequence: an epoch that changes
// when the sequence restarts and a sequence number within it. Order holds
// within one epoch only; a cursor from another epoch gets resync.
type Cursor struct {
	Epoch string
	Seq   uint64
}

// ParseCursor parses the wire form <epoch>:<seq>. The epoch is 1 to 64
// characters of letters, digits, dot, underscore or hyphen; seq is a decimal
// without leading zeros, so the wire form round-trips byte for byte.
func ParseCursor(s string) (Cursor, error) {
	epoch, seq, ok := strings.Cut(s, ":")
	n, err := strconv.ParseUint(seq, 10, 64)
	if !ok || !isValidEpoch(epoch) || err != nil || strconv.FormatUint(n, 10) != seq {
		return Cursor{}, fmt.Errorf("%w: %q", ErrInvalidCursor, s)
	}
	return Cursor{Epoch: epoch, Seq: n}, nil
}

// String returns the wire form.
func (c Cursor) String() string {
	return c.Epoch + ":" + strconv.FormatUint(c.Seq, 10)
}

// IsZero reports the absence of a cursor: a fresh subscription.
func (c Cursor) IsZero() bool { return c.Epoch == "" }

func isValidEpoch(s string) bool {
	if s == "" || len(s) > maxCursorEpochLength {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// WatchBlock is one block on a WATCH stream. Status is set on control blocks
// (acknowledgement, heartbeat, terminal) and empty on event blocks. Metadata
// holds every other key; a reader keeps keys it does not know.
type WatchBlock struct {
	Status   string
	Metadata map[string]string
}

// WatchControl is an acknowledgement, heartbeat or terminal block. A zero
// cursor is omitted: a stream refused before subscription has none.
func WatchControl(status string, cursor Cursor) WatchBlock {
	b := WatchBlock{Status: status, Metadata: map[string]string{}}
	if !cursor.IsZero() {
		b.Metadata["cursor"] = cursor.String()
	}
	return b
}

// Cursor returns the block's cursor.
func (b WatchBlock) Cursor() (Cursor, error) {
	return ParseCursor(b.Metadata["cursor"])
}

// WriteTo writes the block as a fenced frontmatter block with no body. Keys
// and values must satisfy the metadata grammar, so the line reader on the
// other side never meets a fence inside a value.
func (b WatchBlock) WriteTo(w io.Writer) (int64, error) {
	for k, v := range b.Metadata {
		if !IsValidMetaKey(k) || !IsValidMetaValue(v) {
			return 0, fmt.Errorf("%w: key %q", ErrMalformedWatchBlock, k)
		}
	}
	block, err := encodeStatusBlock(b.Status, b.Metadata)
	if err != nil {
		return 0, fmt.Errorf("encoding watch block: %w", err)
	}
	if len(block) > MaxWatchBlockLength {
		return 0, fmt.Errorf("%w: %d > %d bytes", ErrMalformedWatchBlock, len(block), MaxWatchBlockLength)
	}
	n, err := w.Write(block)
	return int64(n), err
}

// WatchReader decodes blocks from a stream one at a time, holding at most one
// block, so a slow consumer bounds memory rather than the peer.
type WatchReader struct {
	br *bufio.Reader
}

// NewWatchReader wraps r.
func NewWatchReader(r io.Reader) *WatchReader {
	return &WatchReader{br: bufio.NewReader(r)}
}

// Next returns the next block. io.EOF at a block boundary is the stream's
// end; a cut inside a block is ErrMalformedWatchBlock wrapping
// io.ErrUnexpectedEOF.
func (r *WatchReader) Next() (WatchBlock, error) {
	line, err := readLineLimited(r.br, MaxWatchBlockLength)
	if err != nil {
		if errors.Is(err, io.EOF) {
			return WatchBlock{}, io.EOF
		}
		return WatchBlock{}, fmt.Errorf("reading watch block: %w", err)
	}
	if line+"\n" != FrontmatterFence {
		return WatchBlock{}, fmt.Errorf("%w: expected opening fence", ErrMalformedWatchBlock)
	}
	var block []byte
	size := len(FrontmatterFence)
	for {
		line, err := readLineLimited(r.br, MaxWatchBlockLength)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return WatchBlock{}, fmt.Errorf("%w: %w", ErrMalformedWatchBlock, io.ErrUnexpectedEOF)
			}
			return WatchBlock{}, fmt.Errorf("%w: %w", ErrMalformedWatchBlock, err)
		}
		size += len(line) + 1
		if size > MaxWatchBlockLength {
			return WatchBlock{}, fmt.Errorf("%w: exceeds %d bytes", ErrMalformedWatchBlock, MaxWatchBlockLength)
		}
		if line+"\n" == FrontmatterFence {
			break
		}
		block = append(block, line...)
		block = append(block, '\n')
	}
	return decodeWatchBlock(block)
}

// decodeWatchBlock parses the text between the fences.
func decodeWatchBlock(block []byte) (WatchBlock, error) {
	status, meta, err := decodeStatusBlock(block)
	if err != nil {
		return WatchBlock{}, fmt.Errorf("%w: %w", ErrMalformedWatchBlock, err)
	}
	return WatchBlock{Status: status, Metadata: meta}, nil
}

// WatchEvent is one change hint: what changed and the cursor to resume from.
type WatchEvent struct {
	Cursor  Cursor
	Path    string
	Version int
	// Hash addresses the new version's content; empty when the op stores no
	// new content.
	Hash string
	Op   string
	// Agent is the writing software; User the verified person behind it, when
	// the surface stamps one. Both are the write's metadata keys of that name.
	Agent string
	User  string
}

// Event decodes an event block. Keys the event does not define are ignored,
// and an op this version does not know is returned as is for IsKnownOp.
func (b WatchBlock) Event() (WatchEvent, error) {
	if b.Status != "" {
		return WatchEvent{}, fmt.Errorf("%w: a control block is not an event", ErrMalformedWatchBlock)
	}
	cursor, err := b.Cursor()
	if err != nil {
		return WatchEvent{}, fmt.Errorf("%w: %w", ErrMalformedWatchBlock, err)
	}
	e := WatchEvent{Cursor: cursor, Path: b.Metadata["path"], Hash: b.Metadata["hash"], Op: b.Metadata["op"], Agent: b.Metadata[MetaAgent], User: b.Metadata[MetaUser]}
	if err := ValidateRequestPath(e.Path); err != nil {
		return WatchEvent{}, fmt.Errorf("%w: %w", ErrMalformedWatchBlock, err)
	}
	if e.Version, err = strconv.Atoi(b.Metadata["version"]); err != nil || e.Version < 1 {
		return WatchEvent{}, fmt.Errorf("%w: version %q", ErrMalformedWatchBlock, b.Metadata["version"])
	}
	if e.Hash != "" {
		if _, ok := IsHashPath(e.Hash); !ok {
			return WatchEvent{}, fmt.Errorf("%w: hash %q", ErrMalformedWatchBlock, e.Hash)
		}
	}
	if e.Op == "" {
		return WatchEvent{}, fmt.Errorf("%w: missing op", ErrMalformedWatchBlock)
	}
	return e, nil
}

// Block encodes the event; empty Hash, Agent and User are omitted.
func (e WatchEvent) Block() WatchBlock {
	m := map[string]string{
		"cursor":  e.Cursor.String(),
		"path":    e.Path,
		"version": strconv.Itoa(e.Version),
		"op":      e.Op,
	}
	if e.Hash != "" {
		m["hash"] = e.Hash
	}
	if e.Agent != "" {
		m[MetaAgent] = e.Agent
	}
	if e.User != "" {
		m[MetaUser] = e.User
	}
	return WatchBlock{Metadata: m}
}
