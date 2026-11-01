package backup

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// A backup is encrypted before it leaves the node: it holds the threads'
// code and data and T3's state, and the object store is someone else's.
// The stream is AES-256-GCM in chunks of chunkSize, each sealed with a
// nonce of the stream's random prefix and the chunk's number, and the last
// marked as last, so that chunks can't be reordered, dropped or cut off
// unnoticed (the STREAM construction).
const chunkSize = 64 << 10

// KeySize is the length of a backup key.
const KeySize = 32

// ErrKey means the key can't open the backup: it is another's, or the
// backup was changed.
var ErrKey = errors.New("the backup key doesn't open this backup")

// ParseKey reads a key written as hex.
func ParseKey(s string) ([]byte, error) {
	key, err := hex.DecodeString(strings.TrimSpace(s))
	if err != nil || len(key) != KeySize {
		return nil, fmt.Errorf("a backup key is %d bytes of hex", KeySize)
	}
	return key, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// nonceFor returns chunk n's nonce: 7 bytes of prefix, the number, and
// whether it is the last chunk.
func nonceFor(prefix []byte, n uint32, last bool) []byte {
	nonce := make([]byte, 12)
	copy(nonce, prefix[:7])
	binary.BigEndian.PutUint32(nonce[7:11], n)
	if last {
		nonce[11] = 1
	}
	return nonce
}

// chunks reads r in pieces of size bytes, saying which is the last: a
// piece is handed out once the next has begun.
type chunks struct {
	r          io.Reader
	cur, ahead []byte
	n          int  // bytes in cur
	read       bool // cur has been read
	last       bool // cur is the last piece
}

func newChunks(r io.Reader, size int) *chunks {
	return &chunks{r: r, cur: make([]byte, size), ahead: make([]byte, size)}
}

// next returns the next piece and whether it is the last; io.EOF after it.
func (c *chunks) next() ([]byte, bool, error) {
	if c.last {
		return nil, false, io.EOF
	}
	if !c.read {
		var err error
		if c.n, err = full(c.r, c.cur); err != nil {
			return nil, false, err
		}
		c.read = true
	}
	m, err := full(c.r, c.ahead)
	if err != nil {
		return nil, false, err
	}
	// A short piece, or nothing after this one: this one is the last.
	c.last = c.n < len(c.cur) || m == 0
	piece := c.cur[:c.n]
	if !c.last {
		c.cur, c.ahead, c.n = c.ahead, c.cur, m
		return c.ahead[:len(piece)], false, nil
	}
	return piece, true, nil
}

// full reads as much of buf as r has, up to its end.
func full(r io.Reader, buf []byte) (int, error) {
	n, err := io.ReadFull(r, buf)
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return n, nil
	}
	return n, err
}

// sealer is Encrypt's reader.
type sealer struct {
	aead    cipher.AEAD
	prefix  []byte
	chunks  *chunks
	i       uint32
	pending []byte // what is sealed and not yet read
}

// Encrypt returns a reader of r encrypted with key.
func Encrypt(key []byte, r io.Reader) (io.Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	prefix := make([]byte, 7)
	if _, err := rand.Read(prefix); err != nil {
		return nil, err
	}
	return &sealer{aead: aead, prefix: prefix, chunks: newChunks(r, chunkSize), pending: slices.Clone(prefix)}, nil
}

func (s *sealer) Read(p []byte) (int, error) {
	if len(s.pending) == 0 {
		chunk, last, err := s.chunks.next()
		if err != nil {
			return 0, err
		}
		s.pending = s.aead.Seal(nil, nonceFor(s.prefix, s.i, last), chunk, nil)
		s.i++
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

// opener is Decrypt's reader.
type opener struct {
	aead    cipher.AEAD
	r       io.Reader
	prefix  []byte
	chunks  *chunks
	i       uint32
	pending []byte
}

// Decrypt returns a reader of what Encrypt made from r with key. A read
// fails with ErrKey if a chunk doesn't open, is missing, or the stream was
// cut short.
func Decrypt(key []byte, r io.Reader) (io.Reader, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	return &opener{aead: aead, r: r}, nil
}

func (o *opener) Read(p []byte) (int, error) {
	if o.prefix == nil {
		o.prefix = make([]byte, 7)
		if _, err := io.ReadFull(o.r, o.prefix); err != nil {
			return 0, ErrKey
		}
		o.chunks = newChunks(o.r, chunkSize+o.aead.Overhead())
	}
	if len(o.pending) == 0 {
		chunk, last, err := o.chunks.next()
		if errors.Is(err, io.EOF) {
			return 0, io.EOF
		}
		if err != nil {
			return 0, err
		}
		plain, err := o.aead.Open(nil, nonceFor(o.prefix, o.i, last), chunk, nil)
		if err != nil {
			return 0, ErrKey
		}
		o.pending = plain
		o.i++
		if len(plain) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, o.pending)
	o.pending = o.pending[n:]
	return n, nil
}
