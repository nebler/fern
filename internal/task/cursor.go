package task

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// Cursor is a SQLite-assigned signed 64-bit event cursor. Zero is reserved for
// an initial/empty-page position; persisted events must pass ValidateEvent.
type Cursor int64

func (c Cursor) Validate() error {
	if c < 0 {
		return ErrInvalidCursor
	}
	return nil
}

func (c Cursor) ValidateEvent() error {
	if c <= 0 {
		return ErrInvalidCursor
	}
	return nil
}

func (c Cursor) String() string { return strconv.FormatInt(int64(c), 10) }

func ParseCursorWire(v string) (Cursor, error) {
	if v == "" || (len(v) > 1 && v[0] == '0') {
		return 0, ErrInvalidCursor
	}
	for i := range len(v) {
		if v[i] < '0' || v[i] > '9' {
			return 0, ErrInvalidCursor
		}
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, ErrInvalidCursor
	}
	return Cursor(n), nil
}

// ParseAfterCursor treats an omitted query value as the initial cursor.
func ParseAfterCursor(v string) (Cursor, error) {
	if v == "" {
		return 0, nil
	}
	return ParseCursorWire(v)
}

func (c Cursor) MarshalJSON() ([]byte, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(c.String())
}

func (c *Cursor) UnmarshalJSON(data []byte) error {
	if c == nil {
		return ErrInvalidCursor
	}
	if len(data) < 2 || data[0] != '"' || data[len(data)-1] != '"' || bytes.ContainsRune(data, '\\') {
		return fmt.Errorf("%w: cursor must be a decimal JSON string", ErrInvalidCursor)
	}
	parsed, err := ParseCursorWire(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*c = parsed
	return nil
}
