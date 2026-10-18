package api

import (
	"errors"
	"net/http"
	"time"
)

var ErrBadAppPairingCode = errors.New("api: pairing code expired, used or incorrect")

type appPairingRedeemer interface {
	RedeemPairingCode(code, lanHint string) (string, time.Time, error)
}

// The existing pairing route supplies the local-address, JSON, browser-origin
// and house-password checks. A claim never runs through an app session and
// cannot choose a role: the box keeps the role of the code it issued.
func (s *Server) handlePairingClaim(w http.ResponseWriter, r *http.Request, code string) {
	if appLinkOverSession(r) {
		writeAppLinkError(w, http.StatusForbidden, "enter a pairing code while on your home network")
		return
	}
	redeemer, ok := s.deps.AppEnroll.(appPairingRedeemer)
	if !ok {
		writeAppLinkError(w, http.StatusServiceUnavailable, "update the box to pair with a typed code")
		return
	}
	url, expires, err := redeemer.RedeemPairingCode(code, s.appLinkLANHint(r))
	if errors.Is(err, ErrBadAppPairingCode) {
		writeAppLinkError(w, http.StatusBadRequest, "that code has expired, was used or is incorrect; make a new code in Settings → FTW app")
		return
	}
	if err != nil {
		writeAppLinkError(w, http.StatusInternalServerError, "could not read the pairing code; make a new one and try again")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, appLinkPairing{URL: url, ExpiresAtMs: expires.UnixMilli()})
}
