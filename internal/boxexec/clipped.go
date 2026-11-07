package boxexec

import "fmt"

// Clipped keeps the start and the end of what is written to it, whatever its
// length, and counts what it leaves out in between. A command can print far
// more than anything holding its output has memory for. It is not safe for
// concurrent use.
type Clipped struct {
	half       int
	head, tail []byte
	total      int
}

// NewClipped returns a Clipped that keeps at most limit bytes.
func NewClipped(limit int) *Clipped { return &Clipped{half: limit / 2} }

// Write never fails.
func (c *Clipped) Write(p []byte) (int, error) {
	n := len(p)
	c.total += n
	if room := c.half - len(c.head); room > 0 {
		room = min(room, len(p))
		c.head = append(c.head, p[:room]...)
		p = p[room:]
	}
	// Past half the tail is what will be thrown away, so most writes copy once.
	if len(p) >= c.half {
		c.tail = append(c.tail[:0], p[len(p)-c.half:]...)
		return n, nil
	}
	c.tail = append(c.tail, p...)
	if len(c.tail) >= 2*c.half {
		c.tail = append(c.tail[:0], c.tail[len(c.tail)-c.half:]...)
	}
	return n, nil
}

// String returns what was written, with a note in place of what was left out.
func (c *Clipped) String() string {
	tail := c.tail
	if len(tail) > c.half {
		tail = tail[len(tail)-c.half:]
	}
	left := c.total - len(c.head) - len(tail)
	if left == 0 {
		return string(c.head) + string(tail)
	}
	return string(c.head) + fmt.Sprintf("\n[… %d bytes left out …]\n", left) + string(tail)
}
