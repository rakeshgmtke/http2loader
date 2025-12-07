# Architecture Deep Dive

## Module Breakdown

### main.go

#### Data Structures

```go
type API struct {
  Name   string  // Endpoint name
  URL    string  // Target URL
  Method string  // HTTP method
  Body   string  // Request body
}

type Config struct {
  Global struct {
    TPS            float64 // Transactions per second (global)
    MaxInflight    int     // Semaphore slots
    PendingBuf     int     // Queue capacity
    RequestTimeout int     // Per-request timeout (seconds)
    MaxRetries     int     // Retry attempts
    BackoffMs      int     // Exponential backoff base (ms)
    MetricsAddr    string  // Prometheus listen address
    MaxIdlePerHost int     // HTTP/2 conn pool per host
    IdleConnSec    int     // Idle connection timeout (seconds)
  }
  APIs []API
}

type Job struct {
  API API    // Target endpoint
  Seq int64  // Global sequence number
}
```

#### Function: `main()`

**Responsibility**: Initialize and orchestrate all subsystems

**Steps**:
1. Parse command-line flags (`-config path`)
2. Load configuration with defaults
3. Validate at least one API exists
4. Create HTTP/2 client with pooling
5. Initialize channels:
   - `pending`: Job queue (size = `PendingBuf`)
   - `sem`: Semaphore (size = `MaxInflight`)
6. Create context for cancellation
7. Setup signal handler
8. Spawn goroutines:
   - Metrics HTTP server
   - Scheduler (global TPS)
   - Dispatcher (worker pool)
   - Queue monitor
9. Block on `ctx.Done()`
10. Drain queue on shutdown
11. Close idle connections
12. Exit cleanly

**Metrics Updated**: None (orchestrator only)

---

#### Function: `loadConfig(path string)`

**Responsibility**: Parse JSON config with sensible defaults

**Defaults Applied**:
```
TPS              → 100
MaxInflight      → 1000
PendingBuf       → MaxInflight * 2
RequestTimeout   → 15 seconds
MaxRetries       → 1
BackoffMs        → 50 ms
MetricsAddr      → :9090
MaxIdlePerHost   → 200
IdleConnSec      → 90 seconds
```

**Returns**: `*Config` or error

---

#### Function: `schedulerGlobal(ctx, totalTPS, apis, pending)`

**Responsibility**: Distribute jobs at target TPS rate to all APIs round-robin

**Algorithm**:
```
Every 1ms:
  1. Accumulate fractional TPS: acc += totalTPS / 1000.0
  2. Extract integer jobs: toEmit = floor(acc)
  3. Reduce accumulator: acc -= float64(toEmit)
  4. For each job:
     - Get next API round-robin
     - Increment global sequence
     - Send to pending queue
```

**Example**:
- TPS=1000 → 1 job/ms (1 per tick)
- TPS=500 → 0.5 jobs/ms (alternates 1, 0, 1, 0...)
- TPS=2500 → 2.5 jobs/ms (2, 2, 3, 2, 3 pattern)

**Metrics Updated**: None (dispatcher monitors queue)

---

#### Function: `dispatcher(ctx, client, pending, sem, inFlightWG, cfg)`

**Responsibility**: Worker pool that drains queue and spawns senders

**Algorithm**:
```
Loop:
  1. If ctx.Done → drain remaining jobs then exit
  2. Read from pending channel
  3. Call startSend(job)
```

**Concurrency**: Single goroutine (serializes queue consumption)

**Metrics Updated**: Via `startSend()`

---

#### Function: `startSend(ctx, job, client, sem, inFlightWG, cfg)`

**Responsibility**: Acquire slot, spawn sender, handle cleanup

**Steps**:
1. Acquire semaphore (blocks if full)
2. Increment `inFlightWG` counter
3. Increment `metricInFlight`
4. Spawn goroutine:
   - Create request context with timeout
   - Record start time
   - Call `sendWithRetry()`
   - Calculate latency
   - Update success/failure metrics
   - Release semaphore
   - Decrement `inFlightWG`
   - Decrement `metricInFlight`

**Metrics Updated**:
- `metricInFlight` (Inc/Dec)
- `metricSent` (on success)
- `metricFailed` (on error)
- `metricLatency` (on success)

---

#### Function: `sendWithRetry(ctx, client, job, maxAttempts, backoffBase)`

**Responsibility**: HTTP request with retry and exponential backoff

**Algorithm**:
```
For attempt = 1 to maxAttempts:
  1. Check context timeout
  2. Create HTTP request
  3. Set Content-Type header
  4. Send request
  5. If success:
     - Drain response body
     - Check status 200-299
     - Return nil on success
  6. If fail:
     - Store error
     - If not last attempt:
       - Sleep: backoffBase + random(0-100ms)
       - Check context
  7. Return last error
```

**Retry Conditions**:
- Network errors
- Non-2xx status codes
- Context timeout (checked per attempt)

