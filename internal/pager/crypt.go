package pager

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

// Encryption at rest. Every page except the header page 0 is encrypted
// with AES-256 in XTS mode (IEEE 1619, as used for disk encryption) where
// it is written to the database file or the WAL, with the page number as
// the tweak; pages are decrypted when read. Pages in memory, page images
// sent to replicas and the checksums are plaintext: the checksum, stored
// inside the encrypted page, also detects a wrong key or tampering.
//
// The header page stays readable so that the file can be identified: it
// records (format version 4) that the file is encrypted and a check value
// derived from the key, so that a wrong key is reported as such. It holds
// no table data (page counts, roots, sequence number, database id).

// ErrEncrypted is returned when an encrypted database is opened without a
// key, and ErrWrongKey when the key does not match.
var (
	ErrEncrypted    = errors.New("bangdb: the database is encrypted; an encryption key is required")
	ErrWrongKey     = errors.New("bangdb: wrong encryption key")
	ErrNotEncrypted = errors.New("bangdb: the database is not encrypted; open it without a key (or make an encrypted copy with Backup)")
)

type pageCipher struct {
	data, tweak cipher.Block
	check       [16]byte
}

func derive(key []byte, label string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(label))
	return m.Sum(nil)
}

func newPageCipher(key []byte) (*pageCipher, error) {
	if len(key) < 16 {
		return nil, errors.New("bangdb: encryption key must be at least 16 bytes (32 random bytes recommended)")
	}
	k1, err := aes.NewCipher(derive(key, "bangdb page data"))
	if err != nil {
		return nil, err
	}
	k2, err := aes.NewCipher(derive(key, "bangdb page tweak"))
	if err != nil {
		return nil, err
	}
	c := &pageCipher{data: k1, tweak: k2}
	copy(c.check[:], derive(key, "bangdb key check"))
	return c, nil
}

// xts encrypts or decrypts src (a multiple of 16 bytes) into dst for the
// given sector.
func xts(data, tweakKey cipher.Block, dst, src []byte, sector uint64, encrypt bool) {
	var t [16]byte
	binary.LittleEndian.PutUint64(t[:8], sector)
	tweakKey.Encrypt(t[:], t[:])
	var x [16]byte
	for i := 0; i+16 <= len(src); i += 16 {
		for j := range x {
			x[j] = src[i+j] ^ t[j]
		}
		if encrypt {
			data.Encrypt(x[:], x[:])
		} else {
			data.Decrypt(x[:], x[:])
		}
		for j := range x {
			dst[i+j] = x[j] ^ t[j]
		}
		// t *= alpha in GF(2^128), little-endian.
		carry := t[15] >> 7
		for j := 15; j > 0; j-- {
			t[j] = t[j]<<1 | t[j-1]>>7
		}
		t[0] = t[0]<<1 ^ 0x87*carry
	}
}

// encrypt returns the on-disk form of page pgno (a new slice, or b itself
// when there is nothing to do).
func (p *Pager) encrypt(pgno uint32, b []byte) []byte {
	if p.crypt == nil || pgno == 0 {
		return b
	}
	out := make([]byte, len(b))
	xts(p.crypt.data, p.crypt.tweak, out, b, uint64(pgno), true)
	return out
}

// decrypt turns a page read from disk into plaintext, in place.
func (p *Pager) decrypt(pgno uint32, b []byte) {
	if p.crypt == nil || pgno == 0 {
		return
	}
	xts(p.crypt.data, p.crypt.tweak, b, b, uint64(pgno), false)
}

// adopt gives a header taken from elsewhere (a primary, a copied file)
// this pager's encryption marker.
func (p *Pager) adopt(h *Header) {
	h.KeyCheck = [16]byte{}
	if p.crypt != nil {
		h.KeyCheck = p.crypt.check
	}
}
