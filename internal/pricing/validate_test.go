package pricing

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type object = map[string]any

func fixture(t *testing.T) object {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "configs", "pricing", "pricing.ap-southeast-1.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc object
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func findRate(doc object, id string) object {
	for _, item := range doc["rates"].([]any) {
		if rate := item.(object); rate["id"] == id {
			return rate
		}
	}
	panic("missing fixture rate: " + id)
}

func firstTier(doc object) object {
	return findRate(doc, "s3.storage.standard")["tiers"].([]any)[0].(object)
}

func parseFixture(t *testing.T, doc object, required ...string) (*Catalog, error) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return Parse(data, required...)
}

func rejectCases(t *testing.T, cases map[string]func(object)) {
	t.Helper()
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := fixture(t)
			mutate(doc)
			if catalog, err := parseFixture(t, doc); err == nil || catalog != nil {
				t.Fatalf("invalid catalog accepted: catalog=%v, error=%v", catalog, err)
			}
		})
	}
}

func TestExampleAndRequiredVerifiedRate(t *testing.T) {
	catalog, err := parseFixture(t, fixture(t), "network.nat_hour", "ddb.storage")
	if err != nil {
		t.Fatal(err)
	}
	verified := 0
	for _, rate := range catalog.Rates {
		if rate.Status == "verified" {
			verified++
		}
		if rate.ID == "network.nat_hour" && rate.Tiers[0].USD != "0.0590000000" {
			t.Fatal("price changed during parsing")
		}
	}
	if len(catalog.Rates) != 50 || verified != 30 {
		t.Fatalf("unexpected counts: %d rates, %d verified", len(catalog.Rates), verified)
	}
}

func TestUnpricedIsNotZeroOrAvailableToCosting(t *testing.T) {
	doc := fixture(t)
	if len(findRate(doc, "ec2.compute")["tiers"].([]any)) != 0 {
		t.Fatal("unpriced rate has tiers")
	}
	for _, id := range []string{"ec2.compute", "not-a-price"} {
		if catalog, err := parseFixture(t, doc, id); err == nil || catalog != nil || !strings.Contains(err.Error(), "missing verified price") {
			t.Fatalf("required %s: catalog=%v, error=%v", id, catalog, err)
		}
	}
}

func TestSchemaRejectsInvalidValuesAndUnknownFields(t *testing.T) {
	rejectCases(t, map[string]func(object){
		"unknown root field":     func(d object) { d["account_id"] = "123456789012" },
		"missing required field": func(d object) { delete(d, "basis") },
		"wrong version":          func(d object) { d["schema_version"] = 2 },
		"invalid date":           func(d object) { d["as_of"] = "not-a-date" },
		"invalid calendar date":  func(d object) { d["as_of"] = "2026-02-30" },
		"invalid source URI":     func(d object) { d["sources"].(object)["api"].(object)["url"] = "https://bad host/" },
		"negative numeric price": func(d object) { firstTier(d)["usd"] = -1 },
		"positive numeric price": func(d object) { firstTier(d)["usd"] = 1 },
		"negative decimal price": func(d object) { firstTier(d)["usd"] = "-1" },
		"exponent price":         func(d object) { firstTier(d)["usd"] = "1e-3" },
		"missing tier end":       func(d object) { delete(firstTier(d), "end") },
		"unknown tier field":     func(d object) { firstTier(d)["extra"] = true },
		"wrong dimension type":   func(d object) { findRate(d, "s3.storage.standard")["dimensions"].(object)["sku"] = 42 },
		"unpriced with tiers":    func(d object) { findRate(d, "s3.storage.standard")["status"] = "unpriced" },
		"verified without tiers": func(d object) { findRate(d, "s3.storage.standard")["tiers"] = []any{} },
		"null tiers":             func(d object) { findRate(d, "s3.storage.standard")["tiers"] = nil },
	})
}

