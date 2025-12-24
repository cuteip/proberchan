package probeping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/cuteip/proberchan/internal/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/internal/netnshelper"
	"github.com/cuteip/proberchan/internal/timeutil"
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
	l          *zap.Logger
	dns        *dnsutil.Runner
	nsExecutor netnshelper.Executor
	latency    metric.Int64Histogram
	attempts   metric.Int64Counter
	failed     metric.Int64Counter

	name      string
	state     RunnerState
	namespace namespaceState
	stop      chan struct{}
}

type RunnerState struct {
	mu   sync.RWMutex
	conf *config.PingConfig
}

type namespaceState struct {
	mu   sync.RWMutex
	name string
}

func (n *namespaceState) Set(name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.name = strings.TrimSpace(name)
}

func (n *namespaceState) Get() string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return n.name
}

func New(l *zap.Logger, dns *dnsutil.Runner, name string, executor netnshelper.Executor) (*Runner, error) {
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
		l:          l,
		dns:        dns,
		nsExecutor: executor,
		latency:    latencyHist,
		attempts:   attemptsCounter,
		failed:     failedCounter,
		name:       name,
		state: RunnerState{
			mu:   sync.RWMutex{},
			conf: &config.PingConfig{},
		},
		namespace: namespaceState{
			mu: sync.RWMutex{},
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

func (r *Runner) SetNamespace(namespace string) {
	r.namespace.Set(namespace)
}

func (r *Runner) GetNamespace() string {
	return r.namespace.Get()
}

func (r *Runner) withNamespace(ctx context.Context, fn func(context.Context) error) error {
	namespace := r.GetNamespace()
	if namespace == "" || r.nsExecutor == nil {
		return fn(ctx)
	}
	return r.nsExecutor.WithNamespace(ctx, namespace, fn)
}

func (r *Runner) logFields(fields ...zap.Field) []zap.Field {
	if namespace := r.GetNamespace(); namespace != "" {
		fields = append(fields, zap.String("netns", namespace))
	}
	return fields
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

func (r *Runner) probe(ctx context.Context, conf *config.PingConfig) {
	var wg sync.WaitGroup
	baseAttr := []attribute.KeyValue{attribute.String("probe", r.name)}
	if conf.Src != "" {
		baseAttr = append(baseAttr, attribute.String("src", conf.Src))
	}
	r.attempts.Add(ctx, 1, metric.WithAttributes(baseAttr...))
	for _, target := range conf.Targets {
		wg.Add(1)
		go func(target config.PingTarget) {
			defer wg.Done()
			baseAttr := append(baseAttr, attribute.String("target", target.Host))

			if target.Description != "" {
				baseAttr = append(baseAttr, attribute.String("description", target.Description))
			}

			var dstIPAddrs []netip.Addr // 実際に ping する宛先 IP アドレス
			targetIPAddr, err := netip.ParseAddr(target.Host)
			if err == nil {
				dstIPAddrs = []netip.Addr{targetIPAddr}
			} else {
				// host が IP アドレスでない場合は FQDN（末尾ドット有無は曖昧）とみなして名前解決する
				for _, ipv := range conf.ResolveIPVersions {
					// 名前解決に失敗しても、もう一方の IP バージョン側で成功する可能性があるので、continue で継続
					switch ipv {
					case 4:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(target.Host), dns.Type(dns.TypeA))
						if err != nil {
							r.l.Warn("failed to resolve IPv4 address", r.logFields(zap.Error(err))...)
							attrs := append(baseAttr, attrResonFailedToResolveIPAddr)
							r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
							continue
						}
						dstIPAddrs = append(dstIPAddrs, ips...)
					case 6:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(target.Host), dns.Type(dns.TypeAAAA))
						if err != nil {
							r.l.Warn("failed to resolve IPv6 address", r.logFields(zap.Error(err))...)
							attrs := append(baseAttr, attrResonFailedToResolveIPAddr)
							r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
							continue
						}
						dstIPAddrs = append(dstIPAddrs, ips...)
					default:
						r.l.Warn("unknown IP version", r.logFields(zap.Int("ip_version", ipv))...)
					}
				}
			}
			// とりあえずは逐次で ping する
			for _, dstIPAddr := range dstIPAddrs {
				err := r.probeByIPAddr(ctx, dstIPAddr, baseAttr)
				if err != nil {
					r.l.Warn("failed to probe", r.logFields(zap.Error(err))...)
				}
			}
		}(target)
	}
	wg.Wait()
}

func (r *Runner) probeByIPAddr(ctx context.Context, ipAddr netip.Addr, baseAttrs []attribute.KeyValue) error {
	return r.withNamespace(ctx, func(ctx context.Context) error {
		conf := r.GetConfig()
		pinger := probing.New("")
		if ipAddr.Is6() && ipAddr.IsLinkLocalUnicast() {
			pinger.SetPrivileged(true)
		}
		ip := &net.IPAddr{IP: ipAddr.AsSlice()}
		if ipAddr.Is6() && ipAddr.IsLinkLocalUnicast() && conf.Src != "" {
			ip.Zone = conf.Src
		}
		pinger.SetIPAddr(ip)
		pinger.Count = 1
		if conf.DF {
			pinger.SetDoNotFragment(true)
		}
		if conf.Size > 0 {
			pinger.Size = int(conf.Size)
		}
		if conf.Src != "" {
			if _, err := net.InterfaceByName(conf.Src); err != nil {
				r.l.Warn("src interface not found for ping", r.logFields(
					zap.String("src", conf.Src),
					zap.String("dst_ip", ipAddr.String()),
					zap.Error(err),
				)...)
				return nil
			}
			pinger.InterfaceName = conf.Src
		}

		// pinger.Timeout にセットするとタイムアウトになったかどうかを判定できないので、
		// context に WithTimeout でセットしてそれで判定する
		// https://github.com/prometheus-community/pro-bing/issues/70#issuecomment-2307468862
		var timeout time.Duration
		if conf.TimeoutMs == 0 {
			timeout = defaultTimeout
		} else {
			timeout = time.Duration(conf.TimeoutMs) * time.Millisecond
		}
		pingerCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		r.l.Debug("ping run ...", r.logFields(
			zap.String("pinger", fmt.Sprintf("%+v", pinger)),
			zap.Duration("timeout", timeout),
		)...)
		ipVersion := 4
		if ipAddr.Is6() {
			ipVersion = 6
		}

		attrs := []attribute.KeyValue{
			attribute.Int("size", pinger.Size),
			attribute.Bool("df", conf.DF),
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
	})
}
