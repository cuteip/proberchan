package probehttp

import (
	"context"
	"net/http"
	"net/url"
	"sync"
	"time"

	configpb "github.com/cuteip/proberchan/gen/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/otelconst"
	probing "github.com/prometheus-community/pro-bing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
)

var (
	defaultTimeout = 1 * time.Second

	attrReasonFailedToParseURL = attribute.String("reason", "failed to parse URL")
	attrResonUnknown           = attribute.String("reason", "unknown")

	ViewExponentialHistograms = []sdkmetric.View{
		sdkmetric.NewView(sdkmetric.Instrument{Name: "http_response_time"}, otelconst.ExponentialHistogramStream),
		sdkmetric.NewView(sdkmetric.Instrument{Name: "http_time_to_first_byte"}, otelconst.ExponentialHistogramStream),
	}
)

type Runner struct {
	l            *zap.Logger
	dns          *dnsutil.Runner
	responseTime metric.Int64Histogram
	ttfb         metric.Int64Histogram
	failed       metric.Int64Counter
}

func New(l *zap.Logger, dns *dnsutil.Runner) (*Runner, error) {
	responseTime, err := otel.Meter("proberchan").Int64Histogram("http_response_time",
		metric.WithUnit("ns"),
		metric.WithDescription("http response time of http probes"),
	)
	if err != nil {
		return nil, err
	}
	ttfb, err := otel.Meter("proberchan").Int64Histogram("http_time_to_first_byte",
		metric.WithUnit("ns"),
		metric.WithDescription("time to first byte of http probes"),
	)
	if err != nil {
		return nil, err
	}
	failedCounter, err := otel.Meter("proberchan").Int64Counter("http_failed",
		metric.WithDescription("Total number of failed http probes"),
		// 失敗理由は "reason" につける
	)
	if err != nil {
		return nil, err
	}
	return &Runner{
		l:            l,
		dns:          dns,
		responseTime: responseTime,
		ttfb:         ttfb,
		failed:       failedCounter,
	}, nil
}

func (r *Runner) ProbeTickerLoop(ctx context.Context, conf *configpb.HttpConfig) error {
	interval := time.Duration(conf.GetIntervalMs()) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			go r.Probe(ctx, conf)
		}
	}
}

func (r *Runner) Probe(ctx context.Context, conf *configpb.HttpConfig) {
	var wg sync.WaitGroup
	for _, target := range conf.GetTargets() {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			targetURL, err := url.Parse(target)
			if err != nil {
				r.failed.Add(ctx, 1, metric.WithAttributes(attrReasonFailedToParseURL))
				r.l.Warn("failed to parse URL", zap.Error(err))
				return
			}
			baseAttr := []attribute.KeyValue{
				attribute.String("target", target),
			}
			r.ProbeByTarget(ctx, conf, targetURL, baseAttr)
		}(target)
	}
	wg.Wait()
}

func (r *Runner) ProbeByTarget(ctx context.Context, conf *configpb.HttpConfig, target *url.URL, baseAttrs []attribute.KeyValue) {
	var timeout time.Duration
	if conf.GetTimeoutMs() == 0 {
		timeout = defaultTimeout
	} else {
		timeout = time.Duration(conf.GetTimeoutMs()) * time.Millisecond
	}
	httpCaller := probing.NewHttpCaller(target.String(),
		probing.WithHTTPCallerTimeout(timeout),
		probing.WithHTTPCallerMethod(http.MethodGet),
		probing.WithHTTPCallerOnResp(func(suite *probing.TraceSuite, info *probing.HTTPCallInfo) {
			responseTime := suite.GetGeneralEnd().Sub(suite.GetGeneralStart()) // おそらく DNS 名前解決時間含む
			ttfb := suite.GetFirstByteReceived().Sub(suite.GetGeneralStart())  // おそらく DNS 名前解決時間含む
			attrs := []attribute.KeyValue{
				attribute.Int("status_code", info.StatusCode),
			}
			attrs = append(attrs, baseAttrs...)
			r.responseTime.Record(ctx, responseTime.Nanoseconds(), metric.WithAttributes(attrs...))
			r.ttfb.Record(ctx, ttfb.Nanoseconds(), metric.WithAttributes(attrs...))
		}),
		probing.WithHTTPCallerOnConnDone(func(suite *probing.TraceSuite, network, addr string, err error) {
			if err != nil {
				attrs := append(baseAttrs, attrResonUnknown)
				r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
				r.l.Warn("failed to connect", zap.Error(err))
			}
		}),
	)
	httpCaller.RunWithContext(ctx)
}
