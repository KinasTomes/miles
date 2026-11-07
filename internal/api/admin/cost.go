package admin

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/KinasTomes/miles/configs/pricing"
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

	var catalog *pricing.Catalog
	// If ap-southeast-1, use embedded default catalog
	if region == "ap-southeast-1" {
		catalog, _ = pricing.Parse(pricingschema.DefaultCatalog())
	} else {
		for _, p := range []string{
			fmt.Sprintf("configs/pricing/pricing.%s.example.json", region),
			fmt.Sprintf("../../configs/pricing/pricing.%s.example.json", region),
		} {
			if b, err := os.ReadFile(p); err == nil {
				catalog, _ = pricing.Parse(b)
				break
			}
		}
	}
	if catalog == nil {
		catalog, _ = pricing.Parse(pricingschema.DefaultCatalog())
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
