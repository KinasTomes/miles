// Package pricingschema embeds the shared pricing contract without duplicating it.
package pricingschema

import _ "embed"

//go:embed pricing.schema.json
var schema string

//go:embed pricing.ap-southeast-1.example.json
var defaultCatalog []byte

func JSONSchema() string { return schema }

func DefaultCatalog() []byte { return defaultCatalog }
