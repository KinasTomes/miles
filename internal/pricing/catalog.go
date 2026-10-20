// Package pricing parses and validates the shared pricing catalog, without estimating costs.
package pricing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	pricingschema "github.com/KinasTomes/miles/configs/pricing"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

type Catalog struct {
	SchemaVersion int               `json:"schema_version"`
	Region        string            `json:"region"`
	AsOf          string            `json:"as_of"`
	Currency      string            `json:"currency"`
	Basis         string            `json:"basis"`
	Sources       map[string]Source `json:"sources"`
	StorageRules  []StorageRule     `json:"storage_rules"`
	Rates         []Rate            `json:"rates"`
}

type Source struct {
	URL         string `json:"url"`
	RetrievedAt string `json:"retrieved_at"`
	Kind        string `json:"kind"`
}

type StorageRule struct {
	Class                 string `json:"class"`
	MinimumDays           int64  `json:"minimum_days"`
	MinimumBillableBytes  int64  `json:"minimum_billable_bytes"`
	MetadataStandardBytes int64  `json:"metadata_standard_bytes"`
	MetadataArchiveBytes  int64  `json:"metadata_archive_bytes"`
	SourceID              string `json:"source_id"`
}

type Rate struct {
	ID              string            `json:"id"`
	Category        string            `json:"category"`
	Service         string            `json:"service"`
	Unit            string            `json:"unit"`
	Status          string            `json:"status"`
	SourceID        string            `json:"source_id"`
	Dimensions      map[string]string `json:"dimensions"`
	Tiers           []Tier            `json:"tiers"`
	Note            string            `json:"note"`
	GrossAdjustment *GrossAdjustment  `json:"gross_adjustment,omitempty"`
}

// Decimal quantities remain strings; End == nil means an unbounded tier.
type Tier struct {
	Begin string  `json:"begin"`
	End   *string `json:"end"`
	USD   string  `json:"usd"`
}

type GrossAdjustment struct {
	Method    string    `json:"method"`
	RawTiers  []Tier    `json:"raw_tiers"`
	Allowance Allowance `json:"allowance"`
}

type Allowance struct {
	Quantity string `json:"quantity"`
	Unit     string `json:"unit"`
	Scope    string `json:"scope"`
	SourceID string `json:"source_id"`
}

var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	const location = "https://miles.invalid/pricing.schema.json"
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(pricingschema.JSONSchema()))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	compiler.AssertFormat()
	if err := compiler.AddResource(location, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(location)
})

// Parse enforces both Draft 2020-12 and business rules. No catalog is returned on
// error, including when a required rate is absent or unpriced. No AWS calls occur.
func Parse(data []byte, requiredRates ...string) (*Catalog, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("pricing JSON: %w", err)
	}
	schema, err := compiledSchema()
	if err != nil {
		return nil, fmt.Errorf("pricing schema: %w", err)
	}
	if err := schema.Validate(document); err != nil {
		return nil, fmt.Errorf("pricing schema: %w", err)
	}
	var catalog Catalog
	if err := json.Unmarshal(data, &catalog); err != nil {
		return nil, fmt.Errorf("pricing catalog: %w", err)
	}
	if err := catalog.validate(requiredRates); err != nil {
		return nil, err
	}
	return &catalog, nil
}
