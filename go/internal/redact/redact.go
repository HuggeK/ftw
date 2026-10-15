// Package redact builds what an agent may read of the box's logs and
// configuration (docs/agents.md, Redaction on the box). Nothing it returns
// may carry a name, an address, a serial, a token or a secret.
//
// It is a list of patterns, and that is its limit: text in a shape no rule
// knows can pass. So it leans the safe way. A value is kept only when it
// looks like a number, a flag, a duration or a short code word; everything
// else in an attribute is dropped, and free text is scrubbed of every shape
// below. A driver that logs something new needs a new case in the tests.
package redact

import (
	"encoding/json"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/srcfl/ftw/go/protocol/cloudwire"
)

// Redactor scrubs text for one box. Names are what its configuration calls
// things (the site, drivers, devices, loadpoints, vehicles); Key cuts opaque
// ids from them, so the same driver has the same id in every answer.
type Redactor struct {
	key   []byte
	names *regexp.Regexp
}

// New builds a redactor. Names shorter than three characters are left out:
// replacing "pv" everywhere would scrub the words, not the names.
func New(key []byte, names []string) *Redactor {
	r := &Redactor{key: append([]byte(nil), key...)}
	var quoted []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if len(n) < 3 || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		quoted = append(quoted, regexp.QuoteMeta(n))
	}
	if len(quoted) > 0 {
		// Longest first, so "Easee carport" goes before "Easee".
		sort.Slice(quoted, func(a, b int) bool { return len(quoted[a]) > len(quoted[b]) })
		r.names = regexp.MustCompile(`(?i)(^|[^\pL\pN_])(` + strings.Join(quoted, "|") + `)($|[^\pL\pN_])`)
	}
	return r
}

// ID is a source's id as agents see it: the readable copy's construction
// (cloudwire.SourceID) under the box's agent key, so the src in a log line is
// the src in live.get. Stable for this key, and useless for finding the name.
func (r *Redactor) ID(name string) string {
	return cloudwire.SourceID(r.key, name)
}

