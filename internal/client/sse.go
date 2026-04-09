package client

import (
	"encoding/json"

	"github.com/cascadecodes/banya-cli/pkg/protocol"
)

// ParseEventData extracts a typed payload from a generic ServerEvent.
// The caller should switch on evt.Type and call the appropriate extractor.
func ParseEventData(evt protocol.ServerEvent) (any, error) {
	raw, err := json.Marshal(evt.Data)
	if err != nil {
		return nil, err
	}

	switch evt.Type {
	case protocol.EventContentDelta, protocol.EventContentDone:
		var d protocol.ContentDelta
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		return d, nil

	case protocol.EventToolCallStart, protocol.EventToolCallDelta, protocol.EventToolCallDone:
		var tc protocol.ToolCall
		if err := json.Unmarshal(raw, &tc); err != nil {
			return nil, err
		}
		return tc, nil

	case protocol.EventApprovalNeeded:
		var ar protocol.ApprovalRequest
		if err := json.Unmarshal(raw, &ar); err != nil {
			return nil, err
		}
		return ar, nil

	case protocol.EventError:
		var e protocol.ErrorData
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, err
		}
		return e, nil

	default:
		return evt.Data, nil
	}
}
