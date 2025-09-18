package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"syscall"

	"github.com/cuteip/proberchan/internal/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/probers"
	probedns "github.com/cuteip/proberchan/probers/dns"
	probehttp "github.com/cuteip/proberchan/probers/http"
	probeping "github.com/cuteip/proberchan/probers/ping"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/prometheus"

	// "go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func run(cmd *cobra.Command, _ []string) error {
	loggerConfig := zap.NewProductionConfig()
	loggerConfig.Encoding = "console"
	loggerConfig.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	loggerConfig.DisableStacktrace = true

	logLevelStr := viper.GetString(logLevelKey)
	logLevel, err := zap.ParseAtomicLevel(logLevelStr)
	if err != nil {
		return errors.Wrapf(err, "failed to parse log level: %s", logLevelStr)
	}
	loggerConfig.Level = zap.NewAtomicLevelAt(logLevel.Level())
	l, err := loggerConfig.Build()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	shutdownMeterProvider, err := initMeterProvider(ctx)
	if err != nil {
		return err
	}
	defer shutdownMeterProvider(ctx)

	configPath := viper.GetString(configKey)
	conf, err := config.LoadFromFile(configPath)
	if err != nil {
		return err
	}

	if err := conf.Validate(); err != nil {
		return errors.Wrap(err, "invalid configuration")
	}

	l.Sugar().Debugf("config: %+v", conf)

	dnsResolverIPAddrPortStr := conf.DNSResolver
	_, err = netip.ParseAddrPort(dnsResolverIPAddrPortStr)
	if err != nil {
		return errors.Wrapf(err, "failed to parse resolver address. must be '<ip_address>:<port>': %s", dnsResolverIPAddrPortStr)
	}

	dnsRunner := dnsutil.New(dnsResolverIPAddrPortStr)

	runningProbers := probers.NewRunningProbers()
	for _, probe := range conf.Probes {
		switch probe.Type {
		case "ping":
			l.Debug("starting ping prober", zap.String("name", probe.Name))
			prober, err := probeping.New(l, dnsRunner, probe.Name)
			if err != nil {
				return err
			}
			prober.SetConfig(probe.Ping)
			prober.Start(ctx)
			runningProbers.AddPing(probe.Name, prober)
		case "http":
			l.Debug("starting http prober", zap.String("name", probe.Name))
			prober, err := probehttp.New(l, dnsRunner, probe.Name)
			if err != nil {
				return err
			}
			prober.SetConfig(probe.HTTP)
			prober.Start(ctx)
			runningProbers.AddHTTP(probe.Name, prober)
		case "dns":
			l.Debug("starting dns prober", zap.String("name", probe.Name))
			prober, err := probedns.New(l, dnsRunner, probe.Name)
			if err != nil {
				return err
			}
			prober.SetConfig(probe.DNS)
			prober.Start(ctx)
			runningProbers.AddDNS(probe.Name, prober)
		}
	}

	term := make(chan os.Signal, 1)
	signal.Notify(term, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-term
		l.Info("sigterm signal received. shutdown ...")
		cancel()
	}()

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			<-hup
			l.Info("sighup signal received. reload ...")
			newConf, err := config.LoadFromFile(configPath)
			if err != nil {
				l.Warn("config.LoadFromFile() failed", zap.Error(err))
			}

			if err := newConf.Validate(); err != nil {
				l.Warn("newConf.Validate() failed", zap.Error(err))
				continue
			}

			// remove (stop)
			for _, curProbe := range conf.Probes {
				_, exist := newConf.GetProbe(curProbe.Name)
				if !exist {
					switch curProbe.Type {
					case "ping":
						l.Info("stop ping prober", zap.String("name", curProbe.Name))
						runningProbers.RemovePing(curProbe.Name)
					case "http":
						l.Info("stop http prober", zap.String("name", curProbe.Name))
						runningProbers.RemoveHTTP(curProbe.Name)
					case "dns":
						l.Info("stop dns prober", zap.String("name", curProbe.Name))
						runningProbers.RemoveDNS(curProbe.Name)
					}
				}
			}

			// add (start) or update
			for _, newConfProbe := range newConf.Probes {
				switch newConfProbe.Type {
				case "ping":
					prober, exist := runningProbers.GetPing(newConfProbe.Name)
					if exist {
						l.Info("update ping probe", zap.String("name", newConfProbe.Name))
						prober.SetConfig(newConfProbe.Ping)
					} else {
						l.Info("start ping probe", zap.String("name", newConfProbe.Name))
						prober, err := probeping.New(l, dnsRunner, newConfProbe.Name)
						if err != nil {
							l.Warn("failed to create new ping prober", zap.Error(err))
							continue
						}
						prober.SetConfig(newConfProbe.Ping)
						prober.Start(ctx)
						runningProbers.AddPing(newConfProbe.Name, prober)
					}
				case "http":
					prober, exist := runningProbers.GetHTTP(newConfProbe.Name)
					if exist {
						l.Info("update http probe", zap.String("name", newConfProbe.Name))
						prober.SetConfig(newConfProbe.HTTP)
					} else {
						l.Info("start new http probe", zap.String("name", newConfProbe.Name))
						prober, err := probehttp.New(l, dnsRunner, newConfProbe.Name)
						if err != nil {
							l.Warn("failed to create new http prober", zap.Error(err))
							continue
						}
						prober.SetConfig(newConfProbe.HTTP)
						prober.Start(ctx)
						runningProbers.AddHTTP(newConfProbe.Name, prober)
					}
				case "dns":
					prober, exist := runningProbers.GetDNS(newConfProbe.Name)
					if exist {
						l.Info("update dns probe", zap.String("name", newConfProbe.Name))
						prober.SetConfig(newConfProbe.DNS)
					} else {
						l.Info("start new dns probe", zap.String("name", newConfProbe.Name))
						prober, err := probedns.New(l, dnsRunner, newConfProbe.Name)
						if err != nil {
							l.Warn("failed to create new dns prober", zap.Error(err))
							continue
						}
						prober.SetConfig(newConfProbe.DNS)
						prober.Start(ctx)
						runningProbers.AddDNS(newConfProbe.Name, prober)
					}
				}
			}

			conf = newConf
			l.Info("reload done")
		}
	}()

	<-ctx.Done()
	l.Info("context canceled")
	return nil
}

