package appproto

import "github.com/srcfl/ftw/go/internal/control"

// wireModes projects control's catalogue onto the wire.
//
// The catalogue is the single source of truth for what modes exist and how
// they are presented; hand-writing a list here would let the app offer a mode
// the box does not have, or hide one it does.
func wireModes(cat []control.ModeInfo) []ModeInfo {
	out := make([]ModeInfo, 0, len(cat))
	for _, m := range cat {
		out = append(out, ModeInfo{
			Key:     string(m.Key),
			Label:   m.Label,
			Tooltip: m.Tooltip,
			Tier:    string(m.Tier),
		})
	}
	return out
}
