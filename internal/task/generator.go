package task

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
	"time"
)

const maxUUIDv7Milliseconds = uint64(1<<48 - 1)

// Generator creates Fern UUIDv7 IDs and 128-bit OpenCode IDs. It serializes
// entropy reads and preserves UUID ordering when the clock stalls or regresses.
type Generator struct {
	mu       sync.Mutex
	random   io.Reader
	now      func() time.Time
	lastUUID [16]byte
	haveLast bool
}

type AdmissionIDs struct {
	TaskID            TaskID
	AttemptID         AttemptID
	ReceiptID         ReceiptID
	TaskEventID       EventID
	AttemptEventID    EventID
	OpenCodeSessionID OpenCodeSessionID
	OpenCodeMessageID OpenCodeMessageID
}

// SealRequestIDs is allocated before admitting a user-authorized seal so every
// identity used after a restart is durable before collection starts.
type SealRequestIDs struct {
	SealRequestID SealRequestID
	ReceiptID     ReceiptID
	ResultID      ResultID
	ResultEventID EventID
	TaskEventID   EventID
}

func NewGenerator(random io.Reader, now func() time.Time) (*Generator, error) {
	if random == nil || now == nil {
		return nil, ErrIDGeneration
	}
	return &Generator{random: random, now: now}, nil
}

func NewSecureGenerator() *Generator {
	return &Generator{random: rand.Reader, now: time.Now}
}

// GenerateAdmissionIDs allocates the complete identity set that must exist
// before durable admission. It returns a zero set if any allocation fails.
func (g *Generator) GenerateAdmissionIDs() (AdmissionIDs, error) {
	var ids AdmissionIDs
	var err error
	if ids.TaskID, err = g.TaskID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.AttemptID, err = g.AttemptID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.ReceiptID, err = g.ReceiptID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.TaskEventID, err = g.EventID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.AttemptEventID, err = g.EventID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.OpenCodeSessionID, err = g.OpenCodeSessionID(); err != nil {
		return AdmissionIDs{}, err
	}
	if ids.OpenCodeMessageID, err = g.OpenCodeMessageID(); err != nil {
		return AdmissionIDs{}, err
	}
	return ids, nil
}

func (g *Generator) GenerateSealRequestIDs() (SealRequestIDs, error) {
	var ids SealRequestIDs
	var err error
	if ids.SealRequestID, err = g.SealRequestID(); err != nil {
		return SealRequestIDs{}, err
	}
	if ids.ReceiptID, err = g.ReceiptID(); err != nil {
		return SealRequestIDs{}, err
	}
	if ids.ResultID, err = g.ResultID(); err != nil {
		return SealRequestIDs{}, err
	}
	if ids.ResultEventID, err = g.EventID(); err != nil {
		return SealRequestIDs{}, err
	}
	if ids.TaskEventID, err = g.EventID(); err != nil {
		return SealRequestIDs{}, err
	}
	return ids, nil
}

func (g *Generator) WorkspaceID() (WorkspaceID, error) {
	value, err := g.fernID("wsp_")
	return WorkspaceID(value), err
}

func (g *Generator) TaskID() (TaskID, error) {
	value, err := g.fernID("tsk_")
	return TaskID(value), err
}

func (g *Generator) AttemptID() (AttemptID, error) {
	value, err := g.fernID("att_")
	return AttemptID(value), err
}

func (g *Generator) ReceiptID() (ReceiptID, error) {
	value, err := g.fernID("rcp_")
	return ReceiptID(value), err
}

func (g *Generator) EventID() (EventID, error) {
	value, err := g.fernID("fev_")
	return EventID(value), err
}

func (g *Generator) ApprovalID() (ApprovalID, error) {
	value, err := g.fernID("apr_")
	return ApprovalID(value), err
}

func (g *Generator) SealRequestID() (SealRequestID, error) {
	value, err := g.fernID("slr_")
	return SealRequestID(value), err
}

func (g *Generator) ResultID() (ResultID, error) {
	value, err := g.fernID("res_")
	return ResultID(value), err
}

func (g *Generator) VerificationID() (VerificationID, error) {
	value, err := g.fernID("ver_")
	return VerificationID(value), err
}