var (
	reBearer = regexp.MustCompile(`(?i)\b(bearer|basic|token)\s+[A-Za-z0-9+/=._~-]+`)
	// A secret-named key followed by a value, in key=value, key: value or
	// "key": "value" form.
	reSecretKV = regexp.MustCompile(`(?i)("?[A-Za-z0-9_.-]*(?:pass|secret|token|apikey|api_key|auth|credential|private|cookie|session)[A-Za-z0-9_.-]*"?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&}]+)`)
	reURL      = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+`)
	reEmail    = regexp.MustCompile(`(?i)[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`)
	reMAC      = regexp.MustCompile(`(?i)\b(?:[0-9a-f]{2}[:-]){5}[0-9a-f]{2}\b`)
	reIPv4     = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d+)?\b`)
	reIPv6     = regexp.MustCompile(`(?i)\[?\b(?:[0-9a-f]{0,4}:){2,7}[0-9a-f]{0,4}\b\]?(?::\d+)?`)
	// A host ends in any two-letter label, every country's, or in a common
	// longer one. A code word such as planner.prefs.set ends in neither.
	reHost   = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:[a-z]{2}|com|net|org|xyz|info|biz|energy|cloud|app|dev|tech|online|site|store|shop|page|link|live|life|world|space|website|pro|name|mobi|asia|top|club|vip|work|fun|icu|one|tools|network|systems|solar|house|family|email|local|lan|home|internal|box|localdomain|arpa|fritz|router|gateway)\b(?::\d+)?`)
	reQuoted = regexp.MustCompile(`"[^"\n]*"|'[^'\n]*'|` + "`[^`\n]*`")
	reToken  = regexp.MustCompile(`[A-Za-z0-9+/=_-]{20,}`)
	reHexRun = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
	reDigits = regexp.MustCompile(`\d{7,}`)
	// A word with letters and digits mixed, six or more: the shape of a
	// serial, a VIN or a device id.
	reWord = regexp.MustCompile(`[A-Za-z0-9_-]{6,}`)
)

// Text scrubs one line of free text.
func (r *Redactor) Text(s string) string {
	if s == "" {
		return s
	}
	if r.names != nil {
		// Twice: a match takes the character after it, so a name right
		// after another escapes the first pass.
		for range 2 {
			s = r.names.ReplaceAllString(s, "${1}[name]${3}")
		}
	}
	s = reBearer.ReplaceAllString(s, "$1 [secret]")
	s = reSecretKV.ReplaceAllString(s, "${1}[secret]")
	s = reURL.ReplaceAllString(s, "[url]")
	s = reEmail.ReplaceAllString(s, "[email]")
	s = reMAC.ReplaceAllString(s, "[mac]")
	s = reIPv4.ReplaceAllString(s, "[ip]")
	s = reIPv6.ReplaceAllStringFunc(s, func(m string) string {
		// A clock time, 12:34:56, is not an address. An address has a
		// hex letter, a "::" or more groups than a time.
		if strings.Contains(m, "::") || strings.Count(m, ":") >= 3 || strings.ContainsAny(strings.ToLower(m), "abcdef") {
			return "[ip]"
		}
		return m
	})
	s = reHost.ReplaceAllString(s, "[host]")
	s = reQuoted.ReplaceAllString(s, "[text]")
	s = reToken.ReplaceAllStringFunc(s, func(m string) string {
		if tokenLike(m) {
			return "[token]"
		}
		return m
	})
	s = reHexRun.ReplaceAllString(s, "[hex]")
	s = reDigits.ReplaceAllString(s, "[number]")
	s = reWord.ReplaceAllStringFunc(s, func(m string) string {
		if mixed(m) {
			return "[id]"
		}
		return m
	})
	return s
}

// tokenLike tells a credential or an id from a long code word: it has a
// digit, or upper and lower case without the underscores of a code word.
// planner_passive_arbitrage stays; a base64 token does not.
func tokenLike(s string) bool {
	upper, lower := false, false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			return true
		case c >= 'A' && c <= 'Z':
			upper = true
		case c >= 'a' && c <= 'z':
			lower = true
		}
	}
	return upper && lower && !strings.Contains(s, "_")
}

// mixed reports a word with both letters and digits.
func mixed(s string) bool {
	letter, digit := false, false
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			digit = true
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z'):
			letter = true
		}
	}
	return letter && digit
}

var (
	reAttrKey  = regexp.MustCompile(`(?:^|\s)([A-Za-z_][A-Za-z0-9_.-]{0,63})=`)
	reNumber   = regexp.MustCompile(`^-?\d{1,6}(?:\.\d+)?(?:[eE][-+]?\d+)?$`)
	reDuration = regexp.MustCompile(`^-?(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+$`)
	reCodeWord = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// identifying keys name a value that is about a person, a place, a device
// or a network, whatever it looks like.
var identifying = []string{
	"name", "label", "title", "alias", "serial", "sn", "mac", "vin", "imei", "email", "mail",
	"phone", "user", "login", "owner", "host", "ip", "addr", "address", "url", "uri", "endpoint",
	"ssid", "lat", "lon", "lng", "location", "street", "city", "zip", "post", "path", "file",
	"topic", "device", "model", "id",
}

func identifyingKey(k string) bool {
	k = strings.ToLower(k)
	for _, word := range splitKey(k) {
		if slices.Contains(identifying, word) {
			return true
		}
	}
	return secretKey(k)
}

func splitKey(k string) []string {
	return strings.FieldsFunc(k, func(r rune) bool { return r == '_' || r == '-' || r == '.' })
}

var secretWords = []string{"pass", "passwd", "password", "secret", "token", "apikey", "key", "auth",
	"credential", "credentials", "private", "cookie", "session", "bearer", "authorization"}

func secretKey(k string) bool {
	k = strings.ToLower(k)
	for _, word := range splitKey(k) {
		if slices.Contains(secretWords, word) {
			return true
		}
	}
	for _, s := range []string{"password", "secret", "token", "apikey", "api_key", "credential"} {
		if strings.Contains(k, s) {
			return true
		}
	}
	return false
}

// allowedKeys are the attribute keys a line keeps. Any other key goes with
// its value.
var allowedKeys = []string{
	"mode", "state", "reason", "code", "kind", "phase", "phases", "phase_mode", "op", "tier", "level",
	"status", "result", "action", "lane", "cap", "subsystem", "err", "error", "ok", "stale", "blocked",
	"strategy", "plan", "planner", "fallback", "carrier",
}

// numericKeys are the allowed keys whose value may be a number. A number
// under any other key goes, so coordinates, phone numbers and totals do not
// leave.
var numericKeys = []string{
	"power_w", "target_w", "setpoint_w", "w", "kw", "current_a", "amps", "a", "voltage", "v", "soc",
	"soc_pct", "pct", "ms", "s", "hold_s", "took", "elapsed", "elapsed_ms", "duration", "age_ms", "count",
	"n", "attempt", "attempts", "retries", "rev", "slot", "slots", "hz", "bucket", "bytes", "status_code",
	"lag_ms", "overrun_ms",
}

func keepKey(k string) bool {
	k = strings.ToLower(k)
	return slices.Contains(allowedKeys, k) || slices.Contains(numericKeys, k)
}

// Attrs scrubs a log line's "k=v k=v" tail. Only allowed keys stay. A value
// stays when it is a flag, a duration or a short code word, or a number
// under a key allowed numbers.
func (r *Redactor) Attrs(tail string) string {
	tail = strings.TrimSpace(tail)
	if tail == "" {
		return ""
	}
	locs := reAttrKey.FindAllStringSubmatchIndex(tail, -1)
	if len(locs) == 0 {
		return ""
	}
	var b strings.Builder
	for i, loc := range locs {
		key := tail[loc[2]:loc[3]]
		start := loc[1]
		end := len(tail)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		value := strings.TrimSpace(tail[start:end])
		if !keepKey(key) {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(r.attrValue(key, value))
	}
	return b.String()
}

func (r *Redactor) attrValue(key, value string) string {
	numeric := slices.Contains(numericKeys, strings.ToLower(key))
	switch {
	case identifyingKey(key):
		return "[omitted]"
	case value == "true" || value == "false" || value == "<nil>":
		return value
	case reDuration.MatchString(value):
		return value
	case reNumber.MatchString(value):
		if numeric {
			return value
		}
	case reCodeWord.MatchString(value) && r.Text(value) == value:
		return value
	}
	return "[omitted]"
}

// Line is one log line, as the box's ring holds it.
type Line struct {
	TsMs   int64
	Level  string
	Msg    string
	Driver string
	Attrs  string
}

// Out is a redacted line. Src is the driver's opaque id, empty for the
// box's own lines.
type Out struct {
	TsMs  int64
	Level string
	Src   string
	Msg   string
}

// Log scrubs one line. The box's own message is scrubbed; a driver's free
// text never leaves, so its line keeps the level, the driver's id and the
// allowed attributes. Attributes keep only allowed keys and safe values.
func (r *Redactor) Log(l Line) Out {
	out := Out{TsMs: l.TsMs, Level: l.Level}
	if l.Driver != "" {
		out.Src = r.ID(l.Driver)
	} else {
		out.Msg = r.Text(l.Msg)
	}
	if attrs := r.Attrs(l.Attrs); attrs != "" {
		out.Msg = strings.TrimSpace(out.Msg + " " + attrs)
	}
	return out
}

// removedKeys are configuration keys whose whole value goes: where the house
// is, how to reach a device, who it belongs to and where alerts are sent.
var removedKeys = []string{
	"lat", "lon", "lng", "latitude", "longitude", "location", "address", "street", "city", "zip",
	"postcode", "postal", "host", "hostname", "ip", "url", "uri", "endpoint", "broker", "mac",
	"serial", "sn", "vin", "imei", "email", "mail", "phone", "user", "username", "login", "ssid",
	"webhook", "ntfy", "telegram", "chat", "recipient", "recipients", "to", "topic", "server",
	"device", "port_name", "tty",
}

func removedKey(k string) bool {
	k = strings.ToLower(k)
	if slices.Contains(removedKeys, k) {
		return true
	}
	for _, word := range splitKey(k) {
		if slices.Contains(removedKeys, word) {
			return true
		}
	}
	return false
}

var nameKeys = []string{"name", "label", "title", "alias", "display_name", "friendly_name"}

// unitWords end a key whose number is a quantity: a power, an energy, a
// time, a share, a price. numberKeys are the settings whose number has no
// unit in its name. Any other number in the configuration goes, so a port,
// a Modbus unit id or an account number does not leave.
var (
	unitWords = []string{
		"w", "wh", "kw", "kwh", "kwp", "a", "amps", "v", "s", "ms", "min", "h", "hour", "hours", "days", "deg",
		"degc", "pct", "percent", "soc", "efficiency", "alpha", "k", "n", "cap", "threshold", "weight",
	}
	numberKeys = []string{"phases", "priority", "default_priority", "gain", "voltage", "data_bits", "stop_bits", "baud_rate"}
)

func numberKey(k string) bool {
	if slices.Contains(numberKeys, k) {
		return true
	}
	words := splitKey(k)
	return len(words) > 0 && slices.Contains(unitWords, words[len(words)-1])
}

func isNumber(v any) bool {
	switch v.(type) {
	case float64, float32, int, int64, int32, uint64, uint32, json.Number:
		return true
	}
	return false
}

// Config scrubs a configuration document decoded from JSON, in place, and
// returns it. Secret-named keys become "***", identifying keys go, names
// become opaque ids, a number stays only under a key that names its unit
// or a known setting, and every remaining string is scrubbed as text.
func (r *Redactor) Config(doc any) any {
	out, _ := r.config(doc, "")
	return out
}

// config scrubs doc found under key. keep is false when doc must go.
func (r *Redactor) config(doc any, key string) (any, bool) {
	switch v := doc.(type) {
	case map[string]any:
		for k, val := range v {
			lk := strings.ToLower(k)
			_, flag := val.(bool)
			switch {
			case secretKey(lk) && !flag:
				// A flag such as lan_auth says whether a secret is set,
				// not what it is.
				v[k] = "***"
			case removedKey(lk):
				delete(v, k)
			case slices.Contains(nameKeys, lk):
				if s, ok := val.(string); ok {
					v[k] = r.ID(s)
				} else {
					delete(v, k)
				}
			default:
				if out, keep := r.config(val, lk); keep {
					v[k] = out
				} else {
					delete(v, k)
				}
			}
		}
		// A map keyed by names, such as one entry per driver, keeps its
		// shape under opaque keys.
		for k, val := range v {
			if r.names != nil && r.names.MatchString(k) {
				delete(v, k)
				v[r.ID(k)] = val
			}
		}
		return v, true
	case []any:
		// A list's items are under the list's key.
		kept := v[:0]
		for _, item := range v {
			if out, keep := r.config(item, key); keep {
				kept = append(kept, out)
			}
		}
		return kept, true
	case string:
		return r.Text(v), true
	}
	if isNumber(doc) {
		return doc, numberKey(key)
	}
	return doc, true
}
