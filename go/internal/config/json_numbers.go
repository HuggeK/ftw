package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
)

func DecodeJSON(raw []byte, cfg *Config) error { return decodeJSON(raw, cfg, false) }

func DecodeStrictJSON(raw []byte, cfg *Config) error { return decodeJSON(raw, cfg, true) }

func decodeJSON(raw []byte, cfg *Config, strict bool) error {
	if raw = bytes.TrimSpace(raw); len(raw) == 0 || raw[0] != '{' {
		return errors.New("settings must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return errors.New("settings must contain one JSON object")
	}
	return cfg.DecodeDriverNumbers()
}

// DecodeDriverNumbers is for a configuration decoded with UseNumber. Keep
// the numeric types existing control/driver code already accepts, without
// rounding an identifier or another untouched opaque value.
func (c *Config) DecodeDriverNumbers() error {
	for i := range c.Drivers {
		for key, value := range c.Drivers[i].Config {
			next, err := driverJSONNumber(value, fmt.Sprintf("/drivers/%d/config/%s", i, jsonPathKey(key)))
			if err != nil {
				return err
			}
			c.Drivers[i].Config[key] = next
		}
	}
	return nil
}

func jsonPathKey(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
}

func driverJSONNumber(value any, path string) (any, error) {
	switch v := value.(type) {
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n, nil
		}
		if n, err := strconv.ParseUint(string(v), 10, 64); err == nil {
			return n, nil
		}
		n, err := v.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s: numeric value is outside the supported range", path)
		}
		if n == 0 {
			// Do not build a large decimal denominator for an underflowed
			// exponent. A literal zero needs no exponent comparison.
			mantissa := strings.FieldsFunc(string(v), func(r rune) bool { return r == 'e' || r == 'E' })[0]
			if strings.Trim(mantissa, "-+.0") == "" {
				return n, nil
			}
			return nil, fmt.Errorf("%s: this number would lose precision", path)
		}
		encoded, err := json.Marshal(n)
		if err != nil {
			return nil, err
		}
		before, ok := new(big.Rat).SetString(string(v))
		after, encodedOK := new(big.Rat).SetString(string(encoded))
		if ok && encodedOK && before.Cmp(after) == 0 {
			return n, nil
		}
		return nil, fmt.Errorf("%s: this number would lose precision; use a string if it is an identifier", path)
	case map[string]any:
		for key, item := range v {
			next, err := driverJSONNumber(item, path+"/"+jsonPathKey(key))
			if err != nil {
				return nil, err
			}
			v[key] = next
		}
	case []any:
		for i, item := range v {
			next, err := driverJSONNumber(item, fmt.Sprintf("%s/%d", path, i))
			if err != nil {
				return nil, err
			}
			v[i] = next
		}
	}
	return value, nil
}
