# proberchan

## run

### Grafana Cloud (Japan)

```shell
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="https://otlp-gateway-prod-ap-northeast-0.grafana.net/otlp/v1/metrics"
export BASIC_AUTH_ENCODED=<username:password> # base64 encoded
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic ${BASIC_AUTH_ENCODED}"

./proberchan --log-level info
```
