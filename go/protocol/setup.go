package protocol

import (
	"encoding/json"
	"errors"
)

// SetupCall carries a bounded JSON request to a named setup operation. It
// cannot choose an HTTP route, method, headers or another user's identity.
type SetupCall struct {
	JSON []byte `cbor:"json,omitempty"`
}

// A patch fits one padded bulk envelope. JSON Patch operations edit a list
// entry without resending siblings; the box holds the complete draft.
const SetupRequestBytes = 10 << 10

func DecodeSetupCall(raw []byte) (SetupCall, error) {
	var call SetupCall
	if err := strictDec.Unmarshal(raw, &call); err != nil {
		return call, err
	}
	if len(call.JSON) > SetupRequestBytes || len(call.JSON) > 0 && !json.Valid(call.JSON) {
		return call, errors.New("invalid setup request")
	}
	return call, nil
}