func TestDuplicateSourceAndTierIntegrity(t *testing.T) {
	rejectCases(t, map[string]func(object){
		"duplicate ID":          func(d object) { d["rates"] = append(d["rates"].([]any), d["rates"].([]any)[0]) },
		"unknown source":        func(d object) { findRate(d, "s3.storage.standard")["source_id"] = "unknown" },
		"nonzero first bound":   func(d object) { firstTier(d)["begin"] = "1" },
		"empty tier":            func(d object) { firstTier(d)["end"] = "0" },
		"gap":                   func(d object) { findRate(d, "s3.storage.standard")["tiers"].([]any)[1].(object)["begin"] = "999999" },
		"overlap":               func(d object) { findRate(d, "s3.storage.standard")["tiers"].([]any)[1].(object)["begin"] = "1" },
		"open before last tier": func(d object) { firstTier(d)["end"] = nil },
		"bounded last tier": func(d object) {
			tiers := findRate(d, "s3.storage.standard")["tiers"].([]any)
			tiers[len(tiers)-1].(object)["end"] = "999999999999"
		},
		"duplicate class":     func(d object) { d["storage_rules"] = append(d["storage_rules"].([]any), d["storage_rules"].([]any)[0]) },
		"missing class":       func(d object) { d["storage_rules"] = d["storage_rules"].([]any)[:3] },
		"unknown rule source": func(d object) { d["storage_rules"].([]any)[0].(object)["source_id"] = "unknown" },
		"future source":       func(d object) { d["sources"].(object)["api"].(object)["retrieved_at"] = "2099-01-01" },
	})
}

func TestRejectsMismatchedRegionAndQuoteDimensions(t *testing.T) {
	rejectCases(t, map[string]func(object){
		"catalog Region":   func(d object) { d["region"] = "us-east-1" },
		"quote regionCode": func(d object) { findRate(d, "s3.storage.standard")["dimensions"].(object)["regionCode"] = "us-east-1" },
		"location": func(d object) {
			findRate(d, "s3.storage.standard")["dimensions"].(object)["location"] = "US East (N. Virginia)"
		},
		"usage Region": func(d object) {
			findRate(d, "s3.storage.standard")["dimensions"].(object)["usagetype"] = "USE1-TimedStorage-ByteHrs"
		},
		"usage rate ID": func(d object) {
			findRate(d, "s3.storage.standard")["dimensions"].(object)["usagetype"] = "APS1-Requests-Tier1"
		},
		"missing regionCode": func(d object) { delete(findRate(d, "s3.storage.standard")["dimensions"].(object), "regionCode") },
		"missing SKU":        func(d object) { delete(findRate(d, "s3.storage.standard")["dimensions"].(object), "sku") },
		"unknown API rate":   func(d object) { findRate(d, "s3.storage.standard")["id"] = "s3.unregistered" },
	})
}

func TestRejectsWrongUnitsOrServicesForEveryAPIRate(t *testing.T) {
	for id := range apiRates {
		for _, field := range []string{"unit", "service"} {
			t.Run(id+"/"+field, func(t *testing.T) {
				doc := fixture(t)
				findRate(doc, id)[field] = "wrong-value"
				if catalog, err := parseFixture(t, doc); err == nil || catalog != nil || !strings.Contains(err.Error(), "service/unit") {
					t.Fatalf("wrong %s accepted: catalog=%v error=%v", field, catalog, err)
				}
			})
		}
	}
	for _, source := range []string{"api", "s3"} {
		doc := fixture(t)
		rate := findRate(doc, "s3.storage.standard")
		rate["unit"], rate["source_id"] = "Hrs", source
		if _, err := parseFixture(t, doc); err == nil {
			t.Fatalf("wrong unit accepted for source %s", source)
		}
	}
}

