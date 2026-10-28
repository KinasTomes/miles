package format

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

func validStrings(v reflect.Value) bool { return validStringsAt(v, 0) }
func validStringsAt(v reflect.Value, depth int) bool {
	if depth > 32 {
		return false
	}
	if !v.IsValid() {
		return true
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		return true
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return true
		}
		return validStringsAt(v.Elem(), depth+1)
	case reflect.String:
		return utf8.ValidString(v.String())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !validStringsAt(v.Field(i), depth+1) {
				return false
			}
		}
	case reflect.Map:
		it := v.MapRange()
		for it.Next() {
			if !validStringsAt(it.Key(), depth+1) || !validStringsAt(it.Value(), depth+1) {
				return false
			}
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if !validStringsAt(v.Index(i), depth+1) {
				return false
			}
		}
	}
	return true
}
func ValidStrings(value any) bool { return validStrings(reflect.ValueOf(value)) }

// Canonical strings and UTF-16 member ordering follow RFC8785. Numbers remain
// the reviewed nonnegative safe-integer subset; typed binary projections keep
// int64 business values in their exact schema instead of converting to float64.
func canonicalSubset(v any) []byte {
	var out bytes.Buffer
	var quoted func(string)
	quoted = func(s string) {
		out.WriteByte('"')
		for _, c := range s {
			switch c {
			case '"', '\\':
				out.WriteByte('\\')
				out.WriteRune(c)
			case '\b':
				out.WriteString(`\b`)
			case '\f':
				out.WriteString(`\f`)
			case '\n':
				out.WriteString(`\n`)
			case '\r':
				out.WriteString(`\r`)
			case '\t':
				out.WriteString(`\t`)
			default:
				if c < 32 {
					fmt.Fprintf(&out, `\u%04x`, c)
				} else {
					out.WriteRune(c)
				}
			}
		}
		out.WriteByte('"')
	}
	var write func(any)
	write = func(v any) {
		switch x := v.(type) {
		case string:
			quoted(x)
		case bool:
			if x {
				out.WriteString("true")
			} else {
				out.WriteString("false")
			}
		case json.Number:
			out.WriteString(string(x))
		case []any:
			out.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					out.WriteByte(',')
				}
				write(e)
			}
			out.WriteByte(']')
		case map[string]any:
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Slice(keys, func(i, j int) bool {
				a, b := utf16.Encode([]rune(keys[i])), utf16.Encode([]rune(keys[j]))
				for k := 0; k < len(a) && k < len(b); k++ {
					if a[k] != b[k] {
						return a[k] < b[k]
					}
				}
				return len(a) < len(b)
			})
			out.WriteByte('{')
			for i, k := range keys {
				if i > 0 {
					out.WriteByte(',')
				}
				quoted(k)
				out.WriteByte(':')
				write(x[k])
			}
			out.WriteByte('}')
		}
	}
	write(v)
	return out.Bytes()
}