**Backoff**: Exponential with jitter
- Base: `backoffMs` (e.g., 50ms)
- Jitter: Random 0-100ms
- Total: backoffBase + jitter

**Metrics Updated**: None (caller handles this)

---

#### Function: `newHTTP2Client(maxIdlePerHost, idleTimeout)`

**Responsibility**: Create HTTP/2 client with optimal pooling

**Configuration**:
- **TLS**: MinVersion = TLS 1.2
- **HTTP/2**: Forced with `ForceAttemptHTTP2 = true`
- **Pooling**:
  - `MaxIdleConns = 10000` (global limit)
  - `MaxIdleConnsPerHost = maxIdlePerHost` (per-host limit, usually 200)
  - `IdleConnTimeout = idleTimeout` (e.g., 90 seconds)
- **Multiplexing**: Automatic via `http2.ConfigureTransport()`

**Returns**: `*http.Client` with HTTP/2 transport

---

#### Function: `waitForSig(cancel func())`

**Responsibility**: Block on SIGINT/SIGTERM and trigger shutdown

**Steps**:
1. Create signal channel (buffered)
2. Notify on SIGINT (Ctrl+C) and SIGTERM
3. Block on signal
4. Print shutdown message
5. Call cancel() → propagates to all goroutines via ctx.Done()

---

### Goroutine Lifecycle

```
main()
├─ scheduler()        ← Generates jobs @ TPS
├─ dispatcher()       ← Consumes queue
│  └─ startSend()
│     └─ sendWithRetry() ← HTTP + retry
├─ metrics server     ← HTTP server :9090
├─ queue monitor      ← Updates metricQueued every 250ms
└─ signal handler     ← Waits for SIGINT/SIGTERM
```

### Concurrency Model

```
pending queue (buffered channel)
    ↓
dispatcher (1 goroutine, serializes queue)
    ↓
startSend (N goroutines, spawned per job)
    ├─ Acquire semaphore (blocks if > MaxInflight)
    ├─ sendWithRetry (HTTP request)
    └─ Release semaphore
```

### Resource Management

| Resource | Limit | Rationale |
|----------|-------|-----------|
| Queue depth | `PendingBuf` | Backpressure |
| Inflight requests | `MaxInflight` | Memory, server capacity |
| Idle conns/host | `MaxIdlePerHost` | Resource efficiency |
| Idle conn lifetime | `IdleConnSec` | Connection reuse |

---

## server/server.go

**Purpose**: Mock HTTP/2 target for load testing

**Features**:
- h2c server (HTTP/2 Cleartext, no TLS)
- Configurable endpoints
- Fixed response body per API
- Graceful shutdown

See `server/README.md` for details.

---

## Error Handling Strategy

| Error Type | Handling |
|-----------|----------|
| Config load | Fatal exit |
| No APIs configured | Fatal exit |
| Network errors | Retry with backoff |
| Non-2xx status | Retry with backoff |
| Request timeout | Context cancellation + retry |
| Context timeout | Abort all pending |
| Metrics server fail | Log, continue |

---

## Performance Tuning

### To increase throughput:
1. Increase `tps` (scheduler emits faster)
2. Increase `max_inflight` (more concurrent requests)
3. Increase `max_idle_per_host` (more connection reuse)
4. Increase `pending_buf` (more queue capacity)

### To reduce latency:
1. Decrease `request_timeout_seconds` (fail fast)
2. Decrease `backoff_ms` (quick retry)
3. Increase `max_retries` (more attempts)

### To improve stability:
1. Set `tps` conservatively (avoid overwhelming target)
2. Monitor `client_in_flight_requests` (watch for saturation)
3. Increase `idle_conn_seconds` (avoid reopen)

---

## Metrics Deep Dive

All metrics are Prometheus-compatible, exported on `/metrics`:

```
# HELP client_pending_queue_length Pending jobs waiting dispatch
# TYPE client_pending_queue_length gauge
client_pending_queue_length 42

# HELP client_in_flight_requests Requests currently in-flight
# TYPE client_in_flight_requests gauge
client_in_flight_requests 128

# HELP client_requests_sent_total Total successful requests
# TYPE client_requests_sent_total counter
client_requests_sent_total{api="api1"} 50000
client_requests_sent_total{api="api2"} 45000

# HELP client_requests_failed_total Total failed requests
# TYPE client_requests_failed_total counter
client_requests_failed_total{api="api1"} 100
client_requests_failed_total{api="api2"} 50

# HELP client_request_duration_seconds Latency distribution
# TYPE client_request_duration_seconds histogram
client_request_duration_seconds_bucket{api="api1",le="0.001"} 1000
client_request_duration_seconds_bucket{api="api1",le="0.002"} 2500
...
client_request_duration_seconds_sum{api="api1"} 1500.5
client_request_duration_seconds_count{api="api1"} 50000
```