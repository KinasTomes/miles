package coordination

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
)

func TestChronyBoundsAndUntrustedObservations(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	valid := fmt.Sprintf("A9FEA97B,169.254.169.123,3,%d,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n", now.Unix())
	for _, name := range []string{"valid", "unsynced", "wrong-reference", "local-clock", "nan", "infinite", "wide", "old", "future", "command-late", "clock-step", "trailing", "missing"} {
		t.Run(name, func(t *testing.T) {
			clk, _ := clock.NewVirtual(now, 100)
			body := valid
			switch name {
			case "unsynced":
				body = strings.Replace(body, "Normal", "Not synchronised", 1)
			case "wrong-reference":
				body = strings.Replace(body, "169.254.169.123", "1.2.3.4", 1)
			case "local-clock":
				body = strings.Replace(body, "A9FEA97B", "7F7F0101", 1)
			case "nan":
				body = strings.Replace(body, ",0.001,", ",NaN,", 1)
			case "infinite":
				body = strings.Replace(body, ",0.001,", ",+Inf,", 1)
			case "wide":
				body = strings.Replace(body, ",0.1,0.01,", ",500,0.01,", 1)
			case "old":
				body = strings.Replace(body, fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(-time.Hour).Unix()), 1)
			case "future":
				body = strings.Replace(body, fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(time.Hour).Unix()), 1)
			case "trailing":
				body += valid
			case "missing":
				body = ""
			}
			c := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "process-1", read: func(context.Context) ([]byte, error) {
				if name == "command-late" {
					_ = clk.Advance(time.Second)
				}
				if name == "clock-step" {
					_ = clk.SetWall(now.Add(time.Hour))
				}
				return []byte(body), nil
			}}
			b, err := c.Bounds(context.Background())
			if name == "valid" {
				if err != nil || !b.Lower.Before(now) || !b.Upper.After(now) || b.Horizon.Sub(b.Upper) != runtimeHorizon {
					t.Fatal(b, err)
				}
			} else if err == nil {
				t.Fatal("untrusted clock accepted", b)
			}
		})
	}
}
