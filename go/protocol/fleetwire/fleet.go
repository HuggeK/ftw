// Package fleetwire names the anonymous daily report, without a home id.
package fleetwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
)

const Schema = "ftw.fleet/1"
const StatsSchema = "ftw.fleet-stats/1"
const Meaning = "reports, not unique boxes"
const MaxBodyBytes = 4096
const MaxDays = 90
const MaxLabels = 128
const MaxReports = 100_000

// Report carries only shared labels and buckets. Never add an identifier.
type Report struct {
	Schema  string   `json:"schema"`
	Version string   `json:"ftw_version"`
	Channel string   `json:"channel"`
	Drivers []string `json:"drivers"`
	Battery string   `json:"battery_kwh"`
	Zone    string   `json:"price_zone"`
	Age     string   `json:"install_age"`
}

var release = regexp.MustCompile(`^v[0-9]{1,4}\.[0-9]{1,4}\.[0-9]{1,4}(-beta\.[0-9]{1,4})?$`)
var driver = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)
var zone = regexp.MustCompile(`^[A-Z0-9-]{2,16}$`)

// Decode refuses extra fields, so a sender cannot add a household record.
func Decode(data []byte) (Report, error) {
	bad := errors.New("fleet: invalid report")
	var r Report
	if len(data) > MaxBodyBytes {
		return r, bad
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil || len(fields) != 7 || bytes.Equal(bytes.TrimSpace(fields["drivers"]), []byte("null")) {
		return r, bad
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Schema != Schema || (r.Version != "unknown" && !release.MatchString(r.Version)) {
		return r, bad
	}
	channel := "stable"
	if r.Version == "unknown" {
		channel = "unknown"
	} else if strings.Contains(r.Version, "-beta.") {
		channel = "beta"
	}
	if r.Channel != channel || len(r.Drivers) > 16 || !slices.Contains([]string{"none", "0-5", "5-15", "15-30", "30+"}, r.Battery) ||
		(r.Zone != "unknown" && !zone.MatchString(r.Zone)) || !slices.Contains([]string{"unknown", "0-1m", "1-6m", "6-12m", "1y+"}, r.Age) {
		return r, bad
	}
	for i, name := range r.Drivers {
		if !driver.MatchString(name) || (i > 0 && name <= r.Drivers[i-1]) {
			return r, bad
		}
	}
	return r, nil
}

// Day is a day's totals. No individual report survives aggregation.
type Day struct {
	Date     string         `json:"date"`
	Reports  int            `json:"reports"`
	Versions map[string]int `json:"ftw_versions"`
	Channels map[string]int `json:"channels"`
	Drivers  map[string]int `json:"drivers"`
	Battery  map[string]int `json:"battery_kwh"`
	Zones    map[string]int `json:"price_zones"`
	Age      map[string]int `json:"install_age"`
}

type View struct {
	Schema  string `json:"schema"`
	Meaning string `json:"meaning"`
	Days    []Day  `json:"days"`
}

func NewDay(date string) Day {
	return Day{Date: date, Versions: map[string]int{}, Channels: map[string]int{}, Drivers: map[string]int{},
		Battery: map[string]int{}, Zones: map[string]int{}, Age: map[string]int{}}
}

// Add bounds both the count and the number of labels in a day.
func (d *Day) Add(r Report) bool {
	if d.Reports < 0 || d.Reports >= MaxReports || d.Versions == nil || d.Channels == nil || d.Drivers == nil || d.Battery == nil || d.Zones == nil || d.Age == nil {
		return false
	}
	d.Reports++
	inc(d.Versions, r.Version)
	inc(d.Channels, r.Channel)
	if len(r.Drivers) == 0 {
		inc(d.Drivers, "none")
	} else {
		for _, name := range r.Drivers {
			inc(d.Drivers, name)
		}
	}
	inc(d.Battery, r.Battery)
	inc(d.Zones, r.Zone)
	inc(d.Age, r.Age)
	return true
}

func inc(counts map[string]int, label string) {
	if _, ok := counts[label]; !ok && len(counts) >= MaxLabels-1 {
		label = "other"
	}
	counts[label]++
}
