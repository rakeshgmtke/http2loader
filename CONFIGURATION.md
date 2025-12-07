# Configuration Guide

## config.json Structure

```json
{
  "global": {
    "tps": 1000,
    "max_inflight": 5000,
    "pending_buf": 10000,
    "request_timeout_seconds": 15,
    "max_retries": 3,
    "backoff_ms": 50,
    "metrics_addr": ":9090",
    "max_idle_per_host": 200,
    "idle_conn_seconds": 90
  },
  "apis": [
    {
      "name": "create_order",
      "url": "http://api.example.com/orders",
      "method": "POST",
      "body": "{\"user_id\":123,\"items\":[1,2,3]}"
    },
    {
      "name": "get_status",
      "url": "http://api.example.com/status",
      "method": "GET",
      "body": ""
    }
  ]
}
```

## Global Settings Reference

### tps (float64)
**Description**: Total requests per second across ALL APIs combined  
**Default**: 100  
**Range**: 0.1 - 100000+  
**Notes**:
- Distributes load round-robin across configured APIs
- With 2 APIs @ 1000 TPS: each API gets ~500 req/sec
- Fractional TPS supported (e.g., 0.5 = 1 req every 2 seconds)

**Example**:
```json
"tps": 5000  // 5000 total requests/sec
```

---

### max_inflight (int)
**Description**: Maximum concurrent HTTP requests  
**Default**: 1000  
**Range**: 1 - 100000+  
**Notes**:
- Enforced via semaphore in `startSend()`
- Limits memory and server load
- Should be <= target server capacity
- Monitor `client_in_flight_requests` metric

**Tuning**:
```json
"max_inflight": 500   // Conservative: ~100ms @ 5000 TPS
"max_inflight": 10000 // Aggressive: high throughput
```

---

### pending_buf (int)
**Description**: Job queue buffer size  
**Default**: max_inflight * 2  
**Range**: 100 - 1000000  
**Notes**:
- Queue between scheduler and dispatcher
- If full, scheduler blocks (backpressure)
- Monitor `client_pending_queue_length` metric
- Larger = more memory, more latency

**Tuning**:
```json
"pending_buf": 5000   // 2.5 seconds @ 2000 TPS
```

---

### request_timeout_seconds (int)
**Description**: Per-request HTTP timeout  
**Default**: 15  
**Range**: 1 - 300  
**Notes**:
- Timeout for single HTTP request (not including retries)
- Context timeout, not HTTP header
- Applied in `sendWithRetry()` per attempt

**Tuning**:
```json
"request_timeout_seconds": 5   // Fail fast on slow servers
"request_timeout_seconds": 30  // Allow slow responses
```

---

### max_retries (int)
**Description**: Number of retry attempts on failure  
**Default**: 1  
**Range**: 0 - 10  
**Notes**:
- Total attempts = max_retries + 1 (initial + retries)
- Retries on: network errors, non-2xx status
- Each retry includes backoff + jitter

**Examples**:
```json
"max_retries": 0   // No retries
"max_retries": 3   // Up to 4 total attempts
```

---

### backoff_ms (int)
**Description**: Base exponential backoff in milliseconds  
**Default**: 50  
**Range**: 0 - 5000  
**Notes**:
- Actual sleep = backoff_ms + random(0-100ms)
- Applied between retry attempts
- Reduces thundering herd on target

**Timing Example** (backoff_ms=50):
```
Attempt 1: immediate
Attempt 2: sleep 50-150ms
Attempt 3: sleep 50-150ms
Attempt 4: sleep 50-150ms
```

---

### metrics_addr (string)
**Description**: Prometheus metrics server listen address  
**Default**: ":9090"  
**Format**: ":port" or "host:port"  
**Notes**:
- Exposes `/metrics` endpoint
- Use `:9090` for localhost only
- Use `0.0.0.0:9090` for all interfaces

**Examples**:
```json
"metrics_addr": ":9090"           // Localhost only
"metrics_addr": "0.0.0.0:9090"    // All interfaces
"metrics_addr": "127.0.0.1:8080"  // Specific interface
```

---

### max_idle_per_host (int)
**Description**: Max idle HTTP/2 connections per host  
**Default**: 200  
**Range**: 1 - 10000  
**Notes**:
- HTTP/2 multiplexes streams over single connection
- Limits idle connection pool per hostname
- With HTTP/2, can be lower (streams multiplexed)
- Larger = more memory but better reuse

**Tuning**:
```json
"max_idle_per_host": 100   // Conservative
"max_idle_per_host": 500   // Aggressive
```

---

### idle_conn_seconds (int)
**Description**: Idle connection timeout in seconds  
**Default**: 90  
**Range**: 10 - 3600  
**Notes**:
- Closes unused connections after N seconds
- Prevents stale connection reuse
- Shorter = more connection overhead
- Longer = more memory but better reuse

**Tuning**:
```json
"idle_conn_seconds": 30   // Aggressive cleanup
"idle_conn_seconds": 300  // Long-lived connections
```

