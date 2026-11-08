// Package replication provides online backup streaming and primary→replica
// mirroring for BangDB over TCP (or any net.Conn, e.g. TLS).
//
// A replica connects to a primary and sends its DBID and commit sequence.
// If the primary still has the following commits in its replication log the
// replica simply streams them. Otherwise (new replica, too far behind or a
// different database) the primary first sends a snapshot of the whole
// database — taken online, without stopping writes — followed by the commits
// made during the copy, and then keeps streaming new commits. Replicas are
// read-only and can serve queries, which allows read scale-out.
package replication

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"time"

	"github.com/minaryhub/bangdb/internal/engine"
	"github.com/minaryhub/bangdb/internal/pager"
)

// Wire protocol (all integers big-endian):
//
//	replica -> primary  hello:   "BREP" ver(1) dbid(16) seq(8) term(8) tokenLen(2) token
//	primary -> replica  status:  0 = ok, 1 = unauthorized, 2 = bad request,
//	                             3 = stale primary (the replica has a newer term)
//	                    then, if ok: term(8)
//	primary -> replica  messages, each starting with a type byte:
//	  'S' dbid(16) baseSeq(8) pageCount(4)        snapshot begins
//	  'P' pgno(4) page(4096) crc(4)               snapshot page
//	  'E' endSeq(8)                               snapshot pages done
//	  'F' seq(8) unixNano(8) n(4) n*(pgno(4) page(4096)) crc(4)  committed transaction
//	  'H' primarySeq(8)                           heartbeat
//	replica -> primary  acknowledgements:
//	  'A' seq(8)                                  applied up to seq
//
// The same frame encoding is used by archive segments (see Archiver).
const (
	helloMagic = "BREP"
	version    = 2

	statusOK           = 0
	statusUnauthorized = 1
	statusBadRequest   = 2
	statusStale        = 3

	msgSnapshot  = 'S'
	msgPage      = 'P'
	msgSnapEnd   = 'E'
	msgFrame     = 'F'
	msgHeartbeat = 'H'
	msgAck       = 'A'

	maxFramePages = 1 << 20
)

var errChecksum = errors.New("replication: checksum mismatch")

type hello struct {
	dbid  [16]byte
	seq   uint64
	term  uint64
	token string
}

func writeHello(w io.Writer, h hello) error {
	b := []byte(helloMagic)
	b = append(b, version)
	b = append(b, h.dbid[:]...)
	b = binary.BigEndian.AppendUint64(b, h.seq)
	b = binary.BigEndian.AppendUint64(b, h.term)
	b = binary.BigEndian.AppendUint16(b, uint16(len(h.token)))
	b = append(b, h.token...)
	_, err := w.Write(b)
	return err
}

func readHello(r io.Reader) (hello, error) {
	var h hello
	head := make([]byte, 4+1+16+8+8+2)
	if _, err := io.ReadFull(r, head[:5]); err != nil {
		return h, err
	}
	if string(head[:4]) != helloMagic || head[4] != version {
		return h, errors.New("replication: bad hello (or a replica of another version)")
	}
	if _, err := io.ReadFull(r, head[5:]); err != nil {
		return h, err
	}
	copy(h.dbid[:], head[5:21])
	h.seq = binary.BigEndian.Uint64(head[21:29])
	h.term = binary.BigEndian.Uint64(head[29:37])
	tok := make([]byte, binary.BigEndian.Uint16(head[37:39]))
	if _, err := io.ReadFull(r, tok); err != nil {
		return h, err
	}
	h.token = string(tok)
	return h, nil
}

func writeSnapshotBegin(w *bufio.Writer, info engine.SnapshotInfo) error {
	w.WriteByte(msgSnapshot)
	w.Write(info.DBID[:])
	writeU64(w, info.BaseSeq)
	return writeU32(w, info.PageCount)
}

func writePage(w *bufio.Writer, no uint32, data []byte) error {
	crc := crc32.NewIEEE()
	var nb [4]byte
	binary.BigEndian.PutUint32(nb[:], no)
	crc.Write(nb[:])
	crc.Write(data)
	w.WriteByte(msgPage)
	w.Write(nb[:])
	w.Write(data)
	return writeU32(w, crc.Sum32())
}

func writeFrame(w *bufio.Writer, f engine.Frame) error {
	w.WriteByte(msgFrame)
	writeU64(w, f.Seq)
	var ts [8]byte
	if !f.Time.IsZero() {
		binary.BigEndian.PutUint64(ts[:], uint64(f.Time.UnixNano()))
	}
	w.Write(ts[:])
	writeU32(w, uint32(len(f.Pages)))
	crc := crc32.NewIEEE()
	crc.Write(ts[:])
	var nb [4]byte
	for _, p := range f.Pages {
		binary.BigEndian.PutUint32(nb[:], p.No)
		crc.Write(nb[:])
		crc.Write(p.Data)
		w.Write(nb[:])
		w.Write(p.Data)
	}
	return writeU32(w, crc.Sum32())
}

func writeU64(w *bufio.Writer, v uint64) error {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func writeU32(w *bufio.Writer, v uint32) error {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	_, err := w.Write(b[:])
	return err
}

func readU64(r io.Reader) (uint64, error) {
	var b [8]byte
	_, err := io.ReadFull(r, b[:])
	return binary.BigEndian.Uint64(b[:]), err
}

func readU32(r io.Reader) (uint32, error) {
	var b [4]byte
	_, err := io.ReadFull(r, b[:])
	return binary.BigEndian.Uint32(b[:]), err
}

func readPage(r io.Reader) (uint32, []byte, error) {
	buf := make([]byte, 4+pager.PageSize)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, nil, err
	}
	sum, err := readU32(r)
	if err != nil {
		return 0, nil, err
	}
	if crc32.ChecksumIEEE(buf) != sum {
		return 0, nil, errChecksum
	}
	return binary.BigEndian.Uint32(buf), buf[4:], nil
}

func readFrame(r io.Reader) (engine.Frame, error) {
	var f engine.Frame
	var err error
	if f.Seq, err = readU64(r); err != nil {
		return f, err
	}
	var ts [8]byte
	if _, err := io.ReadFull(r, ts[:]); err != nil {
		return f, err
	}
	if t := binary.BigEndian.Uint64(ts[:]); t != 0 {
		f.Time = time.Unix(0, int64(t))
	}
	n, err := readU32(r)
	if err != nil {
		return f, err
	}
	if n == 0 || n > maxFramePages {
		return f, fmt.Errorf("replication: invalid frame size %d", n)
	}
	crc := crc32.NewIEEE()
	crc.Write(ts[:])
	for i := uint32(0); i < n; i++ {
		buf := make([]byte, 4+pager.PageSize)
		if _, err := io.ReadFull(r, buf); err != nil {
			return f, err
		}
		crc.Write(buf)
		f.Pages = append(f.Pages, engine.PageImage{No: binary.BigEndian.Uint32(buf), Data: buf[4:]})
	}
	sum, err := readU32(r)
	if err != nil {
		return f, err
	}
	if sum != crc.Sum32() {
		return f, errChecksum
	}
	return f, nil
}
