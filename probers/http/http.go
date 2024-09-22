package probehttp

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	configpb "github.com/cuteip/proberchan/gen/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/otelconst"
	"github.com/cuteip/proberchan/probinghttp"
	probing "github.com/prometheus-community/pro-bing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var (
	defaultTimeout   = 1 * time.Second
	defaultUserAgent = "proberchan (+https://github.com/cuteip/proberchan)"

	attrReasonFailedToParseURL  = attribute.String("reason", "failed to parse URL")
	attrReasonUnknown           = attribute.String("reason", "unknown")
	attrReasonConnectionRefused = attribute.String("reason", "connection refused")

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

func (r *Runner) ValidateConfig(conf *configpb.HttpConfig) error {
	if len(conf.GetTargets()) == 0 {
		return errors.New("no targets. at least one target is required")
	}
	if len(conf.GetResolveIpVersions()) == 0 {
		return errors.New("no resolve_ip_versions. at least one resolve_ip_versions is required")
	}
	return nil
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

	var userAgent string
	if conf.GetUserAgent() == "" {
		userAgent = defaultUserAgent
	} else {
		userAgent = conf.GetUserAgent()
	}

	type ipVersion struct {
		version int
		network string // net.Dial network
	}
	ipVersions := []ipVersion{}
	for _, ipv := range conf.GetResolveIpVersions() {
		switch ipv {
		case 4:
			ipVersions = append(ipVersions, ipVersion{version: 4, network: "tcp4"})
		case 6:
			ipVersions = append(ipVersions, ipVersion{version: 6, network: "tcp6"})
		default:
			r.l.Warn("unknown resolve_ip_versions", zap.Int32("ip_version", ipv))
		}
	}
	for _, ipv := range ipVersions {
		baseAttrs2 := append(baseAttrs, attribute.Int("ip_version", ipv.version))
		httpCaller := probinghttp.NewHttpCaller(target.String(),
			probinghttp.WithHTTPCallerLogger(NewLogger(r.l, r, baseAttrs2)),
			probinghttp.WithHTTPCallerClient(&http.Client{
				Transport: newCustomTransport(ipv.network, userAgent),
				// timeout などは WithHTTPCallerTimeout が優先される？
			}),
			probinghttp.WithHTTPCallerTimeout(timeout),
			probinghttp.WithHTTPCallerMethod(http.MethodGet),
			probinghttp.WithHTTPCallerOnResp(func(suite *probinghttp.TraceSuite, info *probinghttp.HTTPCallInfo) {
				responseTime := suite.GetGeneralEnd().Sub(suite.GetGeneralStart()) // おそらく DNS 名前解決時間含む
				ttfb := suite.GetFirstByteReceived().Sub(suite.GetGeneralStart())  // おそらく DNS 名前解決時間含む
				attrs := append(baseAttrs2, attribute.Int("status_code", info.StatusCode))
				r.responseTime.Record(ctx, responseTime.Nanoseconds(), metric.WithAttributes(attrs...))
				r.ttfb.Record(ctx, ttfb.Nanoseconds(), metric.WithAttributes(attrs...))
			}),
			probinghttp.WithHTTPCallerOnConnDone(func(suite *probinghttp.TraceSuite, network, addr string, err error) {
				if err != nil {
					// http.Client Do の err != nil が多く、ここにはほとんど来ず、Logger.Errorf() に来る？
					attrs := append(baseAttrs2, attrReasonUnknown)
					r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
					r.l.Warn("failed to connect", zap.Error(err))
				}
			}),
		)
		httpCaller.RunWithContext(ctx)
		httpCaller.Stop()
	}
}

type customTransport struct {
	transport http.RoundTripper
	userAgent string
}

func newCustomTransport(dialNetwork string, userAgent string) http.RoundTripper {
	dt := http.DefaultTransport.(*http.Transport)
	dt.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
		return net.Dial(dialNetwork, addr)
	}
	// interval おきに毎度新規に TCP コネクションを確立させたいので
	dt.DisableKeepAlives = true
	return &customTransport{
		transport: dt,
		userAgent: userAgent,
	}
}
func (t *customTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", t.userAgent)
	return t.transport.RoundTrip(req)
}

type logger struct {
	l         *zap.Logger
	r         *Runner
	baseAttrs []attribute.KeyValue
}

func convertToFields(args []interface{}) []zapcore.Field {
	fields := make([]zapcore.Field, len(args))
	for i, arg := range args {
		fields[i] = zap.Any("", arg)
	}
	return fields
}

func NewLogger(l *zap.Logger, r *Runner, baseAttrs []attribute.KeyValue) probing.Logger {
	return &logger{
		l:         l,
		r:         r,
		baseAttrs: baseAttrs,
	}
}

func (l *logger) Debugf(format string, args ...interface{}) {
	l.l.Debug(format, convertToFields(args)...)
}

func (l *logger) Errorf(format string, args ...interface{}) {
	// https://github.com/prometheus-community/pro-bing/blob/v0.4.1/http.go#L400-L402
	// http.Client{} の Do() で Connection refused などが発生すると、probing の callback では取れず、
	// このロガーで取るしかないので、ここでやる

	ctx := context.Background()
	isOtelFailedHandled := false

	if len(args) > 0 {
		if urlErr, ok := args[0].(*url.Error); ok {
			if opErr, ok := urlErr.Err.(*net.OpError); ok {
				if syscallErr, ok := opErr.Err.(*os.SyscallError); ok {
					if syscallErr.Err == syscall.ECONNREFUSED {
						attrs := append(l.baseAttrs, attrReasonConnectionRefused)
						l.r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
						isOtelFailedHandled = true
					}
				}
			}
		}
	}

	if !isOtelFailedHandled {
		attrs := append(l.baseAttrs, attrReasonUnknown)
		l.r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
	}

	l.l.Error(format, convertToFields(args)...)
}

func (l *logger) Fatalf(format string, args ...interface{}) {
	// Fatal で exit されると困るので warn に落とす
	l.l.Warn(format, convertToFields(args)...)
}

func (l *logger) Infof(format string, args ...interface{}) {
	l.l.Info(format, convertToFields(args)...)
}

func (l *logger) Warnf(format string, args ...interface{}) {
	l.l.Warn(format, convertToFields(args)...)
}
