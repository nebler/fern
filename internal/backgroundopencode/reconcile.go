package backgroundopencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"

	"github.com/nebler/fern/internal/jsoncanon"
)

// ReconcileSession is read-only and compares the complete caller-selected
// ownership tuple. A same-ID session with any different stable field conflicts.
func (c *Client) ReconcileSession(ctx context.Context, spec SessionSpec) (ReconcileState, error) {
	if !validSessionSpec(spec) {
		return ReconcileUncertain, ErrInvalidConfig
	}
	info, err := c.ReadSession(ctx, spec.ID)
	if errors.Is(err, ErrNotFound) {
		return ReconcileAbsent, nil
	}
	if err != nil {
		return ReconcileUncertain, err
	}
	if validateSession(info, spec, "reconcile session") != nil {
		return ReconcileConflict, nil
	}
	return ReconcileExact, nil
}

type promptRecord struct {
	seq  int64
	data promptEventData
}

var durableEventVersions = map[string]int64{
	"session.next.agent.switched":     1,
	"session.next.model.switched":     1,
	"session.next.moved":              1,
	"session.next.prompted":           1,
	"session.next.prompt.admitted":    1,
	"session.next.context.updated":    1,
	"session.next.synthetic":          1,
	"session.next.shell.started":      1,
	"session.next.shell.ended":        1,
	"session.next.step.started":       1,
	"session.next.step.ended":         2,
	"session.next.step.failed":        2,
	"session.next.text.started":       1,
	"session.next.text.ended":         1,
	"session.next.reasoning.started":  1,
	"session.next.reasoning.ended":    1,
	"session.next.tool.input.started": 1,
	"session.next.tool.input.ended":   1,
	"session.next.tool.called":        1,
	"session.next.tool.progress":      1,
	"session.next.tool.success":       1,
	"session.next.tool.failed":        1,
	"session.next.retried":            1,
	"session.next.compaction.started": 1,
	"session.next.compaction.ended":   1,
	"session.next.revert.staged":      1,
	"session.next.revert.cleared":     1,
	"session.next.revert.committed":   1,
}

// ReconcilePrompt exhausts only finite durable history. ReconcileExact means
// one exact admission and the exact promotion semantics requested by Resume.
// ReconcileAdmitted means Resume was requested but no promotion is durable yet.
func (c *Client) ReconcilePrompt(ctx context.Context, sessionID string, spec PromptSpec, bounds HistoryBounds) (ReconcileState, error) {
	if !validSessionID(sessionID) || !validPromptSpec(spec) || bounds.PageLimit < 1 || bounds.PageLimit > maxPageLimit || bounds.MaxPages < 1 || bounds.MaxPages > maxScanPages || bounds.MaxEvents < 1 || bounds.MaxEvents > maxScanEvents {
		return ReconcileUncertain, ErrInvalidConfig
	}
	after := int64(0)
	events := 0
	seenSeq := make(map[int64]struct{})
	seenEventIDs := make(map[string]struct{})
	admissions := make(map[string]promptRecord)
	promotions := make(map[string]promptRecord)
	targetConflict := false
	for pageNumber := 0; pageNumber < bounds.MaxPages; pageNumber++ {
		page, err := c.historyPage(ctx, sessionID, after, bounds.PageLimit)
		if errors.Is(err, ErrNotFound) {
			if pageNumber == 0 {
				return ReconcileAbsent, nil
			}
			return ReconcileUncertain, err
		}
		if err != nil {
			return ReconcileUncertain, err
		}
		if len(page.Data) == 0 {
			if *page.HasMore {
				return ReconcileUncertain, protocol("reconcile prompt", "empty history page claims continuation")
			}
			return reconcilePromptRecords(sessionID, spec, admissions, promotions, targetConflict)
		}
		for _, event := range page.Data {
			if event.Durable == nil || event.Durable.Seq == nil || event.Durable.Version == nil || *event.Durable.Seq <= after || event.Durable.AggregateID != sessionID || !validPrefixedID(event.ID, "evt_") || len(event.Data) == 0 {
				return ReconcileUncertain, protocol("reconcile prompt", "durable event identity or sequence")
			}
			wantVersion, known := durableEventVersions[event.Type]
			if !known || *event.Durable.Version != wantVersion {
				return ReconcileUncertain, protocol("reconcile prompt", "durable event type or version")
			}
			if _, duplicate := seenSeq[*event.Durable.Seq]; duplicate {
				return ReconcileUncertain, protocol("reconcile prompt", "duplicate durable sequence")
			}
			if _, duplicate := seenEventIDs[event.ID]; duplicate {
				return ReconcileUncertain, protocol("reconcile prompt", "duplicate durable event identity")
			}
			var base struct {
				Timestamp *float64 `json:"timestamp"`
				SessionID string   `json:"sessionID"`
			}
			if jsoncanon.Check(event.Data, maxJSONDepth) != nil {
				return ReconcileUncertain, protocol("reconcile prompt", "invalid durable event data")
			}
			if err := json.Unmarshal(event.Data, &base); err != nil || base.Timestamp == nil || !validTimestamp(*base.Timestamp) || base.SessionID != sessionID {
				return ReconcileUncertain, protocol("reconcile prompt", "durable event timestamp or ownership")
			}
			seenSeq[*event.Durable.Seq] = struct{}{}
			seenEventIDs[event.ID] = struct{}{}
			after = *event.Durable.Seq
			events++
			if events > bounds.MaxEvents {
				return ReconcileUncertain, ErrScanBound
			}
			if event.Type != "session.next.prompt.admitted" && event.Type != "session.next.prompted" {
				continue
			}
			var data promptEventData
			if err := strictDecode(event.Data, &data, "reconcile prompt event"); err != nil {
				return ReconcileUncertain, err
			}
			if !validPromptEvent(data, sessionID) {
				return ReconcileUncertain, protocol("reconcile prompt", "invalid prompt event")
			}
			if data.MessageID == spec.ID && !promptMatches(data, sessionID, spec) {
				targetConflict = true
			}
			records := admissions
			if event.Type == "session.next.prompted" {
				records = promotions
				admitted, exists := admissions[data.MessageID]
				if !exists {
					return ReconcileUncertain, protocol("reconcile prompt", "promotion has no earlier admission")
				}
				if !samePromptIdentity(admitted.data, data) {
					if data.MessageID == spec.ID {
						targetConflict = true
					} else {
						return ReconcileUncertain, protocol("reconcile prompt", "unrelated promotion conflicts with admission")
					}
				}
			}
			if prior, duplicate := records[data.MessageID]; duplicate {
				if data.MessageID == spec.ID && (!promptMatches(prior.data, sessionID, spec) || !promptMatches(data, sessionID, spec)) {
					targetConflict = true
					continue
				}
				return ReconcileUncertain, protocol("reconcile prompt", "duplicate prompt event identity")
			}
			records[data.MessageID] = promptRecord{seq: after, data: data}
		}
		if !*page.HasMore {
			return reconcilePromptRecords(sessionID, spec, admissions, promotions, targetConflict)
		}
	}
	return ReconcileUncertain, ErrScanBound
}

