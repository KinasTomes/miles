package admin

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

func (s *server) costReport(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	q, err := query(c.r, "region", "as_of")
	if err != nil {
		return err
	}
	region := q.Get("region")
	if region == "" {
		region = "ap-southeast-1"
	}
	asOf := q.Get("as_of")
	if asOf == "" {
		asOf = s.now().Format("2006-01-02")
	}

	// Try loading catalog from configs/pricing or build minimal catalog
	pricingPath := fmt.Sprintf("configs/pricing/pricing.%s.example.json", region)
	var catalog *pricing.Catalog
	if b, err := os.ReadFile(pricingPath); err == nil {
		catalog, _ = pricing.Parse(b)
	}
	if catalog == nil {
		catalog = &pricing.Catalog{
			SchemaVersion: 1,
			Region:        region,
			AsOf:          asOf,
			Currency:      "USD",
			Basis:         "gross_model_before_tax_credits_free_tier",
			Rates:         []pricing.Rate{},
			Sources:       map[string]pricing.Source{},
		}
	}

	usage := []cost.Usage{}
	if s.Statistics != nil {
		global, err := s.Statistics.Global(c.r.Context())
		if err == nil {
			month := 30 * 24 * time.Hour
			elapsed := 30 * 24 * time.Hour
			bytesPerGB := uint64(1024 * 1024 * 1024)

			if len(global.PhysicalClasses) > 0 {
				for classStr, b := range global.PhysicalClasses {
					class := storage.StorageClass(classStr)
					if b > 0 && class.Validate() == nil {
						if u, e := cost.StorageUsage(class, uint64(b), elapsed, bytesPerGB, month); e == nil {
							usage = append(usage, u)
						}
					}
				}
			} else if global.BytesStored != nil && *global.BytesStored > 0 {
				if u, e := cost.StorageUsage(storage.Standard, uint64(*global.BytesStored), elapsed, bytesPerGB, month); e == nil {
					usage = append(usage, u)
				}
			}
		}
	}

	report, err := cost.Evaluate(catalog, usage)
	if err != nil {
		return err
	}

	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, report)
	return nil
}