func initMeterProvider(ctx context.Context) (func(context.Context) error, error) {
	expOtlpHTTP, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	readerOtlpHTTP := sdkmetric.NewPeriodicReader(expOtlpHTTP)

	// expStdout, err := stdoutmetric.New()
	// if err != nil {
	// 	return nil, err
	// }
	// readerStdout := sdkmetric.NewPeriodicReader(expStdout)

	readerProm, err := newPromExporter()
	if err != nil {
		return nil, err
	}
	go serveMetrics()

	res, err := resource.New(ctx, resource.WithHost(), resource.WithFromEnv())
	if err != nil {
		return nil, err
	}

	var views []sdkmetric.View
	views = append(views, probeping.ViewExponentialHistograms...)
	views = append(views, probehttp.ViewExponentialHistograms...)
	views = append(views, probedns.ViewExponentialHistograms...)
	mprovider := sdkmetric.NewMeterProvider(
		sdkmetric.WithView(views...),
		sdkmetric.WithReader(readerOtlpHTTP),
		// sdkmetric.WithReader(readerStdout),
		sdkmetric.WithReader(readerProm), // experimental
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mprovider)
	return mprovider.Shutdown, nil
}

func newPromExporter() (sdkmetric.Reader, error) {
	return prometheus.New()
}

func serveMetrics() {
	log.Printf("serving metrics at localhost:2223/metrics")
	http.Handle("/metrics", promhttp.Handler())
	err := http.ListenAndServe("127.0.0.1:2223", nil)
	if err != nil {
		fmt.Printf("error serving http: %v", err)
		return
	}
}
