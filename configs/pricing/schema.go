// Package pricingschema embeds the shared pricing contract without duplicating it.
package pricingschema

import _ "embed"

//go:embed pricing.schema.json
var schema string

func JSONSchema() string { return schema }
