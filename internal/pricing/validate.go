package pricing

import (
	"fmt"
	"math/big"
)

// New Regions and verified API rate IDs require an explicit qualifier/unit mapping.
var regions = map[string]struct{ location, usagePrefix string }{
	"ap-southeast-1": {"Asia Pacific (Singapore)", "APS1-"},
}

var apiRates = map[string]struct{ service, unit, usage string }{
	"s3.storage.standard":          {"AmazonS3", "GB-Mo", "TimedStorage-ByteHrs"},
	"s3.storage.standard_ia":       {"AmazonS3", "GB-Mo", "TimedStorage-SIA-ByteHrs"},
	"s3.storage.glacier_ir":        {"AmazonS3", "GB-Mo", "TimedStorage-GIR-ByteHrs"},
	"s3.storage.glacier":           {"AmazonS3", "GB-Mo", "TimedStorage-GlacierByteHrs"},
	"s3.put_standard":              {"AmazonS3", "Requests", "Requests-Tier1"},
	"s3.get_standard":              {"AmazonS3", "Requests", "Requests-Tier2"},
	"s3.retrieval_ia":              {"AmazonS3", "GB", "Retrieval-SIA"},
	"s3.retrieval_ir":              {"AmazonS3", "GB", "Retrieval-GIR"},
	"s3.restore_standard_gb":       {"AmazonS3", "GB", "Standard-Retrieval-Bytes"},
	"s3.put_ia":                    {"AmazonS3", "Requests", "Requests-SIA-Tier1"},
	"s3.put_ir":                    {"AmazonS3", "Requests", "Requests-GIR-Tier1"},
	"s3.put_glacier":               {"AmazonS3", "Requests", "Requests-GLACIER-Tier1"},
	"s3.get_ia":                    {"AmazonS3", "Requests", "Requests-SIA-Tier2"},
	"s3.get_ir":                    {"AmazonS3", "Requests", "Requests-GIR-Tier2"},
	"s3.restore_standard_requests": {"AmazonS3", "Requests", "Requests-Tier3"},
	"s3.restore_bulk_requests":     {"AmazonS3", "Requests", "Requests-Tier5"},
	"s3.restore_bulk_gb":           {"AmazonS3", "GB", "Bulk-Retrieval-Bytes"},
	"s3.transition_ir":             {"AmazonS3", "Requests", "Requests-Tier4"},
	"s3.transition_glacier":        {"AmazonS3", "Requests", "Requests-Tier3"},
	"ddb.storage":                  {"AmazonDynamoDB", "GB-Mo", "TimedStorage-ByteHrs"},
	"ddb.pitr":                     {"AmazonDynamoDB", "GB-Mo", "TimedPITRStorage-ByteHrs"},
	"ddb.backup":                   {"AmazonDynamoDB", "GB-Month", "TimedBackupStorage-ByteHrs"},
	"ddb.write":                    {"AmazonDynamoDB", "WriteRequestUnits", "WriteRequestUnits"},
	"ddb.read":                     {"AmazonDynamoDB", "ReadRequestUnits", "ReadRequestUnits"},
	"network.nat_hour":             {"AmazonEC2", "Hrs", "NatGateway-Hours"},
	"network.nat_gb":               {"AmazonEC2", "GB", "NatGateway-Bytes"},
	"network.endpoint_hour":        {"AmazonVPC", "Hrs", "VpcEndpoint-Hours"},
	"network.public_ipv4":          {"AmazonVPC", "Hrs", "PublicIPv4:InUseAddress"},
	"alb.hour":                     {"AWSELB", "Hrs", "LoadBalancerUsage"},
	"alb.lcu":                      {"AWSELB", "LCU-Hrs", "LCUUsage"},
}

