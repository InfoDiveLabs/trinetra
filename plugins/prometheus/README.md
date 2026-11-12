# Prometheus exporter

Serves `GET /metrics` in Prometheus text format 0.0.4 on its own listener, and ships a Grafana dashboard.

Permissions: `metrics.read`, `net.listen`, `fleet.read`.

## Settings

| Setting | Default | Meaning |
|---|---|---|
| `listen` | `127.0.0.1:9469` | host:port to bind. Changing it rebinds the listener. |
| `bearer_token` | empty | When set, scrapes need `Authorization: Bearer <token>`; others get 401. |
| `include_fleet` | off | On a fleet master, add per-node series. |

## Metrics

Gauges: `trinetra_snapshot_timestamp_seconds`, `trinetra_cpu_usage_percent`, `trinetra_memory_usage_percent`, `trinetra_swap_usage_percent`, `trinetra_load1`, `trinetra_load5`, `trinetra_load15`, `trinetra_temperature_celsius` (when a sensor exists), `trinetra_disk_{usage_percent,free_bytes,size_bytes}{mount}`, `trinetra_network_{receive,transmit}_bytes_per_second{interface}`.

Container series (`trinetra_containers_running`, `trinetra_containers_total`, `trinetra_container_cpu_usage_percent{name}`, `trinetra_container_memory_mebibytes{name}`) appear when the host also grants `docker.read`.

With `include_fleet`: `trinetra_node_up{node,id}`, `trinetra_node_cpu_usage_percent`, `trinetra_node_memory_usage_percent`, `trinetra_node_worst_disk_usage_percent`.

## Prometheus config

```yaml
scrape_configs:
  - job_name: trinetra
    authorization:
      credentials: <bearer_token>
    static_configs:
      - targets: ["127.0.0.1:9469"]
```

Download `grafana-dashboard.json` from the plugin's card and import it in Grafana.

This plugin is the worked example in [docs/plugins/authoring.md](../../docs/plugins/authoring.md).
