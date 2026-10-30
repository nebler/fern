package watch

import (
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// event is one fanotify event of a group that reports folders' file
// handles and names (FAN_REPORT_DFID_NAME).
type event struct {
	mask   uint64
	pid    int
	handle string // the folder's file handle (handleKey)
	name   string // the entry in it; "" or "." for the folder itself
}

// The layout of what the kernel reads out (linux/fanotify.h): each event is
// a fanotify_event_metadata, then info records. A file handle record is a
// header (type, padding, length), the filesystem's ID, then a file_handle
// (its length, its type, the handle) and, for DFID_NAME, the name ending in
// a NUL.
const (
	metadataLen  = 24 // event_len, vers, reserved, metadata_len, mask, fd, pid
	infoHeader   = 4
	fsidLen      = 8
	handleHeader = 8
)

var errShort = errors.New("short fanotify event")

// decode returns the events in buf, as one read returned them.
func decode(buf []byte) ([]event, error) {
	var evs []event
	for len(buf) > 0 {
		if len(buf) < metadataLen {
			return nil, errShort
		}
		evLen := int(binary.NativeEndian.Uint32(buf[0:]))
		vers := buf[4]
		metaLen := int(binary.NativeEndian.Uint16(buf[6:]))
		if vers != unix.FANOTIFY_METADATA_VERSION {
			return nil, fmt.Errorf("fanotify event version %d, want %d", vers, unix.FANOTIFY_METADATA_VERSION)
		}
		if evLen < metadataLen || metaLen < metadataLen || metaLen > evLen || evLen > len(buf) {
			return nil, errShort
		}
		e := event{
			mask: binary.NativeEndian.Uint64(buf[8:]),
			pid:  int(int32(binary.NativeEndian.Uint32(buf[20:]))),
		}
		if err := e.info(buf[metaLen:evLen]); err != nil {
			return nil, err
		}
		evs = append(evs, e)
		buf = buf[evLen:]
	}
	return evs, nil
}

// info reads the event's folder handle and name from its info records.
func (e *event) info(recs []byte) error {
	for len(recs) > 0 {
		if len(recs) < infoHeader {
			return errShort
		}
		typ := recs[0]
		n := int(binary.NativeEndian.Uint16(recs[2:]))
		if n < infoHeader || n > len(recs) {
			return errShort
		}
		rec := recs[infoHeader:n]
		recs = recs[n:]
		if typ != unix.FAN_EVENT_INFO_TYPE_DFID_NAME && typ != unix.FAN_EVENT_INFO_TYPE_DFID {
			continue
		}
		if len(rec) < fsidLen+handleHeader {
			return errShort
		}
		rec = rec[fsidLen:]
		hLen := int(binary.NativeEndian.Uint32(rec[0:]))
		hType := int32(binary.NativeEndian.Uint32(rec[4:]))
		if hLen > len(rec)-handleHeader {
			return errShort
		}
		e.handle = handleKey(hType, rec[handleHeader:handleHeader+hLen])
		if typ == unix.FAN_EVENT_INFO_TYPE_DFID_NAME {
			name := rec[handleHeader+hLen:]
			for i, c := range name {
				if c == 0 {
					name = name[:i]
					break
				}
			}
			e.name = string(name)
		}
	}
	return nil
}

// handleKey is a file handle as a map key.
func handleKey(typ int32, handle []byte) string {
	return fmt.Sprintf("%d:%x", typ, handle)
}
