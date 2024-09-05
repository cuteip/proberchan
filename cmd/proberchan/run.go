package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/netip"
	"os"
	"time"

	configpb "github.com/cuteip/proberchan/gen/config"
	"github.com/cuteip/proberchan/internal/dnsutil"
	"github.com/cuteip/proberchan/probers/ping"
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
	proberPing, err := ping.New(l, dnsRunner)
	if err != nil {
		return err
	}
	switch conf.GetProbe().Type {
	case configpb.ProberConfig_PING:
		err = proberPing.ValidateConfig(conf.Probe.GetPingProbe())
		if err != nil {
			return errors.Wrapf(err, "failed to validate ping probe config")
		}
		err = proberPing.ProbeTickerLoop(ctx, conf.Probe.GetPingProbe())
		if err != nil {
			return err
		}
	}

	return nil
}

func initMeterProvider(ctx context.Context) (func(context.Context) error, error) {
	exportInterval := 10 * time.Second

	expOtlpHTTP, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	readerOtlpHTTP := sdkmetric.NewPeriodicReader(expOtlpHTTP,
		sdkmetric.WithInterval(exportInterval),
	)

	expStdout, err := stdoutmetric.New()
	if err != nil {
		return nil, err
	}
	readerStdout := sdkmetric.NewPeriodicReader(expStdout,
		sdkmetric.WithInterval(exportInterval),
	)

	readerProm, err := newPromExporter()
	if err != nil {
		return nil, err
	}
	go serveMetrics()

	res, err := resource.New(ctx, resource.WithHost(), resource.WithFromEnv())
	if err != nil {
		return nil, err
	}

	viewExponentialHistogram := sdkmetric.NewView(
		sdkmetric.Instrument{
			Name: "*_latency",
		},
		sdkmetric.Stream{
			Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
				MaxSize:  160,
				MaxScale: 20,
			},
			// Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
			// 	// nanoseconds
			// 	Boundaries: []float64{1e6, 2e6, 5e6, 10e6, 20e6, 50e6, 100e6, 200e6, 500e6, 1000e6},
			// },
		},
	)

	mprovider := sdkmetric.NewMeterProvider(
		sdkmetric.WithView(viewExponentialHistogram),
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
