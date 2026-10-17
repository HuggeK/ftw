package cloudwire

import "errors"

const MaxWebPushRows = 16
const MaxWebPushBytes = 32 * 1024

// ValidateWebPush bounds the service-only part of a roster. The service also
// checks delivery addresses and the current device grant before it sends.
func ValidateWebPush(rows []WebPushRow) error {
	bad := errors.New("cloud: invalid browser notice rows")
	if len(rows) > MaxWebPushRows {
		return bad
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.ID == "" || len(r.ID) > 128 || seen[r.ID] || r.Device == "" || len(r.Device) > 128 || r.Epoch == 0 || len(r.Endpoint) > 2048 || len(r.Auth) > 512 || len(r.CT) == 0 || len(r.CT) > 1024 {
			return bad
		}
		seen[r.ID] = true
	}
	b, err := Marshal(rows)
	if err != nil || len(b) > MaxWebPushBytes {
		return bad
	}
	return nil
}
