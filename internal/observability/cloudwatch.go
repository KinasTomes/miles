package observability

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/aws/aws-sdk-go-v2/aws"
	cw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	"github.com/aws/aws-sdk-go-v2/service/cloudwatch/types"
)

type CloudWatchAPI interface {
	PutMetricData(context.Context, *cw.PutMetricDataInput, ...func(*cw.Options)) (*cw.PutMetricDataOutput, error)
}

// CloudWatch exports one bounded batch per minute, outside requests/scrapes.
// Counters become window deltas; gauges are observations (Average/Max, never an
// invented cluster sum). Unknown ACKs can duplicate a later delta: best-effort
// telemetry, not exact job accounting or a billing ledger.
type CloudWatch struct {
	api      CloudWatchAPI
	profile  string
	mu       sync.Mutex
	acked    map[string]float64
	failures atomic.Uint64
}

func NewCloudWatch(api CloudWatchAPI, profile string) (*CloudWatch, error) {
	if api == nil || (profile != "cloud" && profile != "edge") {
		return nil, errors.New("invalid CloudWatch exporter")
	}
	return &CloudWatch{api: api, profile: profile, acked: map[string]float64{}}, nil
}
func (c *CloudWatch) Failures() uint64 { return c.failures.Load() }
func (c *CloudWatch) Flush(ctx context.Context, at time.Time, samples []Metric) error {
	if c == nil || c.api == nil {
		return errors.New("invalid CloudWatch exporter")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if at.IsZero() || len(samples) > 128 {
		return errors.New("invalid metric batch")
	}
	input := &cw.PutMetricDataInput{Namespace: aws.String("Miles/DSG")}
	next := map[string]float64{}
	seen := map[string]bool{}
	for _, sample := range samples {
		key := metricKey(sample)
		if !validMetric(sample) || seen[key] {
			return errors.New("invalid metric series")
		}
		seen[key] = true
		value := sample.Value
		if sample.Counter {
			base := c.acked[key]
			if sample.Value < base {
				base = 0
			}
			value -= base
			next[key] = sample.Value
			if value == 0 {
				continue
			}
		}
		dims := []types.Dimension{{Name: aws.String("profile"), Value: aws.String(c.profile)}}
		keys := make([]string, 0, len(sample.Labels))
		for k := range sample.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			dims = append(dims, types.Dimension{Name: aws.String(k), Value: aws.String(sample.Labels[k])})
		}
		unit := types.StandardUnitCount
		if strings.HasSuffix(sample.Name, "_seconds") {
			unit = types.StandardUnitSeconds
		}
		input.MetricData = append(input.MetricData, types.MetricDatum{MetricName: aws.String(sample.Name), Value: aws.Float64(value), Unit: unit, Dimensions: dims, Timestamp: aws.Time(at.UTC()), StorageResolution: aws.Int32(60)})
	}
	if len(input.MetricData) == 0 {
		return nil
	}
	_, e := c.api.PutMetricData(ctx, input, func(o *cw.Options) { o.RetryMaxAttempts = 1 })
	if e != nil {
		c.failures.Add(1)
		return errors.New("CloudWatch metric export failed")
	}
	for key, value := range next {
		c.acked[key] = value
	}
	return nil
}
func (c *CloudWatch) Run(ctx context.Context, timer clock.Clock, source func() []Metric) error {
	if timer == nil || source == nil {
		return errors.New("invalid metric source")
	}
	for ctx.Err() == nil {
		if e := timer.Sleep(ctx, time.Minute); e != nil {
			return e
		}
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		_ = c.Flush(call, timer.Now().WallUTC, source())
		cancel()
	}
	return ctx.Err()
}
