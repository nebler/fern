package runtime

import "fmt"

type Protocol string

const (
	ProtocolV1   Protocol = "v1"
	ProtocolV2   Protocol = "v2"
	ProtocolAuto Protocol = "auto"
)

func (protocol Protocol) Normalize() Protocol {
	if protocol == "" {
		return ProtocolV1
	}
	return protocol
}

func (protocol Protocol) Validate() error {
	switch protocol.Normalize() {
	case ProtocolV1, ProtocolV2, ProtocolAuto:
		return nil
	default:
		return fmt.Errorf("invalid OpenCode protocol %q", protocol)
	}
}

func (protocol Protocol) String() string {
	return string(protocol.Normalize())
}
