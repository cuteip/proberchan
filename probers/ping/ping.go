package ping

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	configpb "github.com/cuteip/proberchan/gen/config"
	probing "github.com/prometheus-community/pro-bing"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.uber.org/zap"
)

var (
	defaultTimeout = 1 * time.Second
)

type Runner struct {
	l       *zap.Logger
	latency metric.Int64Histogram
	timeout metric.Int64Counter
	failed  metric.Int64Counter
}

func New(l *zap.Logger) (*Runner, error) {
	latencyHist, err := otel.Meter("proberchan").Int64Histogram("ping_latency",
		metric.WithUnit("ns"),
		metric.WithDescription("Latency (RTT) of ping probes"),
	)
	if err != nil {
		return nil, err
	}
	timeoutCounter, err := otel.Meter("proberchan").Int64Counter("ping_timeout",
		metric.WithDescription("Total number of timeout ping probes"),
	)
	if err != nil {
		return nil, err
	}
	failedCounter, err := otel.Meter("proberchan").Int64Counter("ping_failed",
		metric.WithDescription("Total number of failed ping probes (exclude timeout)"),
	)
	if err != nil {
		return nil, err
	}
	return &Runner{
		l:       l,
		latency: latencyHist,
		timeout: timeoutCounter,
		failed:  failedCounter,
	}, nil
}

func (r *Runner) ProbeTickerLoop(ctx context.Context, conf *configpb.PingConfig) error {
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

func (r *Runner) Probe(ctx context.Context, conf *configpb.PingConfig) {
	var wg sync.WaitGroup
	for _, target := range conf.GetTargets() {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			err := r.ProbeByTarget(ctx, conf, target)
			if err != nil {
				r.l.Warn("failed to probe", zap.Error(err))
			}
		}(target)
	}
	wg.Wait()
}

func (r *Runner) ProbeByTarget(ctx context.Context, conf *configpb.PingConfig, target string) error {
	pinger, err := probing.NewPinger(target)
	if err != nil {
		return err
	}
	pinger.Count = 1
	if conf.GetDf() {
		pinger.SetDoNotFragment(true)
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
	attr := attribute.NewSet(
		attribute.String("target", target),
		attribute.Int("size", pinger.Size),
		attribute.Bool("df", conf.GetDf()),
	)
	err = pinger.RunWithContext(pingerCtx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// ping timeout
			r.timeout.Add(ctx, 1, metric.WithAttributeSet(attr))
			return nil
		}
		r.failed.Add(ctx, 1, metric.WithAttributeSet(attr))
		return err
	}
	stats := pinger.Statistics()
	// count は 1 だから min, max, avg もどれも同じになる
	r.latency.Record(ctx, stats.MaxRtt.Nanoseconds(), metric.WithAttributeSet(attr))
	return nil
}
