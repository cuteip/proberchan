package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"sync"

	configpb "github.com/cuteip/proberchan/gen/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	probehttp "github.com/cuteip/proberchan/probers/http"
	probeping "github.com/cuteip/proberchan/probers/ping"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"google.golang.org/protobuf/encoding/prototext"
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

	ctx := cmd.Context()
	shutdownMeterProvider, err := initMeterProvider(context.Background())
	if err != nil {
		return err
	}
	defer shutdownMeterProvider(ctx)

	conf, err := loadConfig(viper.GetString(configKey))
	if err != nil {
		return err
	}

	l.Sugar().Debugf("config: %+v", conf)

	dnsResolverIPAddrPortStr := conf.GetDnsResolver()
	_, err = netip.ParseAddrPort(dnsResolverIPAddrPortStr)
	if err != nil {
		return errors.Wrapf(err, "failed to parse resolver address. must be '<ip_address>:<port>': %s", dnsResolverIPAddrPortStr)
	}

	dnsRunner := dnsutil.New(dnsResolverIPAddrPortStr)
	proberPing, err := probeping.New(l, dnsRunner)
	if err != nil {
		return err
	}
	proberHTTP, err := probehttp.New(l, dnsRunner)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	for _, confProber := range conf.GetProbes() {
		switch confProber.Type {
		case configpb.ProberConfig_PING:
			err = proberPing.ValidateConfig(confProber.GetPingProbe())
			if err != nil {
				return errors.Wrapf(err, "failed to validate ping probe config")
			}
			wg.Add(1)
			go func(confProber *configpb.ProberConfig) {
				defer wg.Done()
				err = proberPing.ProbeTickerLoop(ctx, confProber.GetName(), confProber.GetPingProbe())
				if err != nil {
					l.Warn("failed to probe", zap.Error(err))
				}
			}(confProber)
		case configpb.ProberConfig_HTTP:
			err = proberHTTP.ValidateConfig(confProber.GetHttpProbe())
			if err != nil {
				return errors.Wrapf(err, "failed to validate http probe config")
			}
			wg.Add(1)
			go func(confProber *configpb.ProberConfig) {
				defer wg.Done()
				err = proberHTTP.ProbeTickerLoop(ctx, confProber.GetName(), confProber.GetHttpProbe())
				if err != nil {
					l.Warn("failed to probe", zap.Error(err))
				}
			}(confProber)
		}
	}
	wg.Wait()
	return nil
}

func initMeterProvider(ctx context.Context) (func(context.Context) error, error) {
	expOtlpHTTP, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	readerOtlpHTTP := sdkmetric.NewPeriodicReader(expOtlpHTTP)

	expStdout, err := stdoutmetric.New()
	if err != nil {
		return nil, err
	}
	readerStdout := sdkmetric.NewPeriodicReader(expStdout)

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
	mprovider := sdkmetric.NewMeterProvider(
		sdkmetric.WithView(views...),
		sdkmetric.WithReader(readerOtlpHTTP),
		sdkmetric.WithReader(readerStdout),
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

func loadConfig(confPath string) (*configpb.ConfigRoot, error) {
	c, err := os.ReadFile(confPath)
	if err != nil {
		return nil, err
	}

	pbConfig := configpb.ConfigRoot{}
	err = prototext.Unmarshal(c, &pbConfig)
	if err != nil {
		return nil, err
	}
	return &pbConfig, nil
}
