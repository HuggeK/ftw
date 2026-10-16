package protocol

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode"
)

// tags lists a struct's wire names, a nested struct's as "outer.inner", and
// the fields of an embedded struct as its own.
func tags(t *testing.T, typ reflect.Type, prefix string) (cbor, json []string) {
	t.Helper()
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			c, j := tags(t, f.Type, prefix)
			cbor, json = append(cbor, c...), append(json, j...)
			continue
		}
		c := strings.Split(f.Tag.Get("cbor"), ",")[0]
		j := strings.Split(f.Tag.Get("json"), ",")[0]
		cbor = append(cbor, prefix+c)
		json = append(json, j)
		inner := f.Type
		for inner.Kind() == reflect.Pointer {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct {
			c2, j2 := tags(t, inner, prefix+c+".")
			cbor, json = append(cbor, c2...), append(json, j2...)
		}
	}
	return cbor, json
}

// snake turns socPermille into soc_permille, the HTTP routes' spelling.
func snake(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}

// The car's names are the registry's, both ways, and the HTTP spelling is
// the same names in snake_case, so nobody writes a car's field twice.
func TestCarStructsAreTheRegistrys(t *testing.T) {
	for _, tc := range []struct {
		typ  reflect.Type
		want []string
	}{
		{reflect.TypeOf(Car{}), CarFields},
		{reflect.TypeOf(ChargerCar{}), ChargerCarFields},
	} {
		cbor, json := tags(t, tc.typ, "")
		if !slices.Equal(cbor, tc.want) {
			t.Errorf("%s: cbor names %v, registry %v", tc.typ.Name(), cbor, tc.want)
		}
		for i, c := range cbor {
			leaf := c[strings.LastIndex(c, ".")+1:]
			if json[i] != snake(leaf) {
				t.Errorf("%s.%s: json name %q, want %q", tc.typ.Name(), c, json[i], snake(leaf))
			}
		}
	}
}

// The watch's now carries the charger's car fields except the corrected
// car's id, and the car's level fields; a 512 B frame has no room for more.
func TestNowLoadpointNamesItsCar(t *testing.T) {
	cbor, _ := tags(t, reflect.TypeOf(NowLoadpoint{}), "")
	for _, name := range ChargerCarFields {
		if has := slices.Contains(cbor, name); has != (name != "carCorrectedFrom") {
			t.Errorf("NowLoadpoint has %s: %v", name, has)
		}
	}
	for _, name := range []string{"socPermille", "socSource", "socAgeS", "socEstimate"} {
		if !slices.Contains(cbor, name) {
			t.Errorf("NowLoadpoint has no %s", name)
		}
	}
}
