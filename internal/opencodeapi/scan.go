package opencodeapi

import (
	"bytes"
	"context"
)

type ScanOptions struct {
	PageLimit  int
	MaxPages   int
	MaxEntries int
}

// ScanMessages exhausts the finite ascending projection. It does not infer
// completeness from the volatile /api/event stream and never retries a call.
func (client *Client) ScanMessages(ctx context.Context, sessionID string, options ScanOptions) ([]Message, error) {
	if options.PageLimit < 1 || options.PageLimit > maxPageLimit || options.MaxPages < 1 || options.MaxPages > maxScanPages || options.MaxEntries < 1 || options.MaxEntries > maxListEntries {
		return nil, ErrInvalidConfiguration
	}
	seenCursors := map[string]struct{}{"": {}}
	seenMessages := make(map[string][]byte)
	result := make([]Message, 0)
	cursor := ""
	var previousCreated int64
	havePrevious := false
	for pageNumber := 0; pageNumber < options.MaxPages; pageNumber++ {
		if ctx == nil {
			return nil, ErrDeadlineRequired
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		page, err := client.Messages(ctx, sessionID, cursor, options.PageLimit)
		if err != nil {
			return nil, err
		}
		for _, message := range page.Data {
			if havePrevious && message.Time.Created < previousCreated {
				return nil, protocolError("message order is not ascending")
			}
			havePrevious = true
			previousCreated = message.Time.Created
			wire := message.Bytes()
			if prior, ok := seenMessages[message.ID]; ok {
				if !bytes.Equal(prior, wire) {
					return nil, protocolError("duplicate message identity has incompatible bytes")
				}
				return nil, protocolError("duplicate message identity")
			}
			seenMessages[message.ID] = wire
			if len(result) == options.MaxEntries {
				return nil, ErrScanLimit
			}
			result = append(result, message)
		}
		if page.NextCursor == nil {
			return result, nil
		}
		next := *page.NextCursor
		if _, exists := seenCursors[next]; exists {
			return nil, protocolError("message cursor repeated")
		}
		seenCursors[next] = struct{}{}
		cursor = next
	}
	return nil, ErrScanLimit
}
