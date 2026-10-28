package dynamodb

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Records use native N/B/M/L attributes, never JSON blobs or float64 counters.
// Reflection is confined to typed internal records; callers cannot supply maps
// that bypass the schema or construct arbitrary authoritative fields.
func fieldName(name string) string {
	if name == "Version" {
		return "v"
	}
	if name == "Domain" {
		return "domain_id"
	}
	if name == "ETag" {
		return "etag"
	}
	if name == "ETagKind" {
		return "etag_kind"
	}
	if name == "CRC32C" {
		return "crc32c"
	}
	var out []rune
	runes := []rune(name)
	for i, r := range runes {
		if i > 0 && unicode.IsUpper(r) && (unicode.IsLower(runes[i-1]) || (i+1 < len(runes) && unicode.IsLower(runes[i+1]))) {
			out = append(out, '_')
		}
		out = append(out, unicode.ToLower(r))
	}
	return string(out)
}

func marshalNative(v reflect.Value) (types.AttributeValue, error) {
	if !v.IsValid() {
		return nil, s.ErrInvalid
	}
	if v.Type() == reflect.TypeOf(time.Time{}) {
		return text(v.Interface().(time.Time).UTC().Format(time.RFC3339Nano)), nil
	}
	if v.Type() == reflect.TypeOf(s.ObjectHead{}) {
		item, err := HeadItem(v.Interface().(s.ObjectHead))
		return &types.AttributeValueMemberM{Value: item}, err
	}
	switch v.Kind() {
	case reflect.String:
		return text(v.String()), nil
	case reflect.Bool:
		return &types.AttributeValueMemberBOOL{Value: v.Bool()}, nil
	case reflect.Int, reflect.Int64, reflect.Int32:
		return &types.AttributeValueMemberN{Value: strconv.FormatInt(v.Int(), 10)}, nil
	case reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint8:
		return number(v.Uint()), nil
	case reflect.Array, reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b := make([]byte, v.Len())
			reflect.Copy(reflect.ValueOf(b), v)
			return &types.AttributeValueMemberB{Value: b}, nil
		}
		list := make([]types.AttributeValue, v.Len())
		for i := range list {
			var err error
			list[i], err = marshalNative(v.Index(i))
			if err != nil {
				return nil, err
			}
		}
		return &types.AttributeValueMemberL{Value: list}, nil
	case reflect.Struct:
		item := map[string]types.AttributeValue{}
		for i := 0; i < v.NumField(); i++ {
			a, err := marshalNative(v.Field(i))
			if err != nil {
				return nil, err
			}
			item[fieldName(v.Type().Field(i).Name)] = a
		}
		return &types.AttributeValueMemberM{Value: item}, nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return nil, s.ErrInvalid
		}
		item := map[string]types.AttributeValue{}
		it := v.MapRange()
		for it.Next() {
			a, err := marshalNative(it.Value())
			if err != nil {
				return nil, err
			}
			item[it.Key().String()] = a
		}
		return &types.AttributeValueMemberM{Value: item}, nil
	}
	return nil, fmt.Errorf("%w: unsupported native field %s", s.ErrInvalid, v.Type())
}