func (c *Catalog) validate(requiredRates []string) error {
	region, ok := regions[c.Region]
	if !ok {
		return fmt.Errorf("unsupported pricing Region; add a verified qualifier mapping first")
	}
	for id, source := range c.Sources {
		// Schema format assertions have already checked canonical YYYY-MM-DD dates.
		if source.RetrievedAt > c.AsOf {
			return fmt.Errorf("source %s retrieval date is after catalog as_of", id)
		}
	}
	classes := map[string]bool{}
	for _, rule := range c.StorageRules {
		if classes[rule.Class] {
			return fmt.Errorf("duplicate storage class: %s", rule.Class)
		}
		classes[rule.Class] = true
		if _, ok := c.Sources[rule.SourceID]; !ok {
			return fmt.Errorf("storage rule has unknown source: %s", rule.SourceID)
		}
	}
	if len(classes) != 4 {
		return fmt.Errorf("storage rules must cover the four required classes exactly once")
	}
	rates := map[string]Rate{}
	for _, rate := range c.Rates {
		if _, exists := rates[rate.ID]; exists {
			return fmt.Errorf("duplicate rate ID: %s", rate.ID)
		}
		rates[rate.ID] = rate
		source, ok := c.Sources[rate.SourceID]
		if !ok {
			return fmt.Errorf("unknown pricing source: %s", rate.SourceID)
		}
		if err := rate.validateGrossStorage(c.Sources); err != nil {
			return fmt.Errorf("%s: %w", rate.ID, err)
		}
		if rate.Status != "verified" {
			continue
		}
		expected, registered := apiRates[rate.ID]
		if registered && (rate.Service != expected.service || rate.Unit != expected.unit) {
			return fmt.Errorf("%s: quote service/unit does not match rate ID", rate.ID)
		}
		if source.Kind == "aws_price_api" {
			for _, key := range []string{"sku", "regionCode", "location", "usagetype"} {
				if rate.Dimensions[key] == "" {
					return fmt.Errorf("%s: API quote is missing dimension %s", rate.ID, key)
				}
			}
			if rate.Dimensions["regionCode"] != c.Region || rate.Dimensions["location"] != region.location {
				return fmt.Errorf("%s: API quote Region/location does not match catalog", rate.ID)
			}
			if !registered {
				return fmt.Errorf("%s: unknown API rate ID; register its mapping first", rate.ID)
			}
			if rate.Dimensions["usagetype"] != region.usagePrefix+expected.usage {
				return fmt.Errorf("%s: API quote usagetype does not match Region/rate ID", rate.ID)
			}
		}
		if err := validateTiers(rate.Tiers); err != nil {
			return fmt.Errorf("%s: %w", rate.ID, err)
		}
	}
	for _, id := range requiredRates {
		if rate, ok := rates[id]; !ok || rate.Status != "verified" {
			return fmt.Errorf("missing verified price: %s", id)
		}
	}
	return nil
}

func validateTiers(tiers []Tier) error {
	previousEnd := "0"
	for i, tier := range tiers {
		if compare(tier.Begin, previousEnd) != 0 || (tier.End != nil && compare(*tier.End, tier.Begin) <= 0) {
			return fmt.Errorf("tiers must start at zero, be contiguous and non-empty")
		}
		if tier.End == nil {
			if i != len(tiers)-1 {
				return fmt.Errorf("only the last pricing tier may have an open end")
			}
		} else {
			previousEnd = *tier.End
		}
	}
	if tiers[len(tiers)-1].End != nil {
		return fmt.Errorf("last pricing tier must have an open end")
	}
	return nil
}

func (r Rate) validateGrossStorage(sources map[string]Source) error {
	adjustment := r.GrossAdjustment
	if r.ID != "ddb.storage" || r.Status != "verified" {
		if adjustment != nil {
			return fmt.Errorf("gross adjustment is only supported for verified ddb.storage")
		}
		return nil
	}
	if adjustment == nil {
		return fmt.Errorf("ddb.storage requires Free Tier provenance for the gross model")
	}
	allowance := adjustment.Allowance
	if _, ok := sources[allowance.SourceID]; !ok {
		return fmt.Errorf("unknown allowance source: %s", allowance.SourceID)
	}
	free, paid := adjustment.RawTiers[0], adjustment.RawTiers[1]
	if compare(allowance.Quantity, "0") <= 0 || compare(free.Begin, "0") != 0 || free.End == nil ||
		compare(*free.End, allowance.Quantity) != 0 || compare(free.USD, "0") != 0 ||
		compare(paid.Begin, allowance.Quantity) != 0 || paid.End != nil || compare(paid.USD, "0") <= 0 {
		return fmt.Errorf("invalid raw Free Tier tariff or allowance quantity")
	}
	if len(r.Tiers) != 1 || compare(r.Tiers[0].Begin, "0") != 0 || r.Tiers[0].End != nil ||
		compare(r.Tiers[0].USD, paid.USD) != 0 {
		return fmt.Errorf("gross storage must use the paid rate from quantity zero")
	}
	return nil
}

// compare is called only after the schema has validated nonnegative decimal strings.
func compare(a, b string) int {
	x, _ := new(big.Rat).SetString(a)
	y, _ := new(big.Rat).SetString(b)
	return x.Cmp(y)
}
