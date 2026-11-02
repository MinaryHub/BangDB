// Package wire is the client/server protocol of BangDB's server mode.
//
// Every message is type(1) length(4, big-endian) payload. Strings are a
// uvarint length and the bytes; value lists use the row record encoding of
// package value (NULL, INTEGER, REAL, TEXT, BLOB).
//
//	client -> server
//	  'h' hello    magic "BDB1" user password
//	  'q' query    mode(1: 0 exec, 1 query) sql nargs(uvarint) names(nargs strings) values(row)
//	  'x' cancel   stop the running statement
//	  'p' ping
//	server -> client
//	  'o' ok       hello accepted, or pong
//	  'k' columns  names(row) types(row)
//	  'r' row      values(row)
//	  'd' done     rowsAffected(varint) lastInsertID(varint) inTx(1)
//	  'e' error    code(1) message
//
// A statement is answered with columns (if it returns rows), its rows and
// done, or with an error at any point; a cancel that arrives after the
// statement ended is ignored.
package wire

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Magic starts the hello message.
const Magic = "BDB1"

// Message types.
const (
	Hello   = 'h'
	Query   = 'q'
	Cancel  = 'x'
	Ping    = 'p'
	OK      = 'o'
	Columns = 'k'
	Row     = 'r'
	Done    = 'd'
	Error   = 'e'
)

// Query modes.
const (
	ModeExec  = 0
	ModeQuery = 1
)

// Error codes, so that clients can rebuild the engine's error kinds.
const (
	CodeGeneric      = 0
	CodeConstraint   = 1
	CodeBusy         = 2
	CodeReadOnly     = 3
	CodeTxDone       = 4
	CodeInterrupted  = 5
	CodeUnauthorized = 6
	CodeTooMany      = 7
	CodeForbidden    = 8
)

// MaxMessage bounds a message's payload.
const MaxMessage = 256 << 20

// WriteMsg writes one message (buffered; the caller flushes).
func WriteMsg(w *bufio.Writer, typ byte, payload []byte) error {
	var h [5]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	w.Write(h[:])
	_, err := w.Write(payload)
	return err
}

// ReadMsg reads one message.
func ReadMsg(r *bufio.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxMessage {
		return 0, nil, fmt.Errorf("wire: message of %d bytes is too large", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return 0, nil, err
	}
	return h[0], b, nil
}

// AppendString appends a length-prefixed string.
func AppendString(b []byte, s string) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// AppendBytes appends a length-prefixed byte string.
func AppendBytes(b, s []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(s))), s...)
}

// Reader decodes a payload.
type Reader struct {
	B   []byte
	Err error
}

var errShort = errors.New("wire: malformed message")

func (r *Reader) Uvarint() uint64 {
	v, n := binary.Uvarint(r.B)
	if n <= 0 {
		r.Err = errShort
		return 0
	}
	r.B = r.B[n:]
	return v
}

func (r *Reader) Varint() int64 {
	v, n := binary.Varint(r.B)
	if n <= 0 {
		r.Err = errShort
		return 0
	}
	r.B = r.B[n:]
	return v
}

func (r *Reader) Bytes() []byte {
	n := r.Uvarint()
	if r.Err != nil || n > uint64(len(r.B)) {
		r.Err = errShort
		return nil
	}
	b := r.B[:n]
	r.B = r.B[n:]
	return b
}

func (r *Reader) String() string { return string(r.Bytes()) }

func (r *Reader) Byte() byte {
	if len(r.B) == 0 {
		r.Err = errShort
		return 0
	}
	c := r.B[0]
	r.B = r.B[1:]
	return c
}