func (g *Generator) PublicationID() (PublicationID, error) {
	value, err := g.fernID("pub_")
	return PublicationID(value), err
}

func (g *Generator) PublicationOperationID() (PublicationOperationID, error) {
	value, err := g.fernID("op_")
	return PublicationOperationID(value), err
}

func (g *Generator) OpenCodeSessionID() (OpenCodeSessionID, error) {
	value, err := g.openCodeID("ses_")
	return OpenCodeSessionID(value), err
}

func (g *Generator) OpenCodeMessageID() (OpenCodeMessageID, error) {
	value, err := g.openCodeID("msg_")
	return OpenCodeMessageID(value), err
}

func (g *Generator) fernID(prefix string) (string, error) {
	if g == nil || g.random == nil || g.now == nil {
		return "", ErrIDGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if now.UnixMilli() < 0 || uint64(now.UnixMilli()) > maxUUIDv7Milliseconds {
		return "", fmt.Errorf("%w: clock outside UUIDv7 range", ErrIDGeneration)
	}
	milliseconds := uint64(now.UnixMilli())
	var uuid [16]byte
	if !g.haveLast || milliseconds > uuidMilliseconds(g.lastUUID) {
		if _, err := io.ReadFull(g.random, uuid[6:]); err != nil {
			return "", fmt.Errorf("%w: entropy unavailable", ErrIDGeneration)
		}
		setUUIDMilliseconds(&uuid, milliseconds)
		setUUIDv7Bits(&uuid)
	} else {
		uuid = g.lastUUID
		if !incrementUUIDv7Random(&uuid) {
			milliseconds = uuidMilliseconds(g.lastUUID) + 1
			if milliseconds > maxUUIDv7Milliseconds {
				return "", fmt.Errorf("%w: UUIDv7 range exhausted", ErrIDGeneration)
			}
			if _, err := io.ReadFull(g.random, uuid[6:]); err != nil {
				return "", fmt.Errorf("%w: entropy unavailable", ErrIDGeneration)
			}
			setUUIDMilliseconds(&uuid, milliseconds)
			setUUIDv7Bits(&uuid)
		}
	}
	g.lastUUID = uuid
	g.haveLast = true
	return prefix + formatUUID(uuid), nil
}

func (g *Generator) openCodeID(prefix string) (string, error) {
	if g == nil || g.random == nil {
		return "", ErrIDGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	var value [16]byte
	if _, err := io.ReadFull(g.random, value[:]); err != nil {
		return "", fmt.Errorf("%w: entropy unavailable", ErrIDGeneration)
	}
	return prefix + hex.EncodeToString(value[:]), nil
}

func setUUIDMilliseconds(uuid *[16]byte, milliseconds uint64) {
	for index := 5; index >= 0; index-- {
		uuid[index] = byte(milliseconds)
		milliseconds >>= 8
	}
}

func uuidMilliseconds(uuid [16]byte) uint64 {
	var milliseconds uint64
	for index := 0; index < 6; index++ {
		milliseconds = milliseconds<<8 | uint64(uuid[index])
	}
	return milliseconds
}

func setUUIDv7Bits(uuid *[16]byte) {
	uuid[6] = uuid[6]&0x0f | 0x70
	uuid[8] = uuid[8]&0x3f | 0x80
}

func incrementUUIDv7Random(uuid *[16]byte) bool {
	for index := 15; index >= 9; index-- {
		uuid[index]++
		if uuid[index] != 0 {
			return true
		}
	}
	low := (uuid[8] & 0x3f) + 1
	uuid[8] = 0x80 | low&0x3f
	if low <= 0x3f {
		return true
	}
	uuid[7]++
	if uuid[7] != 0 {
		return true
	}
	low = (uuid[6] & 0x0f) + 1
	uuid[6] = 0x70 | low&0x0f
	return low <= 0x0f
}

func formatUUID(uuid [16]byte) string {
	compact := hex.EncodeToString(uuid[:])
	return compact[:8] + "-" + compact[8:12] + "-" + compact[12:16] + "-" + compact[16:20] + "-" + compact[20:]
}
