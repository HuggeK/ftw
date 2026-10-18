package onboarding

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
)

// EditOperations applies JSON Patch add/replace/remove operations to a
// named draft revision. Editing a list entry does not resend its siblings.
func (s *Service) EditOperations(live *config.Config, expected int64, raw json.RawMessage) (Draft, error) {
	return s.EditOperationsWithCheck(live, expected, raw, nil)
}

func (s *Service) EditOperationsWithCheck(live *config.Config, expected int64, raw json.RawMessage, beforeSave func() error) (Draft, error) {
	d, err := s.Read(live)
	if err != nil {
		return Draft{}, err
	}
	if d.Revision != expected {
		return Draft{}, ErrConflict
	}
	if err := rejectDuplicates(raw); err != nil {
		return Draft{}, err
	}
	var operations []struct {
		Op    string          `json:"op"`
		Path  string          `json:"path"`
		Value json.RawMessage `json:"value"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&operations); err != nil || len(operations) == 0 || len(operations) > 100 {
		return Draft{}, errors.New("send 1..100 setup add, replace or remove operations")
	}
	var original, next any
	dec = json.NewDecoder(bytes.NewReader(d.Config))
	dec.UseNumber()
	if err := dec.Decode(&original); err != nil {
		return Draft{}, err
	}
	dec = json.NewDecoder(bytes.NewReader(d.Config))
	dec.UseNumber()
	if err := dec.Decode(&next); err != nil {
		return Draft{}, err
	}
	for _, op := range operations {
		if op.Op != "add" && op.Op != "replace" && op.Op != "remove" {
			return Draft{}, errors.New("setup supports only add, replace and remove")
		}
		if !strings.HasPrefix(op.Path, "/") || len(op.Path) > 1024 {
			return Draft{}, errors.New("use a JSON pointer within the setup configuration")
		}
		parts := strings.Split(op.Path[1:], "/")
		for i, part := range parts {
			for j := 0; j < len(part); j++ {
				if part[j] == '~' {
					if j+1 >= len(part) || (part[j+1] != '0' && part[j+1] != '1') {
						return Draft{}, errors.New("invalid JSON pointer escape")
					}
					j++
				}
			}
			parts[i] = strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")
		}
		var value any
		if op.Op == "remove" {
			if len(op.Value) > 0 {
				return Draft{}, errors.New("remove does not take a value")
			}
		} else {
			dec = json.NewDecoder(bytes.NewReader(op.Value))
			dec.UseNumber()
			if len(op.Value) == 0 || dec.Decode(&value) != nil {
				return Draft{}, errors.New("add and replace require a JSON value")
			}
		}
		next, err = editPointer(next, parts, op.Op, value)
		if err != nil {
			return Draft{}, err
		}
	}
	patch, err := json.Marshal(mergeDifference(original, next))
	if err != nil {
		return Draft{}, err
	}
	// Edit performs the final revision check, type validation, secret
	// preservation and one durable write. Any concurrent edit conflicts.
	return s.EditWithCheck(live, expected, patch, beforeSave)
}

func editPointer(node any, parts []string, op string, value any) (any, error) {
	if len(parts) == 0 {
		return value, nil
	}
	key, last := parts[0], len(parts) == 1
	switch object := node.(type) {
	case map[string]any:
		old, exists := object[key]
		if last {
			if op != "add" && !exists {
				return nil, fmt.Errorf("setup field %q is absent", key)
			}
			if op == "remove" {
				delete(object, key)
			} else {
				object[key] = value
			}
			return object, nil
		}
		if !exists {
			return nil, fmt.Errorf("setup field %q is absent", key)
		}
		next, err := editPointer(old, parts[1:], op, value)
		if err != nil {
			return nil, err
		}
		object[key] = next
		return object, nil
	case []any:
		index, err := strconv.Atoi(key)
		if key == "-" && last && op == "add" {
			index, err = len(object), nil
		}
		if err != nil || index < 0 || index > len(object) || (index == len(object) && (!last || op != "add")) || key != "-" && strconv.Itoa(index) != key {
			return nil, errors.New("setup list index is absent or invalid")
		}
		if last {
			switch op {
			case "add":
				object = append(object, nil)
				copy(object[index+1:], object[index:])
				object[index] = value
			case "replace":
				object[index] = value
			case "remove":
				object = append(object[:index], object[index+1:]...)
			}
			return object, nil
		}
		next, err := editPointer(object[index], parts[1:], op, value)
		if err != nil {
			return nil, err
		}
		object[index] = next
		return object, nil
	default:
		return nil, errors.New("setup pointer does not name an object or list")
	}
}

func mergeDifference(before, after any) any {
	next, object := after.(map[string]any)
	if !object {
		return after
	}
	old, _ := before.(map[string]any)
	patch := map[string]any{}
	for key := range old {
		if _, exists := next[key]; !exists {
			patch[key] = nil
		}
	}
	for key, value := range next {
		patch[key] = mergeDifference(old[key], value)
	}
	return patch
}
