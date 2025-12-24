# proberchan

## run

### Grafana Cloud (Japan)

```shell
export OTEL_EXPORTER_OTLP_METRICS_ENDPOINT="https://otlp-gateway-prod-ap-northeast-0.grafana.net/otlp/v1/metrics"
export BASIC_AUTH_ENCODED=<username:password> # base64 encoded
export OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic ${BASIC_AUTH_ENCODED}"

# service.name: job
# service.instance.id: instance
# https://opentelemetry.io/docs/specs/otel/compatibility/prometheus_and_openmetrics/#resource-attributes
export OTEL_RESOURCE_ATTRIBUTES="service.name=proberchan,service.instance.id=$(cat /etc/hostname)"

./proberchan --log-level info
```

## Network namespaces

Each probe entry can optionally set `netns` to the name managed by `ip netns`.
When specified, DNS resolution and probe traffic are executed inside that namespace via [`github.com/vishvananda/netns`](https://github.com/vishvananda/netns), which requires `CAP_NET_ADMIN` on the host process.
Example:

```yaml
probes:
  - name: "ping_v4"
    type: "ping"
    netns: "tenant-blue"
    ping:
      # ...
```

## dev memo

```shell
git tag $(date --utc +%Y%m%d-%H%M%S)-$(git rev-parse --short HEAD)
```