func reconcilePromptRecords(sessionID string, spec PromptSpec, admissions, promotions map[string]promptRecord, targetConflict bool) (ReconcileState, error) {
	if targetConflict {
		return ReconcileConflict, nil
	}
	admitted, hasAdmission := admissions[spec.ID]
	promoted, hasPromotion := promotions[spec.ID]
	if !hasAdmission {
		if hasPromotion {
			return ReconcileUncertain, protocol("reconcile prompt", "promotion has no admission")
		}
		return ReconcileAbsent, nil
	}
	if !promptMatches(admitted.data, sessionID, spec) {
		return ReconcileConflict, nil
	}
	if hasPromotion && !promptMatches(promoted.data, sessionID, spec) {
		return ReconcileConflict, nil
	}
	if hasPromotion && promoted.seq <= admitted.seq {
		return ReconcileUncertain, protocol("reconcile prompt", "promotion does not follow admission")
	}
	if !spec.Resume {
		return ReconcileExact, nil
	}
	if !hasPromotion {
		return ReconcileAdmitted, nil
	}
	return ReconcileExact, nil
}

func validPromptEvent(data promptEventData, sessionID string) bool {
	return validMessageID(data.MessageID) && data.Timestamp != nil && validTimestamp(*data.Timestamp) && data.SessionID == sessionID &&
		(data.Delivery == "steer" || data.Delivery == "queue") && len(data.Prompt.Text) <= maxPromptBytes &&
		len(data.Prompt.Files) <= 100 && len(data.Prompt.Agents) <= 100
}

func promptMatches(data promptEventData, sessionID string, spec PromptSpec) bool {
	return data.SessionID == sessionID && data.MessageID == spec.ID && data.Prompt.Text == spec.Text && data.Delivery == spec.Delivery &&
		len(data.Prompt.Files) == 0 && len(data.Prompt.Agents) == 0
}

func samePromptIdentity(left, right promptEventData) bool {
	return left.SessionID == right.SessionID && left.MessageID == right.MessageID && left.Prompt.Text == right.Prompt.Text && left.Delivery == right.Delivery &&
		rawMessagesEqual(left.Prompt.Files, right.Prompt.Files) && rawMessagesEqual(left.Prompt.Agents, right.Prompt.Agents)
}

func rawMessagesEqual(left, right []json.RawMessage) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !bytes.Equal(left[index], right[index]) {
			return false
		}
	}
	return true
}

func validTimestamp(value float64) bool {
	return finiteNonnegative(value) && math.Trunc(value) == value && value <= 9_007_199_254_740_991
}

type historyPage struct {
	Data    []durableEvent `json:"data"`
	HasMore *bool          `json:"hasMore"`
}

func (c *Client) historyPage(ctx context.Context, sessionID string, after int64, limit int) (historyPage, error) {
	query := url.Values{"after": {strconv.FormatInt(after, 10)}, "limit": {strconv.Itoa(limit)}}
	var page historyPage
	err := c.json(ctx, http.MethodGet, sessionPath(sessionID)+"/history?"+query.Encode(), nil, http.StatusOK, "read history", statusAuthority{sessionID: sessionID}, &page)
	if err != nil {
		return historyPage{}, err
	}
	if page.Data == nil || page.HasMore == nil || len(page.Data) > limit {
		return historyPage{}, protocol("read history", "page exceeds requested finite limit")
	}
	return page, nil
}
