package main

import (
	"errors"
	"time"

	"github.com/srcfl/ftw/go/internal/api"
	"github.com/srcfl/ftw/go/internal/appenroll"
)

func (a *appLinkAPI) RedeemPairingCode(typed, lanHint string) (string, time.Time, error) {
	code, expires, err := a.enroll.RedeemSpokenCode(typed)
	if errors.Is(err, appenroll.ErrBadPairing) {
		return "", time.Time{}, api.ErrBadAppPairingCode
	}
	if err != nil {
		return "", time.Time{}, err
	}
	url, err := a.enroll.EnrollmentURL(code, lanHint)
	return url, expires, err
}