func TestGrossStorageExcludesButPreservesFreeTier(t *testing.T) {
	catalog, err := parseFixture(t, fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, rate := range catalog.Rates {
		if rate.ID == "ddb.storage" {
			adjustment := rate.GrossAdjustment
			if len(rate.Tiers) != 1 || rate.Tiers[0].Begin != "0" || rate.Tiers[0].End != nil || rate.Tiers[0].USD != "0.2850000000" {
				t.Fatal("gross paid tier changed")
			}
			if adjustment == nil || adjustment.Allowance.Scope != "account_region" || adjustment.Allowance.Quantity != "25" ||
				adjustment.RawTiers[0].Begin != "0" || *adjustment.RawTiers[0].End != "25" || adjustment.RawTiers[0].USD != "0.0000000000" {
				t.Fatal("Free Tier provenance changed")
			}
		}
		if rate.ID == "s3.restore_bulk_gb" && rate.Tiers[0].USD != "0.0000000000" {
			t.Fatal("genuine zero price changed")
		}
	}
}

func TestRejectsInconsistentGrossAdjustments(t *testing.T) {
	cases := map[string]func(object){
		"raw Free Tier used as gross": func(r object) { r["tiers"] = r["gross_adjustment"].(object)["raw_tiers"] },
		"missing adjustment":          func(r object) { delete(r, "gross_adjustment") },
		"zero gross rate":             func(r object) { r["tiers"].([]any)[0].(object)["usd"] = "0" },
		"wrong allowance":             func(r object) { r["gross_adjustment"].(object)["allowance"].(object)["quantity"] = "24" },
		"zero allowance":              func(r object) { r["gross_adjustment"].(object)["allowance"].(object)["quantity"] = "0" },
		"unknown allowance source":    func(r object) { r["gross_adjustment"].(object)["allowance"].(object)["source_id"] = "unknown" },
		"nonzero free rate":           func(r object) { r["gross_adjustment"].(object)["raw_tiers"].([]any)[0].(object)["usd"] = "0.1" },
		"bounded paid tariff":         func(r object) { r["gross_adjustment"].(object)["raw_tiers"].([]any)[1].(object)["end"] = "100" },
		"per trial allowance":         func(r object) { r["gross_adjustment"].(object)["allowance"].(object)["scope"] = "per_trial" },
		"unpriced adjustment":         func(r object) { r["status"], r["tiers"] = "unpriced", []any{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := fixture(t)
			mutate(findRate(doc, "ddb.storage"))
			if catalog, err := parseFixture(t, doc); err == nil || catalog != nil {
				t.Fatalf("invalid gross adjustment accepted: catalog=%v error=%v", catalog, err)
			}
		})
	}
	doc := fixture(t)
	findRate(doc, "s3.storage.standard")["gross_adjustment"] = findRate(doc, "ddb.storage")["gross_adjustment"]
	if _, err := parseFixture(t, doc); err == nil {
		t.Fatal("gross adjustment accepted on another rate")
	}
}

func TestDecimalComparisonsAreExact(t *testing.T) {
	doc := fixture(t)
	findRate(doc, "s3.storage.standard")["tiers"] = []any{
		object{"begin": "0", "end": "9007199254740992.000000000000000001", "usd": "0.000000000000000001"},
		object{"begin": "9007199254740992.000000000000000001", "end": "9007199254740992.000000000000000002", "usd": "0.000000000000000002"},
		object{"begin": "9007199254740992.000000000000000002", "end": nil, "usd": "0"},
	}
	if _, err := parseFixture(t, doc); err != nil {
		t.Fatal(err)
	}
	findRate(doc, "s3.storage.standard")["tiers"].([]any)[1].(object)["begin"] = "9007199254740992.000000000000000000"
	if _, err := parseFixture(t, doc); err == nil {
		t.Fatal("sub-float precision overlap accepted")
	}
}

func TestRejectsInvalidJSON(t *testing.T) {
	for _, input := range []string{"", "{", "null", "[]", "{} {}", "{} trailing"} {
		if catalog, err := Parse([]byte(input)); err == nil || catalog != nil {
			t.Fatalf("invalid JSON accepted: %q", input)
		}
	}
}
