package probedns

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cuteip/proberchan/internal/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/otelconst"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/zap"
)

var (
	defaultTimeout = 1 * time.Second

	attrResonFailedToResolveIPAddr = attribute.String("reason", "failed to resolve ip address")
	attrResonFailedToParseServer   = attribute.String("reason", "failed to parse server")
	attrResonFailedToParseQType    = attribute.String("reason", "failed to parse qtype")
	attrDNSTimeout                 = attribute.String("reason", "dns timeout")
	attrResonUnknown               = attribute.String("reason", "unknown")

	ViewExponentialHistograms = []sdkmetric.View{
		sdkmetric.NewView(sdkmetric.Instrument{Name: "dns_latency"}, otelconst.ExponentialHistogramStream),
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
	conf *config.DNSConfig
}

func New(l *zap.Logger, dns *dnsutil.Runner, name string) (*Runner, error) {
	latencyHist, err := otel.Meter("proberchan").Int64Histogram("dns_latency",
		metric.WithUnit("ns"),
		metric.WithDescription("Latency (RTT) of dns probes"),
	)
	if err != nil {
		return nil, err
	}
	attemptsCounter, err := otel.Meter("proberchan").Int64Counter("dns_attempts",
		metric.WithDescription("Total number of dns probe attempts"),
	)
	if err != nil {
		return nil, err
	}
	failedCounter, err := otel.Meter("proberchan").Int64Counter("dns_failed",
		metric.WithDescription("Total number of failed dns probes"),
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
			conf: &config.DNSConfig{},
		},
		stop: make(chan struct{}),
	}, nil
}

func (r *Runner) GetConfig() *config.DNSConfig {
	r.state.mu.RLock()
	defer r.state.mu.RUnlock()
	return r.state.conf
}

func (r *Runner) SetConfig(conf *config.DNSConfig) {
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

func (r *Runner) probe(ctx context.Context, conf *config.DNSConfig) {
	var wg sync.WaitGroup
	baseAttr := []attribute.KeyValue{attribute.String("probe", r.name)}
	r.attempts.Add(ctx, 1, metric.WithAttributes(baseAttr...))
	for _, target := range conf.Targets {
		wg.Add(1)
		go func(target config.DNSTarget) {
			defer wg.Done()
			baseAttr := append(baseAttr, attribute.String("server", target.Server))
			qname := dnsutil.MustQnameSuffixDot(target.QName)
			baseAttr = append(baseAttr, attribute.String("qname", qname))
			qtype := strings.ToUpper(target.QType)
			baseAttr = append(baseAttr, attribute.String("qtype", qtype))

			protocol, host, port, err := parseServer(target.Server)
			if err != nil {
				r.l.Warn("failed to parse server", zap.Error(err))
				attrs := append(baseAttr, attrResonFailedToParseServer)
				r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
				return
			}
			_, _ = protocol, port // temp

			var dstIPAddrs []netip.Addr // 実際のパケット送信先 IP アドレス
			hostAddr, err := netip.ParseAddr(host)
			if err == nil {
				dstIPAddrs = []netip.Addr{hostAddr}
			} else {
				// target が IP アドレスでない場合は FQDN（末尾ドット有無は曖昧）とみなして名前解決する
				for _, ipv := range conf.ResolveIPVersions {
					// 名前解決に失敗しても、もう一方の IP バージョン側で成功する可能性があるので、continue で継続
					switch ipv {
					case 4:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(host), dns.Type(dns.TypeA))
						if err != nil {
							r.l.Warn("failed to resolve IPv4 address", zap.Error(err))
							attrs := append(baseAttr, attrResonFailedToResolveIPAddr)
							r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
							continue
						}
						dstIPAddrs = append(dstIPAddrs, ips...)
					case 6:
						ips, err := r.dns.ResolveIPAddrByQNAME(ctx, dnsutil.MustQnameSuffixDot(host), dns.Type(dns.TypeAAAA))
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

			qtypeInt, ok := dns.StringToType[qtype]
			if !ok {
				r.l.Warn("unknown DNS qtype", zap.String("qtype", qtype))
				attrs := append(baseAttr, attrResonFailedToParseQType)
				r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
				return
			}

			// とりあえずは逐次で DNS パケットを送信する
			for _, dstIPAddr := range dstIPAddrs {
				err := r.probeByIPAddr(ctx, protocol, dstIPAddr, port, qname, qtypeInt, baseAttr)
				if err != nil {
					r.l.Warn("failed to probe", zap.Error(err))
				}
			}
		}(target)
	}
	wg.Wait()
}

func (r *Runner) probeByIPAddr(
	ctx context.Context,
	protocol Protocol,
	ipAddr netip.Addr,
	port int,
	qname string,
	qtype uint16,
	baseAttrs []attribute.KeyValue,
) error {
	var timeout time.Duration
	if r.GetConfig().TimeoutMs == 0 {
		timeout = defaultTimeout
	} else {
		timeout = time.Duration(r.GetConfig().TimeoutMs) * time.Millisecond
	}

	client := dns.Client{
		Net:     protocol.String(),
		Timeout: timeout,
	}
	m := new(dns.Msg)
	m.SetQuestion(qname, qtype)
	if r.GetConfig().FlagRD {
		m.RecursionDesired = true
	}

	start := time.Now()
	ans, _, err := client.ExchangeContext(ctx, m, netip.AddrPortFrom(ipAddr, uint16(port)).String())
	if opErr, ok := err.(*net.OpError); ok && opErr.Timeout() {
		r.l.Warn("dns timeout (net.OpError)", zap.Error(err))
		attrs := append(baseAttrs, attrDNSTimeout)
		r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
		return nil
	}
	if err != nil {
		r.l.Warn("failed to exchange dns message", zap.Error(err))
		attrs := append(baseAttrs, attrResonUnknown)
		r.failed.Add(ctx, 1, metric.WithAttributes(attrs...))
		return err
	}
	end := time.Now()

	ipVersion := 4
	if ipAddr.Is6() {
		ipVersion = 6
	}

	rcodeStr, ok := dns.RcodeToString[ans.Rcode]
	if !ok {
		rcodeStr = fmt.Sprintf("%d", ans.Rcode)
	}

	attrs := []attribute.KeyValue{
		attribute.String("ip_address", ipAddr.String()),
		attribute.Int("ip_version", ipVersion),
		attribute.String("rcode", rcodeStr),
	}
	attrs = append(baseAttrs, attrs...)
	r.latency.Record(ctx, end.Sub(start).Nanoseconds(), metric.WithAttributes(attrs...))
	return nil
}

func parseServer(s string) (Protocol, string, int, error) {
	u, err := url.Parse(s)
	if err != nil {
		return "", "", 0, errors.Wrapf(err, "failed to parse server: %s", s)
	}

	protocol, err := NewProtocol(u.Scheme)
	if err != nil {
		return "", "", 0, errors.Wrapf(err, "failed to parse server protocol (scheme): %s", s)
	}

	host := u.Hostname()
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		switch protocol {
		case ProtocolUDP:
			port = 53
		case ProtocolTCP:
			port = 53
		}
	}
	return protocol, host, port, nil
}
