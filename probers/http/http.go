package probehttp

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/cuteip/proberchan/internal/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/internal/timeutil"
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
	attempts     metric.Int64Counter
	failed       metric.Int64Counter

	name  string
	state RunnerState
	stop  chan struct{}
}

type RunnerState struct {
	mu   sync.RWMutex
	conf *config.HTTPConfig
}

func New(l *zap.Logger, dns *dnsutil.Runner, name string) (*Runner, error) {
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
	attemptsCounter, err := otel.Meter("proberchan").Int64Counter("http_attempts",
		metric.WithDescription("Total number of http probe attempts"),
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
		attempts:     attemptsCounter,
		failed:       failedCounter,
		name:         name,
		state: RunnerState{
			mu:   sync.RWMutex{},
			conf: &config.HTTPConfig{},
		},
		stop: make(chan struct{}),
	}, nil
}

func (r *Runner) GetConfig() *config.HTTPConfig {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	return r.state.conf
}

func (r *Runner) SetConfig(conf *config.HTTPConfig) {
	r.state.mu.Lock()
	defer r.state.mu.Unlock()
	r.state.conf = conf
}

func (r *Runner) Start(ctx context.Context) {
	go r.ProbeTickerLoop(ctx)
}

func (r *Runner) Stop() {
	r.stop <- struct{}{}
}

func (r *Runner) ProbeTickerLoop(ctx context.Context) {
	baseInterval := time.Duration(r.GetConfig().IntervalMs) * time.Millisecond
	timer := time.NewTimer(timeutil.CalcJitter(baseInterval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-timer.C:
			go r.probe(ctx, r.GetConfig())
			timer.Reset(baseInterval + timeutil.CalcJitter(baseInterval))
		}
	}
}

func (r *Runner) probe(ctx context.Context, conf *config.HTTPConfig) {
	var wg sync.WaitGroup
	baseAttr := []attribute.KeyValue{attribute.String("name", r.name)}
	r.attempts.Add(ctx, 1, metric.WithAttributes(baseAttr...))
	for _, target := range conf.Targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			targetURL, err := url.Parse(target)
			if err != nil {
				r.failed.Add(ctx, 1, metric.WithAttributes(attrReasonFailedToParseURL))
				r.l.Warn("failed to parse URL", zap.Error(err))
				return
			}
			baseAttr := append(baseAttr, attribute.String("target", targetURL.String()))
			r.probeByTarget(ctx, targetURL, baseAttr)
		}(target)
	}
	wg.Wait()
}

func (r *Runner) probeByTarget(ctx context.Context, target *url.URL, baseAttrs []attribute.KeyValue) {
	var timeout time.Duration
	if r.GetConfig().TimeoutMs == 0 {
		timeout = defaultTimeout
	} else {
		timeout = time.Duration(r.GetConfig().TimeoutMs) * time.Millisecond
	}

	var userAgent string
	if r.GetConfig().UserAgent == "" {
		userAgent = defaultUserAgent
	} else {
		userAgent = r.GetConfig().UserAgent
	}

	type ipVersion struct {
		version int
		network string // net.Dial network
	}
	ipVersions := []ipVersion{}
	for _, ipv := range r.GetConfig().ResolveIPVersions {
		switch ipv {
		case 4:
			ipVersions = append(ipVersions, ipVersion{version: 4, network: "tcp4"})
		case 6:
			ipVersions = append(ipVersions, ipVersion{version: 6, network: "tcp6"})
		default:
			r.l.Warn("unknown resolve_ip_versions", zap.Int("ip_version", ipv))
		}
	}
	for _, ipv := range ipVersions {
		baseAttrs2 := append(baseAttrs, attribute.Int("ip_version", ipv.version))
		httpCaller := probinghttp.NewHttpCaller(target.String(),
			probinghttp.WithHTTPCallerLogger(NewLogger(r.l, r, baseAttrs2)),
			probinghttp.WithHTTPCallerClient(&http.Client{
				Transport: newCustomTransport(ipv.network, userAgent),
				// timeout などは WithHTTPCallerTimeout が優先される？
				// リダイレクト先を追わない
				CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
					return http.ErrUseLastResponse
				},
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