func unmarshalNative(a types.AttributeValue, v reflect.Value) error {
	if v.Type() == reflect.TypeOf(time.Time{}) {
		str, ok := a.(*types.AttributeValueMemberS)
		if !ok || str == nil {
			return s.ErrUnavailable
		}
		t, err := time.Parse(time.RFC3339Nano, str.Value)
		if err != nil {
			return s.ErrUnavailable
		}
		v.Set(reflect.ValueOf(t))
		return nil
	}
	if v.Type() == reflect.TypeOf(s.ObjectHead{}) {
		m, ok := a.(*types.AttributeValueMemberM)
		if !ok || m == nil {
			return s.ErrUnavailable
		}
		h, err := decodeHead(m.Value)
		if err != nil {
			return err
		}
		v.Set(reflect.ValueOf(h))
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		x, ok := a.(*types.AttributeValueMemberS)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		v.SetString(x.Value)
	case reflect.Bool:
		x, ok := a.(*types.AttributeValueMemberBOOL)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		v.SetBool(x.Value)
	case reflect.Int, reflect.Int64, reflect.Int32:
		x, ok := a.(*types.AttributeValueMemberN)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		n, err := strconv.ParseInt(x.Value, 10, v.Type().Bits())
		if err != nil {
			return s.ErrUnavailable
		}
		v.SetInt(n)
	case reflect.Uint, reflect.Uint64, reflect.Uint32, reflect.Uint8:
		x, ok := a.(*types.AttributeValueMemberN)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		n, err := strconv.ParseUint(x.Value, 10, v.Type().Bits())
		if err != nil {
			return s.ErrUnavailable
		}
		v.SetUint(n)
	case reflect.Array, reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			b, err := CloneBinary(a)
			if err != nil {
				return s.ErrUnavailable
			}
			if v.Kind() == reflect.Array {
				if len(b) != v.Len() {
					return s.ErrUnavailable
				}
				reflect.Copy(v, reflect.ValueOf(b))
			} else {
				v.SetBytes(b)
			}
			return nil
		}
		x, ok := a.(*types.AttributeValueMemberL)
		if !ok || x == nil || v.Kind() != reflect.Slice {
			return s.ErrUnavailable
		}
		v.Set(reflect.MakeSlice(v.Type(), len(x.Value), len(x.Value)))
		for i, b := range x.Value {
			if err := unmarshalNative(b, v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Struct:
		x, ok := a.(*types.AttributeValueMemberM)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		for i := 0; i < v.NumField(); i++ {
			name := fieldName(v.Type().Field(i).Name)
			if err := unmarshalNative(x.Value[name], v.Field(i)); err != nil {
				return fmt.Errorf("%w: field %s", err, name)
			}
		}
	case reflect.Map:
		x, ok := a.(*types.AttributeValueMemberM)
		if !ok || x == nil {
			return s.ErrUnavailable
		}
		v.Set(reflect.MakeMap(v.Type()))
		for k, b := range x.Value {
			value := reflect.New(v.Type().Elem()).Elem()
			if err := unmarshalNative(b, value); err != nil {
				return err
			}
			v.SetMapIndex(reflect.ValueOf(k), value)
		}
	default:
		return s.ErrUnavailable
	}
	return nil
}

func nativeItem(value any) (map[string]types.AttributeValue, error) {
	a, err := marshalNative(reflect.ValueOf(value))
	if err != nil {
		return nil, err
	}
	m, ok := a.(*types.AttributeValueMemberM)
	if !ok {
		return nil, s.ErrInvalid
	}
	return m.Value, nil
}

func decodeHead(item map[string]types.AttributeValue) (s.ObjectHead, error) {
	var h s.ObjectHead
	// Rebuild the typed ObjectKey; the physical key intentionally omits incarnation.
	obj := map[string]types.AttributeValue{"bucket": item["bucket"], "bucket_id": item["bucket_id"], "key": item["key"]}
	for name, target := range map[string]*string{"bucket": &h.Object.Bucket, "bucket_id": &h.Object.BucketID, "key": &h.Object.Key} {
		x, ok := obj[name].(*types.AttributeValueMemberS)
		if !ok || x == nil {
			return h, s.ErrUnavailable
		}
		*target = x.Value
	}
	v := reflect.ValueOf(&h).Elem()
	for i := 0; i < v.NumField(); i++ {
		name := v.Type().Field(i).Name
		if name == "Object" || name == "PageIndex" {
			continue
		}
		if h.Deleted && (name == "ManifestID" || name == "PageCount" || name == "ChunkCount" || name == "ETag" || name == "ETagKind" || name == "Checksum" || name == "Metadata") {
			continue
		}
		// Deleted is decoded before optional tombstone payload fields.
		if name == "ManifestID" {
			if b, ok := item["deleted"].(*types.AttributeValueMemberBOOL); ok && b.Value {
				continue
			}
		}
		attr := item[fieldName(name)]
		if attr == nil {
			if b, ok := item["deleted"].(*types.AttributeValueMemberBOOL); ok && b.Value && strings.Contains(" ManifestID PageCount ChunkCount ETag ETagKind Checksum Metadata ", " "+name+" ") {
				continue
			}
			return h, s.ErrUnavailable
		}
		if err := unmarshalNative(attr, v.Field(i)); err != nil {
			return h, err
		}
	}
	if !h.Deleted {
		b, err := CloneBinary(item["page_index"])
		if err != nil || len(b) != int(h.PageCount)*8 {
			return h, s.ErrUnavailable
		}
		h.PageIndex = make([]uint64, h.PageCount)
		for i := range h.PageIndex {
			h.PageIndex[i] = binary.LittleEndian.Uint64(b[i*8:])
		}
	}
	if h.ValidateManifestDescriptor() != nil {
		return h, s.ErrUnavailable
	}
	return h, nil
}
