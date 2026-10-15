package appuplink

import (
	"time"

	"github.com/srcfl/ftw/go/internal/apiauth"
	"github.com/srcfl/ftw/go/internal/appenroll"
	"github.com/srcfl/ftw/go/internal/appproto"
	"github.com/srcfl/ftw/go/internal/cloudlink"
)

// CloudRoster is enrolment as FTW Cloud's roster reads it: every paired
// device with the scopes the box derives for it, a phone's role, or for a
// watch what its role and its phone's role share. With Tunnel set, a phone
// may open a session through FTW Cloud and the QR code on the screen is
// listed; a watch has no stream and reaches the box through its exchange.
type CloudRoster struct {
	Enroll *appenroll.Identity
	Tunnel bool
}

// Devices lists the paired devices.
func (r CloudRoster) Devices() []cloudlink.RosterDevice {
	var out []cloudlink.RosterDevice
	for _, e := range r.Enroll.Roster() {
		out = append(out, cloudlink.RosterDevice{ID: e.ID, Key: e.Key, Role: e.Role, Epoch: e.Epoch,
			Scopes: appproto.GrantScopes(e.Role, e.ParentRole), Tun: r.Tunnel && e.Role != apiauth.RoleWatch})
	}
	return out
}

// Pending is the QR code on the screen, while the box takes the tunnel: a
// new phone can then pair through FTW Cloud.
func (r CloudRoster) Pending() ([]byte, time.Time, bool) {
	if !r.Tunnel {
		return nil, time.Time{}, false
	}
	return r.Enroll.PendingCode()
}
