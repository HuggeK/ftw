package cloudwire

import (
	"strconv"
	"strings"
)

// Shards and restarts.
//
// Every home lives on exactly one instance of the service, its shard, and its
// box, phones, watch and extensions all meet there. DefaultHost is where a
// client starts; with one shard it is the only host. An instance that gets a
// connection or a request for a home it does not hold names the host that
// does. The client follows that once, keeps the host for the home, and goes
// back to DefaultHost when the kept host stops answering.
//
// An instance that is about to stop, for a deploy or a move, closes every
// socket with CloseRestart. The reason says over how many milliseconds to
// spread the redial, so the service sets the spread to its own load and no
// client release is needed when it changes.

// DefaultHost is the service's default host.
const DefaultHost = "cloud.ftw.energy"

const (
	// CloseWrongShard closes /v1/box or /v1/app: this instance does not
	// hold the home. The close reason is the host that does. On /v1/box it
	// comes after hello, because the home is the key that signed it; on
	// /v1/app it comes before message 1 is read.
	CloseWrongShard = 4421
	// CloseRestart closes /v1/box or /v1/app when the service is about to
	// stop. The close reason is the spread in milliseconds, in decimal. The
	// client redials at a uniformly random moment within the spread and
	// does not count the close as a failed attempt.
	CloseRestart = 4503
	// StatusWrongShard answers /v1/app/once and /v1/now for a home this
	// instance does not hold: 421 Misdirected Request, with the host that
	// does in ShardHeader and an empty body.
	StatusWrongShard = 421
	// ShardHeader names the host on a StatusWrongShard answer.
	ShardHeader = "Ftw-Shard"
	// CapShard in hello.caps says the box follows CloseWrongShard and
	// spreads its redial after CloseRestart. A second shard holds homes
	// only once their boxes list it.
	CapShard = "shard"

	// RestartSpreadDefaultMs is the spread when a CloseRestart reason is
	// missing or not a number. RestartSpreadMinMs and RestartSpreadMaxMs
	// bound any spread: a reason of 0 must not make every client redial in
	// a tight loop.
	RestartSpreadDefaultMs = 5_000
	RestartSpreadMinMs     = 1_000
	RestartSpreadMaxMs     = 60_000
	// A client forgets a kept host when ShardHostFailures dials in a row
	// to it have failed over at least ShardHostFailWindowMs, and starts
	// again at DefaultHost. The window outlasts a shard's restart.
	ShardHostFailures     = 3
	ShardHostFailWindowMs = 30_000
)

// RestartSpreadMs reads a CloseRestart reason: a whole number of
// milliseconds, from RestartSpreadMinMs to RestartSpreadMaxMs. Anything else
// is the default.
func RestartSpreadMs(reason string) int64 {
	ms, err := strconv.ParseInt(strings.TrimSpace(reason), 10, 64)
	if err != nil || ms < 0 {
		return RestartSpreadDefaultMs
	}
	return min(max(ms, RestartSpreadMinMs), RestartSpreadMaxMs)
}

// ShardHostOK reports whether a client of the service at base may follow a
// wrong-shard answer to host: base itself, or one DNS label of lowercase
// letters, digits and hyphens directly under it, such as s2.cloud.ftw.energy
// under cloud.ftw.energy. Anything else is refused, so a redirect never sends
// a box or a phone off the service's own domain. Neither name carries a port.
func ShardHostOK(base, host string) bool {
	if base == "" || host == "" || strings.ContainsAny(base, ":/") {
		return false
	}
	if host == base {
		return true
	}
	label, ok := strings.CutSuffix(host, "."+base)
	if !ok || label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
		return false
	}
	for i := 0; i < len(label); i++ {
		c := label[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
