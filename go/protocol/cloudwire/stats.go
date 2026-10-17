package cloudwire

import "slices"

// StatsPolicy names the scope included in the current Cloud terms, or
// accepted through the separate local statistics question. Earlier choices
// never accept this policy. Collection still requires service approval.
const StatsPolicy = "ftw-project-stats-2026-10-v2"
const CloudTermsPolicy = "ftw-cloud-2026-10-v2"
const MsgStats = "stats"
const StatsHourMS = int64(3_600_000)
const StatsMinimumHomes = 20

var StatsMetricKeys = []string{
	"battery_capacity_wh", "flexible_power_w", "pv_rated_w",
	"grid_import_wh", "grid_export_wh", "pv_generation_wh",
	"battery_charge_wh", "battery_discharge_wh", "vehicle_charge_wh", "vehicle_discharge_wh",
}

// Stats carries one closed UTC hour. Missing values are unknown. Capacity
// and power are configured limits, never proof that a command took effect.
// It is service-only; no phone or agent receives this message.
type Stats struct {
	Header
	StatsID []byte           `cbor:"statsId"`
	Policy  string           `cbor:"policy"`
	HourMS  int64            `cbor:"hourMs"`
	Metrics map[string]int64 `cbor:"metrics"`
}

func (s Stats) Valid(nowMS int64) bool {
	if len(s.StatsID) != 16 || s.Policy != StatsPolicy || s.HourMS <= 0 || s.HourMS%StatsHourMS != 0 ||
		s.HourMS+StatsHourMS > nowMS || s.HourMS < nowMS-24*StatsHourMS || len(s.Metrics) > len(StatsMetricKeys) {
		return false
	}
	for key, value := range s.Metrics {
		if !slices.Contains(StatsMetricKeys, key) || value < 0 || value > 1_000_000 || value%10 != 0 {
			return false
		}
	}
	return true
}
