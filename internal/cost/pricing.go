package cost

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

// Quantities are already in the catalog's units. Requests are not thousands of
// requests; DDB units already include consistency/transaction/rounding factors.
type Usage struct {
	RateID   string `json:"rate_id"`
	Unit     string `json:"unit"`
	Quantity string `json:"quantity"`
}

type Line struct {
	Usage
	SourceID string `json:"source_id,omitempty"`
	USD      string `json:"usd_exact"`
}

type Unpriced struct {
	Usage
	Reason string `json:"reason"`
}

type Report struct {
	Region           string                    `json:"region"`
	AsOf             string                    `json:"as_of"`
	Currency         string                    `json:"currency"`
	Basis            string                    `json:"basis"`
	Sources          map[string]pricing.Source `json:"sources"`
	Scope            string                    `json:"scope"`
	Lines            []Line                    `json:"lines"`
	Unpriced         []Unpriced                `json:"unpriced"`
	Subtotal         string                    `json:"priced_subtotal_usd_exact"`
	CompleteTotalUSD *string                   `json:"complete_total_usd"`
}

var quantityPattern = regexp.MustCompile(`^[0-9]{1,64}(\.[0-9]{1,64}|/[1-9][0-9]{0,63})?$`)

func boundedAmount(r *big.Rat) bool {
	return r.Num().BitLen() <= 4096 && r.Denom().BitLen() <= 4096
}

func quantity(s string) (*big.Rat, error) {
	if !quantityPattern.MatchString(s) {
		return nil, errors.New("quantity must be a bounded nonnegative decimal or rational string")
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, errors.New("invalid quantity")
	}
	return r, nil
}

// Evaluate validates the shared catalog again, aggregates usage before applying
// progressive tiers, and preserves unpriced lines. W4 reports only a subtotal:
// supplied observations cannot prove complete infrastructure/network coverage.
func Evaluate(catalog *pricing.Catalog, usage []Usage) (Report, error) {
	var out Report
	if catalog == nil || len(usage) > 100000 {
		return out, errors.New("invalid catalog or usage count")
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return out, err
	}
	c, err := pricing.Parse(raw)
	if err != nil {
		return out, err
	}
	out = Report{Region: c.Region, AsOf: c.AsOf, Currency: c.Currency, Basis: c.Basis, Sources: c.Sources, Scope: "provided_usage_only", Lines: []Line{}, Unpriced: []Unpriced{}}
	rates := map[string]pricing.Rate{}
	for _, r := range c.Rates {
		rates[r.ID] = r
	}
	sums := map[string]*big.Rat{}
	units := map[string]string{}
	for _, u := range usage {
		q, e := quantity(u.Quantity)
		if e != nil || u.RateID == "" || len(u.RateID) > 128 || u.Unit == "" || len(u.Unit) > 64 {
			return Report{}, errors.New("invalid usage")
		}
		if old, exists := units[u.RateID]; exists && old != u.Unit {
			return Report{}, errors.New("conflicting usage units")
		}
		if r, exists := rates[u.RateID]; exists && r.Unit != u.Unit {
			return Report{}, fmt.Errorf("%s: usage unit does not match catalog", u.RateID)
		}
		units[u.RateID] = u.Unit
		if sums[u.RateID] == nil {
			sums[u.RateID] = new(big.Rat)
		}
		sums[u.RateID].Add(sums[u.RateID], q)
		if !boundedAmount(sums[u.RateID]) {
			return Report{}, errors.New("usage precision budget exceeded")
		}
	}
	ids := make([]string, 0, len(sums))
	for id := range sums {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	total := new(big.Rat)
	for _, id := range ids {
		q := sums[id]
		u := Usage{id, units[id], q.RatString()}
		r, exists := rates[id]
		if !exists || r.Status != "verified" {
			out.Unpriced = append(out.Unpriced, Unpriced{u, "missing_verified_price"})
			continue
		}
		amount := new(big.Rat)
		for _, tier := range r.Tiers {
			begin, _ := new(big.Rat).SetString(tier.Begin)
			if q.Cmp(begin) <= 0 {
				break
			}
			end := q
			if tier.End != nil {
				bound, _ := new(big.Rat).SetString(*tier.End)
				if bound.Cmp(q) < 0 {
					end = bound
				}
			}
			price, _ := new(big.Rat).SetString(tier.USD)
			part := new(big.Rat).Sub(end, begin)
			amount.Add(amount, part.Mul(part, price))
			if !boundedAmount(amount) {
				return Report{}, errors.New("price precision budget exceeded")
			}
		}
		total.Add(total, amount)
		if !boundedAmount(total) {
			return Report{}, errors.New("subtotal precision budget exceeded")
		}
		out.Lines = append(out.Lines, Line{u, r.SourceID, amount.RatString()})
	}
	out.Subtotal = total.RatString()
	return out, nil
}

// StorageUsage converts an explicitly observed byte-time interval to GB-Mo.
// The driver MUST declare bytes/GB and month length. This does not apply minimum
// sizes/durations or archival metadata: supply billable bytes or list omissions.
func StorageUsage(class storage.StorageClass, bytes uint64, elapsed time.Duration, bytesPerGB uint64, month time.Duration) (Usage, error) {
	rates := map[storage.StorageClass]string{storage.Standard: "s3.storage.standard", storage.StandardIA: "s3.storage.standard_ia", storage.GlacierIR: "s3.storage.glacier_ir", storage.Glacier: "s3.storage.glacier"}
	id, ok := rates[class]
	if !ok || elapsed < 0 || bytesPerGB == 0 || month <= 0 {
		return Usage{}, errors.New("invalid storage accounting interval")
	}
	n := new(big.Int).Mul(new(big.Int).SetUint64(bytes), big.NewInt(int64(elapsed)))
	d := new(big.Int).Mul(new(big.Int).SetUint64(bytesPerGB), big.NewInt(int64(month)))
	return Usage{id, "GB-Mo", new(big.Rat).SetFrac(n, d).RatString()}, nil
}