---

## APIs Configuration

### API Object Structure

```json
{
  "name": "api_name",       // Unique identifier (label in metrics)
  "url": "http://...",      // Full URL (scheme + host + path)
  "method": "POST|GET|PUT", // HTTP method
  "body": "{...}"           // Request body (JSON string or empty)
}
```

### Fields

#### name
- **Type**: string
- **Required**: Yes (used in metrics labels)
- **Example**: `"create_order"`, `"get_user"`, `"health_check"`

#### url
- **Type**: string (URL)
- **Required**: Yes
- **Format**: Must include scheme (`http://` or `https://`)
- **Examples**:
  ```json
  "url": "http://localhost:3000/api/v1/orders"
  "url": "https://api.example.com/users"
  ```

#### method
- **Type**: string
- **Required**: Yes
- **Valid**: `GET`, `POST`, `PUT`, `DELETE`, `PATCH`, etc.
- **Default behavior**: Sets HTTP method, no automatic body handling

#### body
- **Type**: string (JSON-encoded)
- **Required**: No (empty string `""` for GET)
- **Format**: 
  - For JSON payload: `"{\"key\":\"value\"}"`
  - For GET: `""`
  - Raw string (must be valid JSON if content-type is application/json)

**Examples**:
```json
{
  "name": "create_order",
  "url": "http://api.local/orders",
  "method": "POST",
  "body": "{\"user_id\":123,\"amount\":99.99}"
}
```

```json
{
  "name": "get_health",
  "url": "http://api.local/health",
  "method": "GET",
  "body": ""
}
```

---

## Configuration Profiles

### Low Load (Testing)
```json
{
  "global": {
    "tps": 10,
    "max_inflight": 10,
    "pending_buf": 100,
    "request_timeout_seconds": 10,
    "max_retries": 1,
    "backoff_ms": 50,
    "metrics_addr": ":9090",
    "max_idle_per_host": 10,
    "idle_conn_seconds": 90
  },
  "apis": [{"name": "test", "url": "http://localhost:3000/test", "method": "GET", "body": ""}]
}
```

### Medium Load (Staging)
```json
{
  "global": {
    "tps": 1000,
    "max_inflight": 2000,
    "pending_buf": 5000,
    "request_timeout_seconds": 15,
    "max_retries": 2,
    "backoff_ms": 50,
    "metrics_addr": ":9090",
    "max_idle_per_host": 100,
    "idle_conn_seconds": 120
  },
  "apis": [
    {"name": "api1", "url": "http://api.staging/v1/resource", "method": "POST", "body": "..."},
    {"name": "api2", "url": "http://api.staging/v1/health", "method": "GET", "body": ""}
  ]
}
```

### High Load (Production)
```json
{
  "global": {
    "tps": 10000,
    "max_inflight": 10000,
    "pending_buf": 50000,
    "request_timeout_seconds": 20,
    "max_retries": 3,
    "backoff_ms": 100,
    "metrics_addr": "0.0.0.0:9090",
    "max_idle_per_host": 300,
    "idle_conn_seconds": 180
  },
  "apis": [
    {"name": "api1", "url": "https://api.prod/v2/orders", "method": "POST", "body": "..."},
    {"name": "api2", "url": "https://api.prod/v2/stats", "method": "GET", "body": ""},
    {"name": "api3", "url": "https://api.prod/v2/events", "method": "PUT", "body": "..."}
  ]
}
```

---

## Validation & Defaults

When loading config, if a field is missing or invalid:

| Field | Default | Min Check |
|-------|---------|-----------|
| tps | 100 | > 0 |
| max_inflight | 1000 | > 0 |
| pending_buf | max_inflight * 2 | > 0 |
| request_timeout_seconds | 15 | > 0 |
| max_retries | 1 | >= 0 |
| backoff_ms | 50 | > 0 |
| metrics_addr | ":9090" | (none) |
| max_idle_per_host | 200 | > 0 |
| idle_conn_seconds | 90 | > 0 |

**Note**: At least one API must be configured, or `main()` exits with fatal error.

---

## Common Issues

### Issue: Queue keeps growing (pending_buf stays high)
**Cause**: Dispatcher can't keep up with scheduler  
**Solution**: 
- Increase `max_inflight` (more concurrent workers)
- Decrease `tps` (less aggressive scheduling)
- Check target server capacity

### Issue: High latency spikes
**Cause**: Requests timing out or getting retried  
**Solution**:
- Increase `request_timeout_seconds`
- Check target server load
- Increase `max_idle_per_host` for connection reuse

### Issue: Memory grows unbounded
**Cause**: Queue overflow (too many pending jobs)  
**Solution**:
- Decrease `pending_buf`
- Decrease `tps`
- Increase `max_inflight`

### Issue: Metrics server unreachable
**Cause**: Bind failed (port already in use)  
**Solution**:
- Change `metrics_addr` to different port
- Kill process using port: `lsof -i :9090 | grep LISTEN | awk '{print $2}' | xargs kill`