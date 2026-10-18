package onboarding

import (
	"reflect"
	"strings"

	"github.com/srcfl/ftw/go/internal/config"
)

// Schema describes this build's complete public configuration type. Driver
// Config maps use the live driver catalogue for their vendor-specific fields.
// Runtime-only fields and stored password hashes never enter this schema.
func Schema() map[string]any {
	schema := schemaFor(reflect.TypeFor[config.Config]())
	schema["$schema"] = "https://json-schema.org/draft/2020-12/schema"
	schema["description"] = "FTW configuration. Patch a draft, then use Core validation. Units are W, Wh, A, V, seconds and SoC fractions 0..1. Arrays replace the complete array; retain entries you did not change. Driver config fields come from /api/drivers/catalog."
	return schema
}

func schemaFor(t reflect.Type) map[string]any {
	if t.Kind() == reflect.Pointer {
		return map[string]any{"anyOf": []any{schemaFor(t.Elem()), map[string]any{"type": "null"}}}
	}
	switch t.Kind() {
	case reflect.Struct:
		properties := map[string]any{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			properties[name] = schemaFor(f.Type)
		}
		return map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaFor(t.Elem())}
	case reflect.Slice, reflect.Array:
		return map[string]any{"type": "array", "items": schemaFor(t.Elem())}
	case reflect.String:
		return map[string]any{"type": "string"}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	default:
		return map[string]any{}
	}
}
