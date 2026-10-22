package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Decimal is an exact normalized base-10 coefficient/exponent value. It holds
// no float64, big-number backing slice, or mutable pointer. Equivalent JSON
// decimals normalize identically. A zero-value Decimal is invalid, not zero.
// Limits on precision/exponent are supplied by PolicyValidator; parsing never
// expands an exponent into a potentially enormous integer or denominator.
type Decimal struct {
	digits   string
	exponent int64
	negative bool
}

// Parts returns immutable decimal components: value = coefficient * 10^exponent.
// Empty coefficient denotes the invalid zero value.
func (d Decimal) Parts() (coefficient string, exponent int64) {
	if d.negative {
		return "-" + d.digits, d.exponent
	}
	return d.digits, d.exponent
}

func (d Decimal) String() string {
	s, exponent := d.Parts()
	if s == "" || exponent == 0 {
		return s
	}
	return s + "e" + strconv.FormatInt(exponent, 10)
}

func (d Decimal) MarshalJSON() ([]byte, error) {
	if d.digits == "" {
		return nil, ErrPolicyInvalid
	}
	return []byte(d.String()), nil
}

func (d *Decimal) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '-' && (data[0] < '0' || data[0] > '9') {
		return fmt.Errorf("%w: decimal must be a JSON number", ErrPolicyInvalid)
	}
	var number json.Number
	if err := json.Unmarshal(data, &number); err != nil {
		return fmt.Errorf("%w: decimal syntax", ErrPolicyInvalid)
	}
	s := strings.ToLower(number.String())
	exponent := int64(0)
	if at := strings.IndexByte(s, 'e'); at >= 0 {
		var err error
		exponent, err = strconv.ParseInt(s[at+1:], 10, 64)
		if err != nil {
			return fmt.Errorf("%w: decimal exponent overflow", ErrPolicyInvalid)
		}
		s = s[:at]
	}
	negative := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	if at := strings.IndexByte(s, '.'); at >= 0 {
		fractionDigits := int64(len(s) - at - 1)
		if exponent < math.MinInt64+fractionDigits {
			return fmt.Errorf("%w: decimal scale overflow", ErrPolicyInvalid)
		}
		exponent -= fractionDigits
		s = s[:at] + s[at+1:]
	}
	s = strings.TrimLeft(s, "0")
	if s == "" {
		*d = Decimal{digits: "0"}
		return nil
	}
	trimmed := strings.TrimRight(s, "0")
	zeros := int64(len(s) - len(trimmed))
	if exponent > math.MaxInt64-zeros {
		return fmt.Errorf("%w: decimal scale overflow", ErrPolicyInvalid)
	}
	*d = Decimal{digits: trimmed, exponent: exponent + zeros, negative: negative}
	return nil
}

// ratio validates [0,1] or [0,1) without float rounding or exponent expansion.
func (d Decimal) ratio(includeOne bool) bool {
	if d.digits == "" || d.negative {
		return false
	}
	if d.digits == "0" {
		return true
	}
	if d.digits == "1" && d.exponent == 0 {
		return includeOne
	}
	return d.exponent < -int64(len(d.digits)-1)
}
