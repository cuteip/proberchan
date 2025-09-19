package probeping

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/cuteip/proberchan/internal/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/otelconst"
	"github.com/miekg/dns"
	probing "github.com/prometheus-community/pro-bing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
)

var (
	defaultTimeout = 1 * time.Second

	attrResonFailedToResolveIPAddr = attribute.String("reason", "failed to resolve ip address")
	attrPingTimeout                = attribute.String("reason", "ping timeout")
	attrResonUnknown               = attribute.String("reason", "unknown")

	ViewExponentialHistograms = []sdkmetric.View{
		sdkmetric.NewView(sdkmetric.Instrument{Name: "ping_latency"}, otelconst.ExponentialHistogramStream),
	}
)

type Runner struct {
	l        *zap.Logger
	dns      *dnsutil.Runner
	latency  metric.Int64Histogram
	attempts metric.Int64Counter
	failed   metric.Int64Counter

	name  string
	state RunnerState
	stop  chan struct{}
}

type RunnerState struct {
	mu   sync.RWMutex
	conf *config.PingConfig
}

func New(l *zap.Logger, dns *dnsutil.Runner, name string) (*Runner, error) {
	latencyHist, err := otel.Meter("proberchan").Int64Histogram("ping_latency",
		metric.WithUnit("ns"),
		metric.WithDescription("Latency (RTT) of ping probes"),
	)
	if err != nil {
		return nil, err
	}
	attemptsCounter, err := otel.Meter("proberchan").Int64Counter("ping_attempts",
		metric.WithDescription("Total number of ping probe attempts"),
	)
	if err != nil {
		return nil, err
	}
	failedCounter, err := otel.Meter("proberchan").Int64Counter("ping_failed",
		metric.WithDescription("Total number of failed ping probes"),
		// 失敗理由は "reason" につける
	)
	if err != nil {
		return nil, err
	}
	return &Runner{
		l:        l,
		dns:      dns,
		latency:  latencyHist,
		attempts: attemptsCounter,
		failed:   failedCounter,
		name:     name,
		state: RunnerState{
			mu:   sync.RWMutex{},
			conf: &config.PingConfig{},
		},
		stop: make(chan struct{}),
	}, nil
}

func (r *Runner) GetConfig() *config.PingConfig {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	return r.state.conf
}

func (r *Runner) SetConfig(conf *config.PingConfig) {
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
	jitter := time.Duration(rand.Int63n(int64(baseInterval / 10))) // 0-10%
	timer := time.NewTimer(jitter)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.stop:
			return
		case <-timer.C:
			go r.probe(ctx, r.GetConfig())
			jitter = time.Duration(rand.Int63n(int64(baseInterval / 5)))
			timer.Reset(baseInterval + jitter)
		}
	}
}

func (r *Runner) probe(ctx context.Context, conf *config.PingConfig) {
	var wg sync.WaitGroup
	baseAttr := []attribute.KeyValue{attribute.String("probe", r.name)}
	r.attempts.Add(ctx, 1, metric.WithAttributes(baseAttr...))
	for _, target := range conf.Targets {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			baseAttr := append(baseAttr, attribute.String("target", target))
			// target は config に書かれた "targets" の文字列そのまま
			// めっちゃややこしい
			var dstIPAddrs []netip.Addr // 実際に ping する宛先 IP アドレス
			targetIPAddr, err := netip.ParseAddr(target)
			if err == nil {
				dstIPAddrs = []netip.Addr{targetIPAddr}
			} else {
				// target が IP アドレスでない場合は FQDN（末尾ドット有無は曖昧）とみなして名前解決する
				for _, ipv := range conf.ResolveIPVersions {
					// 名前解決に失敗しても、もう一方の IP バージョン側で成功する可能性があるので、continue で継続
					switch ipv {
					case 4:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(target), dns.Type(dns.TypeA))
						if err != nil {
							r.l.Warn("failed to resolve IPv4 address", zap.Error(err))
							attrs := append(baseAttr, attrResonFailedToResolveIPAddr)
							r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
							continue
						}
						dstIPAddrs = append(dstIPAddrs, ips...)
					case 6:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(target), dns.Type(dns.TypeAAAA))
						if err != nil {
							r.l.Warn("failed to resolve IPv6 address", zap.Error(err))
							attrs := append(baseAttr, attrResonFailedToResolveIPAddr)
							r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
							continue
						}
						dstIPAddrs = append(dstIPAddrs, ips...)
					default:
						r.l.Warn("unknown IP version", zap.Int("ip_version", ipv))
					}
				}
			}
			// とりあえずは逐次で ping する
			for _, dstIPAddr := range dstIPAddrs {
				err := r.probeByIPAddr(ctx, dstIPAddr, baseAttr)
				if err != nil {
					r.l.Warn("failed to probe", zap.Error(err))
				}
			}
		}(target)
	}
	wg.Wait()
}

func (r *Runner) probeByIPAddr(ctx context.Context, ipAddr netip.Addr, baseAttrs []attribute.KeyValue) error {
	pinger := probing.New("")
	pinger.SetIPAddr(&net.IPAddr{IP: ipAddr.AsSlice()})
	pinger.Count = 1
	if r.GetConfig().DF {
		pinger.SetDoNotFragment(true)
	}
	if r.GetConfig().Size > 0 {
		pinger.Size = int(r.GetConfig().Size)
	}

	// pinger.Timeout にセットするとタイムアウトになったかどうかを判定できないので、
	// context に WithTimeout でセットしてそれで判定する
	// https://github.com/prometheus-community/pro-bing/issues/70#issuecomment-2307468862
	var timeout time.Duration
	if r.GetConfig().TimeoutMs == 0 {
		timeout = defaultTimeout
	} else {
		timeout = time.Duration(r.GetConfig().TimeoutMs) * time.Millisecond
	}
	pingerCtx, _ := context.WithTimeout(ctx, timeout)
	r.l.Debug("ping run ...", zap.String("pinger", fmt.Sprintf("%+v", pinger)), zap.Duration("timeout", timeout))
	ipVersion := 4
	if ipAddr.Is6() {
		ipVersion = 6
	}

	attrs := []attribute.KeyValue{
		attribute.Int("size", pinger.Size),
		attribute.Bool("df", r.GetConfig().DF),
		attribute.String("ip_address", ipAddr.String()),
		attribute.Int("ip_version", ipVersion),
	}
	attrs = append(attrs, baseAttrs...)

	err := pinger.RunWithContext(pingerCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// ping timeout
			attrs = append(attrs, attrPingTimeout)
			r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
			return nil
		}
		attrs = append(attrs, attrResonUnknown)
		r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
		return err
	}
	stats := pinger.Statistics()
	// count は 1 だから min, max, avg もどれも同じになる
	r.latency.Record(ctx, stats.MaxRtt.Nanoseconds(), metric.WithAttributes(attrs...))
	return nil
}
