package cloudwire

import (
	"bufio"
	"os"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/srcfl/ftw/go/protocol"
)

// cborFields lists a struct's wire names, with the header's four apart.
func cborFields(t *testing.T, typ reflect.Type) (fields []string, nested map[string]reflect.Type, header bool) {
	t.Helper()
	nested = map[string]reflect.Type{}
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Anonymous {
			if f.Type != reflect.TypeOf(Header{}) {
				t.Fatalf("%s embeds %s; only Header may be embedded", typ.Name(), f.Type)
			}
			header = true
			continue
		}
		name := strings.Split(f.Tag.Get("cbor"), ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("%s.%s has no wire name", typ.Name(), f.Name)
		}
		fields = append(fields, name)
		inner := f.Type
		for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Slice || inner.Kind() == reflect.Map {
			inner = inner.Elem()
		}
		if inner.Kind() == reflect.Struct {
			nested[inner.Name()] = inner
		}
	}
	return fields, nested, header
}

func sorted(s []string) []string {
	out := slices.Clone(s)
	sort.Strings(out)
	return out
}

// Every field the box can encode has a row in Fields, and every row is a
// field. A new field without a row fails here, before it can leave a house.
func TestEveryFieldHasARow(t *testing.T) {
	seen := map[string]bool{}
	var walk func(table string, typ reflect.Type, wantHeader bool)
	walk = func(table string, typ reflect.Type, wantHeader bool) {
		fields, nested, header := cborFields(t, typ)
		if header != wantHeader {
			t.Errorf("%s: header embedded = %v, want %v", table, header, wantHeader)
		}
		rows, ok := Fields[table]
		if !ok {
			t.Errorf("%s has no table in Fields", table)
			return
		}
		seen[table] = true
		if got, want := sorted(fields), sorted(rows); !slices.Equal(got, want) {
			t.Errorf("%s: fields %v, rows %v", table, got, want)
		}
		for name, inner := range nested {
			walk(name, inner, false)
		}
	}
	for class, body := range Bodies {
		walk(class, reflect.TypeOf(body), true)
	}
	header, _, _ := cborFields(t, reflect.TypeOf(Header{}))
	if !slices.Equal(sorted(header), sorted(Fields["header"])) {
		t.Errorf("header: fields %v, rows %v", header, Fields["header"])
	}
	seen["header"] = true
	for table := range Fields {
		if !seen[table] {
			t.Errorf("Fields has a %s table no body uses", table)
		}
	}
}

// Each class a phone may read belongs to exactly one scope, and the scope is
// one an owner can grant FTW Cloud.
func TestEveryForwardedClassHasAGrantableScope(t *testing.T) {
	grantable := map[string]bool{"": true}
	for _, s := range protocol.CloudScopes {
		grantable[s] = true
	}
	for class, scope := range ClassScope {
		if !grantable[scope] {
			t.Errorf("%s needs %q, which a grant cannot name", class, scope)
		}
		if slices.Contains(ServiceOnly, class) {
			t.Errorf("%s is both forwarded and service-only", class)
		}
	}
	for class := range Bodies {
		_, forwarded := ClassScope[class]
		if forwarded == slices.Contains(ServiceOnly, class) {
			t.Errorf("%s must be either forwarded or service-only", class)
		}
	}
}

// The tables in docs/protocol-cloud.md and Fields say the same thing. The
// document is normative; this keeps the code from drifting from it.
func TestFieldsMatchTheProtocolDocument(t *testing.T) {
	const path = "../../../docs/protocol-cloud.md"
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		t.Skipf("%s is not in this checkout yet", path)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// A table belongs to the nearest heading above it: "### `live`", a bold
	// "**Source**" or "**`roster`**", a numbered "**Box → service: `hello`**",
	// or "## Common body fields" for the header. Any other heading ends it.
	classHeading := regexp.MustCompile("^### `([a-z.]+)`")
	boldHeading := regexp.MustCompile("^\\*\\*`?([A-Za-z]+)`?\\*\\*")
	stepHeading := regexp.MustCompile("^\\d+\\. \\*\\*[^`*]*`([a-z_]+)`\\*\\*")
	row := regexp.MustCompile("^\\s*\\| `([A-Za-z][A-Za-z0-9]*)` \\|")
	doc := map[string][]string{}
	current := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case classHeading.MatchString(line):
			current = classHeading.FindStringSubmatch(line)[1]
		case stepHeading.MatchString(line):
			current = stepHeading.FindStringSubmatch(line)[1]
		case strings.HasPrefix(line, "## Common body fields"):
			current = "header"
		case strings.HasPrefix(line, "#"), strings.HasPrefix(line, "**"):
			current = ""
			if m := boldHeading.FindStringSubmatch(line); m != nil {
				current = m[1]
			}
		case current != "" && row.MatchString(line):
			doc[current] = append(doc[current], row.FindStringSubmatch(line)[1])
		}
	}
	for table, rows := range Fields {
		if table == MsgBye {
			continue // stated in prose, not in a table
		}
		got, ok := doc[table]
		if !ok {
			t.Errorf("the document has no %s table", table)
			continue
		}
		if !slices.Equal(sorted(got), sorted(rows)) {
			t.Errorf("%s: document %v, code %v", table, sorted(got), sorted(rows))
		}
	}
}
