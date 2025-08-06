package probeping

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	configpb "github.com/cuteip/proberchan/gen/config"
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
}

func New(l *zap.Logger, dns *dnsutil.Runner) (*Runner, error) {
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
	}, nil
}

func (r *Runner) ValidateConfig(conf *configpb.PingConfig) error {
	if len(conf.GetTargets()) == 0 {
		return errors.New("no targets. at least one target is required")
	}
	if len(conf.GetResolveIpVersions()) == 0 {
		return errors.New("no resolve_ip_versions. at least one resolve_ip_versions is required")
	}
	return nil
}

func (r *Runner) ProbeTickerLoop(ctx context.Context, name string, conf *configpb.PingConfig) error {
	interval := time.Duration(conf.GetIntervalMs()) * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			go r.Probe(ctx, name, conf)
		}
	}
}

func (r *Runner) Probe(ctx context.Context, name string, conf *configpb.PingConfig) {
	var wg sync.WaitGroup
	for _, target := range conf.GetTargets() {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			baseAttr := []attribute.KeyValue{
				attribute.String("probe", name),
				attribute.String("target", target),
			}
			r.attempts.Add(ctx, 1, metric.WithAttributes(baseAttr...))
			// target は config に書かれた "targets" の文字列そのまま
			// めっちゃややこしい
			var dstIPAddrs []netip.Addr // 実際に ping する宛先 IP アドレス
			targetIPAddr, err := netip.ParseAddr(target)
			if err == nil {
				dstIPAddrs = []netip.Addr{targetIPAddr}
			} else {
				// target が IP アドレスでない場合は FQDN（末尾ドット有無は曖昧）とみなして名前解決する
				for _, ipv := range conf.ResolveIpVersions {
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
						r.l.Warn("unknown IP version", zap.Int32("ip_version", ipv))
					}
				}
			}
			// とりあえずは逐次で ping する
			for _, dstIPAddr := range dstIPAddrs {
				err := r.ProbeByIPAddr(ctx, conf, dstIPAddr, baseAttr)
				if err != nil {
					r.l.Warn("failed to probe", zap.Error(err))
				}
			}
		}(target)
	}
	wg.Wait()
}

func (r *Runner) ProbeByIPAddr(ctx context.Context, conf *configpb.PingConfig, ipAddr netip.Addr, baseAttrs []attribute.KeyValue) error {
	pinger := probing.New("")
	pinger.SetIPAddr(&net.IPAddr{IP: ipAddr.AsSlice()})
	pinger.Count = 1
	if conf.GetDf() {
		pinger.SetDoNotFragment(true)
	}
	if conf.GetSize() > 0 {
		pinger.Size = int(conf.GetSize())
	}

	// pinger.Timeout にセットするとタイムアウトになったかどうかを判定できないので、
	// context に WithTimeout でセットしてそれで判定する
	// https://github.com/prometheus-community/pro-bing/issues/70#issuecomment-2307468862
	var timeout time.Duration
	if conf.GetTimeoutMs() == 0 {
		timeout = defaultTimeout
	} else {
		timeout = time.Duration(conf.GetTimeoutMs()) * time.Millisecond
	}
	pingerCtx, _ := context.WithTimeout(ctx, timeout)
	r.l.Debug("ping run ...", zap.String("pinger", fmt.Sprintf("%+v", pinger)), zap.Duration("timeout", timeout))
	ipVersion := 4
	if ipAddr.Is6() {
		ipVersion = 6
	}

	attrs := []attribute.KeyValue{
		attribute.Int("size", pinger.Size),
		attribute.Bool("df", conf.GetDf()),
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
