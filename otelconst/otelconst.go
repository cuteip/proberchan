package otelconst

import (
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

var (
	ExponentialHistogramStream = sdkmetric.Stream{
		Aggregation: sdkmetric.AggregationBase2ExponentialHistogram{
			MaxSize:  160,
			MaxScale: 20,
		},
		// Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
		// 	// nanoseconds
		// 	Boundaries: []float64{1e6, 2e6, 5e6, 10e6, 20e6, 50e6, 100e6, 200e6, 500e6, 1000e6},
		// },
	}
)
