# HTTP/2 Load Testing Client

A high-performance HTTP/2 load testing tool written in Go with global TPS control, connection pooling, retry logic, and Prometheus metrics.

## Features

- **Global TPS Control**: Distribute load evenly across all configured APIs
- **HTTP/2 Support**: Native HTTP/2 with connection multiplexing
- **Concurrency Management**: Configurable max in-flight requests with semaphore
- **Retry Logic**: Exponential backoff with jitter for transient failures
- **Prometheus Metrics**: Real-time monitoring of queue depth, latency, success/failure rates
- **Graceful Shutdown**: Drain pending queue on SIGINT/SIGTERM
- **Mock Server**: Included h2c server for testing

## Quick Start

### Prerequisites
- Go 1.19+
- Xcode Command Line Tools (macOS)

### Installation

```bash
cd /Users/rakeshgm/Go\ Learning/http2loader
go mod download
```

### Run Client

```bash
go run main.go -config config.json
```

### Run Mock Server (separate terminal)

```bash
cd server
go run server.go
```

### View Metrics

```
http://localhost:9090/metrics
```

## Configuration (config.json)

```json
{
  "global": {
    "tps": 1000,                    // Total requests/sec across ALL APIs
    "max_inflight": 5000,           // Max concurrent requests
    "pending_buf": 10000,           // Queue buffer size
    "request_timeout_seconds": 15,  // Per-request timeout
    "max_retries": 3,               // Retry attempts
    "backoff_ms": 50,               // Base backoff (with jitter)
    "metrics_addr": ":9090",        // Prometheus metrics port
    "max_idle_per_host": 200,       // HTTP/2 idle conns per host
    "idle_conn_seconds": 90         // Close idle conns after
  },
  "apis": [
    {
      "name": "api1",
      "url": "http://localhost:3000/api1",
      "method": "POST",
      "body": "{\"key\":\"value\"}"
    },
    {
      "name": "api2",
      "url": "http://localhost:3000/api2",
      "method": "GET",
      "body": ""
    }
  ]
}
```

## Architecture

### Components

1. **Scheduler** - Global rate limiter (TPS)
2. **Dispatcher** - Worker pool with semaphore
3. **Sender** - HTTP/2 client with retry logic
4. **Metrics** - Prometheus collector
5. **Mock Server** - Test target (h2c)

### Flow

```
config.json → scheduler (TPS) → pending queue → dispatcher → startSend → sendWithRetry → metrics
                                                    ↑
                                            semaphore (concurrency cap)
```

## Metrics

Exposed on `http://localhost:9090/metrics`:

- `client_pending_queue_length` - Jobs waiting dispatch
- `client_in_flight_requests` - Active requests
- `client_requests_sent_total{api=...}` - Successful requests
- `client_requests_failed_total{api=...}` - Failed requests
- `client_request_duration_seconds{api=...}` - Latency histogram

## Tuning Tips

| Goal | Setting |
|------|---------|
| Higher throughput | Increase `tps`, `max_inflight` |
| Lower latency | Decrease `request_timeout_seconds`, `backoff_ms` |
| Stability | Increase `max_idle_per_host`, `idle_conn_seconds` |
| Error recovery | Increase `max_retries` |

## Graceful Shutdown

Send SIGINT/SIGTERM:
```bash
# Ctrl+C drains pending queue and exits cleanly
```

## License

MIT