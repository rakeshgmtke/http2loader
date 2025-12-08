package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/sirupsen/logrus" // Import logrus

	"golang.org/x/net/http2"
	"golang.org/x/time/rate"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------- Types ----------

type API struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Method  string `json:"method"`
	Body    string `json:"body"`
	DelayMs int    `json:"delay_ms"`
}

type Config struct {
	Server struct {
		MetricsAddr    string `json:"metrics_addr"`
		MaxIdlePerHost int    `json:"max_idle_per_host"`
		IdleConnSec    int    `json:"idle_conn_seconds"`
		Log            struct {
			Level        string `json:"level"`
			Format       string `json:"format"`
			File         string `json:"file"`
			EnableStdout bool   `json:"enable_stdout"`
		} `json:"log"`
	} `json:"server"`
	Load struct {
		TPS                float64 `json:"tps"`
		TpsRampPercent     float64 `json:"tps_ramp_percent"`
		TpsRampHoldSeconds int     `json:"tps_ramp_hold_seconds"`
		MaxInflight        int     `json:"max_inflight"`
		PendingBuf         int     `json:"pending_buf"`
		RequestTimeout     int     `json:"request_timeout_seconds"`
		TotalMsg           int     `json:"total_msg"` // now PER-API total
	} `json:"load"`
	APIs []API `json:"apis"`
}

type Job struct {
	API API
	Seq int64
}

// ---------- Globals ----------

var globalSeq int64
var inFlight int64

var (
	metricQueued = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "client_pending_queue_length",
		Help: "Pending jobs waiting dispatch",
	})
	metricInFlight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "client_in_flight_requests",
		Help: "Requests currently in-flight",
	})
	metricAttempted = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_requests_attempted_total",
		Help: "Total requests attempted (sent to the network layer)",
	}, []string{"api"})
	metricSuccessful = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_responses_successful_total",
		Help: "Total successful responses (2xx status)",
	}, []string{"api"})
	metricFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_responses_failed_total",
		Help: "Total failed responses (3xx, 4xx, 5xx status)",
	}, []string{"api", "status_class"})
	metricLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "client_request_duration_seconds",
		Help:    "Latency distribution",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
	}, []string{"api"})
)

func init() {
	prometheus.MustRegister(metricQueued, metricInFlight, metricAttempted, metricSuccessful, metricFailed, metricLatency)
}

// ---------- main ----------

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	logLevelFlag := flag.String("log-level", "", "log level override (debug, info, warn, error). If empty, config used.")
	logFileFlag := flag.String("log-file", "", "log file path (overrides config file setting if provided)")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(2)
	}

	// Logging setup
	if cfg.Server.Log.Format == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	}

	// Level: flag overrides config
	levelStr := cfg.Server.Log.Level
	if *logLevelFlag != "" {
		levelStr = *logLevelFlag
	}
	if levelStr == "" {
		levelStr = "info"
	}
	level, err := logrus.ParseLevel(levelStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid log level %q, using info\n", levelStr)
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)

	// Output: flag overrides config
	logFile := cfg.Server.Log.File
	if *logFileFlag != "" {
		logFile = *logFileFlag
	}
	if logFile != "" {
		f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			logrus.Fatalf("failed to open log file: %v", err)
		}
		if cfg.Server.Log.EnableStdout {
			logrus.SetOutput(io.MultiWriter(os.Stdout, f))
		} else {
			logrus.SetOutput(f)
		}
	} else {
		logrus.SetOutput(os.Stdout)
	}

	if len(cfg.APIs) == 0 {
		logrus.Fatalf("No APIs configured")
	}

	client := newHTTP2Client(cfg.Server.MaxIdlePerHost, time.Duration(cfg.Server.IdleConnSec)*time.Second)

	pending := make(chan Job, cfg.Load.PendingBuf)
	sem := make(chan struct{}, cfg.Load.MaxInflight)
	var inFlightWG sync.WaitGroup
	var pendingCloser sync.Once
	closePending := func() {
		pendingCloser.Do(func() { close(pending) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForSig(cancel)

	// Metrics & pprof
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())

		mux.HandleFunc("/debug/pprof/", pprof.Index)
		mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
		mux.Handle("/debug/pprof/block", pprof.Handler("block"))
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))

		addr := cfg.Server.MetricsAddr
		if addr == "" {
			addr = ":9090"
		}
		logrus.Infof("Metrics at %s/metrics, pprof at %s/debug/pprof/", addr, addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			logrus.Errorf("Metrics server error: %v", err)
		}
	}()

	stats := NewStats(cfg.APIs)

	// Scheduler and dispatcher
	go scheduler(ctx, cfg, pending, closePending)
	go dispatcher(ctx, client, pending, sem, &inFlightWG, cfg, stats)

	// Queue size updater
	go func() {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				metricQueued.Set(float64(len(pending)))
			}
		}
	}()

	// Metrics TUI printer
	go func() {
		var lastMetrics map[string]APIMetric
		var lastTime time.Time

		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ordered := stats.GetAPIMetricsOrdered()
				currentTime := time.Now()
				tps := make(map[string]float64)
				avgLatency := make(map[string]float64)

				if lastMetrics != nil {
					duration := currentTime.Sub(lastTime).Seconds()
					for _, entry := range ordered {
						name := entry.Name
						m := entry.Metric
						lastM, ok := lastMetrics[name]
						if !ok {
							continue
						}
						tps[name] = float64(m.TotalResponse-lastM.TotalResponse) / duration
						if m.TotalResponse-lastM.TotalResponse > 0 {
							avgLatency[name] = (m.TotalLatency - lastM.TotalLatency) / float64(m.TotalResponse-lastM.TotalResponse)
						}
					}
				}

				printMetrics(stats, len(pending), tps, avgLatency)

				currentMetrics := make(map[string]APIMetric, len(ordered))
				for _, entry := range ordered {
					currentMetrics[entry.Name] = entry.Metric
				}
				lastMetrics = currentMetrics
				lastTime = currentTime
			}
		}
	}()

	<-ctx.Done()
	logrus.Info("Shutdown requested — draining pending queue")

	closePending()
	inFlightWG.Wait()

	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}

	logrus.Info("Exited cleanly")
}

// ---------- loadConfig ----------

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}

	// server defaults
	if cfg.Server.MetricsAddr == "" {
		cfg.Server.MetricsAddr = ":9090"
	}
	if cfg.Server.MaxIdlePerHost <= 0 {
		cfg.Server.MaxIdlePerHost = 200
	}
	if cfg.Server.IdleConnSec <= 0 {
		cfg.Server.IdleConnSec = 90
	}
	if cfg.Server.Log.Format == "" {
		cfg.Server.Log.Format = "text"
	}
	if cfg.Server.Log.Level == "" {
		cfg.Server.Log.Level = "info"
	}

	// load defaults
	if cfg.Load.TPS <= 0 {
		cfg.Load.TPS = 1000
	}
	if cfg.Load.TpsRampHoldSeconds <= 0 {
		cfg.Load.TpsRampHoldSeconds = 1
	}
	if cfg.Load.MaxInflight <= 0 {
		cfg.Load.MaxInflight = 1000
	}
	if cfg.Load.PendingBuf <= 0 {
		cfg.Load.PendingBuf = cfg.Load.MaxInflight * 2
	}
	if cfg.Load.RequestTimeout <= 0 {
		cfg.Load.RequestTimeout = 15
	}
	if cfg.Load.TotalMsg < 0 {
		cfg.Load.TotalMsg = 0
	}

	return &cfg, nil
}

// scheduler: simple TPS-based scheduler (per-API TPS, per-API total_msg)
// - cfg.Load.TPS is treated as per-API TPS
// - cfg.Load.TotalMsg is treated as per-API total messages
// - scheduler enqueues jobs in round-robin order and only enforces TPS
func scheduler(
	ctx context.Context,
	cfg *Config,
	pending chan<- Job,
	closePending func(),
) {
	apis := cfg.APIs
	apiCount := len(apis)
	if apiCount == 0 {
		logrus.Error("[SCHED] no APIs configured — exiting scheduler")
		if closePending != nil {
			closePending()
		}
		return
	}

	// per-API TPS from config
	perApiTPS := cfg.Load.TPS
	if perApiTPS <= 0 {
		logrus.Error("[SCHED] tps (per API) must be > 0 — exiting scheduler")
		if closePending != nil {
			closePending()
		}
		return
	}

	// total messages per API from config; interpret as per-API total
	totalPerAPI := cfg.Load.TotalMsg
	if totalPerAPI < 0 {
		totalPerAPI = 0
	}

	// total messages across all APIs (used for stopping)
	totalMessages := 0
	if totalPerAPI > 0 {
		totalMessages = totalPerAPI * apiCount
	}

	// ramp settings (unchanged semantics)
	rampPercent := cfg.Load.TpsRampPercent
	holdSeconds := cfg.Load.TpsRampHoldSeconds
	useRamp := rampPercent > 0 && holdSeconds > 0

	// Counters
	sent := 0   // total jobs enqueued across all APIs
	apiIdx := 0 // round-robin index

	// Helper to check stop condition
	reachedTotal := func() bool {
		return totalMessages > 0 && sent >= totalMessages
	}

	// ---------------- RAMP PHASE ----------------
	if useRamp {
		stepFraction := rampPercent / 100.0
		if stepFraction <= 0 {
			stepFraction = 0.10
		}

		currentPerApiTPS := perApiTPS * stepFraction
		if currentPerApiTPS < 1 {
			currentPerApiTPS = 1
		}

		for currentPerApiTPS < perApiTPS && !reachedTotal() {
			// compute global TPS for limiter (perApi * apiCount)
			currentGlobalTPS := currentPerApiTPS * float64(apiCount)
			if currentGlobalTPS <= 0 {
				currentGlobalTPS = 1
			}

			/*burst := int(currentGlobalTPS)
			if burst < 1 {
				burst = 1
			}*/
			burst := apiCount
			if burst < 1 {
				burst = 1
			}

			limiter := rate.NewLimiter(rate.Limit(currentGlobalTPS), burst)

			stepStart := time.Now()
			stepEnd := stepStart.Add(time.Duration(holdSeconds) * time.Second)
			logrus.Infof("[SCHED] ramp step: perApi=%.2f global=%.2f from %s to %s",
				currentPerApiTPS, currentGlobalTPS, stepStart.Format(time.RFC3339), stepEnd.Format(time.RFC3339))

			for time.Now().Before(stepEnd) && !reachedTotal() {
				if err := limiter.Wait(ctx); err != nil {
					logrus.Warn("[SCHED] cancelled during ramp: ", err)
					if closePending != nil {
						closePending()
					}
					return
				}

				api := apis[apiIdx]
				apiIdx = (apiIdx + 1) % apiCount

				seq := atomic.AddInt64(&globalSeq, 1)
				select {
				case pending <- Job{API: api, Seq: seq}:
					sent++
					// optional debug logging
					if sent <= 10 || sent%1000 == 0 {
						logrus.Debugf("[SCHED][RAMP] enqueued api=%s seq=%d sent=%d/%d", api.Name, seq, sent, totalMessages)
					}
				case <-ctx.Done():
					logrus.Warn("[SCHED] context done during ramp")
					if closePending != nil {
						closePending()
					}
					return
				}
			}

			currentPerApiTPS += perApiTPS * stepFraction
			if currentPerApiTPS > perApiTPS {
				currentPerApiTPS = perApiTPS
			}
		}
	}

	// ---------------- STEADY-STATE ----------------
	finalGlobalTPS := perApiTPS * float64(apiCount)
	if finalGlobalTPS <= 0 {
		finalGlobalTPS = 1
	}
	/*burst := int(finalGlobalTPS)
	if burst < 1 {
		burst = 1
	}*/
	burst := apiCount
	if burst < 1 {
		burst = 1
	}
	limiter := rate.NewLimiter(rate.Limit(finalGlobalTPS), burst)

	logrus.Infof("[SCHED] steady state: perApi=%.2f global=%.2f per_api_total=%d total_messages=%d",
		perApiTPS, finalGlobalTPS, totalPerAPI, totalMessages)

	for !reachedTotal() {
		if err := limiter.Wait(ctx); err != nil {
			logrus.Warn("[SCHED] cancelled in steady state: ", err)
			if closePending != nil {
				closePending()
			}
			return
		}

		api := apis[apiIdx]
		apiIdx = (apiIdx + 1) % apiCount

		seq := atomic.AddInt64(&globalSeq, 1)
		select {
		case pending <- Job{API: api, Seq: seq}:
			sent++
			if sent <= 10 || sent%1000 == 0 {
				logrus.Debugf("[SCHED][STEADY] enqueued api=%s seq=%d sent=%d/%d", api.Name, seq, sent, totalMessages)
			}
		case <-ctx.Done():
			logrus.Warn("[SCHED] context done in steady state")
			if closePending != nil {
				closePending()
			}
			return
		}
	}

	logrus.Infof("[SCHED] completed sending %d messages (%d per API) — closing", totalMessages, totalPerAPI)
	if closePending != nil {
		closePending()
	}
}

// ---------- scheduler ----------
/*
func scheduler(
	ctx context.Context,
	cfg *Config,
	pending chan<- Job,
	closePending func(),
) {
	apis := cfg.APIs
	apiCount := len(apis)
	if apiCount == 0 {
		logrus.Error("[SCHED] no APIs configured — exiting scheduler")
		if closePending != nil {
			closePending()
		}
		return
	}

	// TPS is per-API
	perApiTPS := cfg.Load.TPS
	if perApiTPS <= 0 {
		logrus.Error("[SCHED] tps (per API) must be > 0 — exiting scheduler")
		if closePending != nil {
			closePending()
		}
		return
	}

	// TOTAL messages = per-API total * number of APIs
	totalPerAPI := cfg.Load.TotalMsg
	totalMessages := 0
	if totalPerAPI > 0 {
		totalMessages = totalPerAPI * apiCount
	}

	rampPercent := cfg.Load.TpsRampPercent
	holdSeconds := cfg.Load.TpsRampHoldSeconds

	apiIdx := 0
	sent := 0

	useRamp := rampPercent > 0 && holdSeconds > 0

	// ---------------- RAMP PHASE ----------------
	if useRamp {
		stepFraction := rampPercent / 100.0
		if stepFraction <= 0 {
			stepFraction = 0.10
		}

		currentPerApiTPS := perApiTPS * stepFraction
		if currentPerApiTPS < 1 {
			currentPerApiTPS = 1
		}

		for currentPerApiTPS < perApiTPS && (totalMessages == 0 || sent < totalMessages) {
			currentGlobalTPS := currentPerApiTPS * float64(apiCount)

			burst := int(currentGlobalTPS)
			if burst < 1 {
				burst = 1
			}
			limiter := rate.NewLimiter(rate.Limit(currentGlobalTPS), burst)

			stepStart := time.Now()
			stepEnd := stepStart.Add(time.Duration(holdSeconds) * time.Second)

			logrus.Infof(
				"[SCHED] ramp step: %.2f TPS/api (%.2f global) from %s to %s",
				currentPerApiTPS,
				currentGlobalTPS,
				stepStart.Format(time.RFC3339),
				stepEnd.Format(time.RFC3339),
			)

			for time.Now().Before(stepEnd) && (totalMessages == 0 || sent < totalMessages) {
				if err := limiter.Wait(ctx); err != nil {
					logrus.Warn("[SCHED] cancelled during ramp: ", err)
					return
				}

				api := apis[apiIdx]
				apiIdx = (apiIdx + 1) % apiCount

				seq := atomic.AddInt64(&globalSeq, 1)

				select {
				case pending <- Job{API: api, Seq: seq}:
					sent++
					logrus.Debugf(
						"[SCHED][RAMP] api=%s seq=%d sent=%d/%d tps/api=%.2f",
						api.Name, seq, sent, totalMessages, currentPerApiTPS,
					)
				case <-ctx.Done():
					logrus.Warn("[SCHED] context done during ramp")
					if closePending != nil {
						closePending()
					}
					return
				}
			}

			currentPerApiTPS += perApiTPS * stepFraction
			if currentPerApiTPS > perApiTPS {
				currentPerApiTPS = perApiTPS
			}
		}
	}

	// ---------------- STEADY-STATE ----------------
	finalGlobalTPS := perApiTPS * float64(apiCount)
	burst := int(finalGlobalTPS)
	if burst < 1 {
		burst = 1
	}
	limiter := rate.NewLimiter(rate.Limit(finalGlobalTPS), burst)

	logrus.Infof(
		"[SCHED] steady state: %.2f TPS/api (%.2f global), per_api_total=%d total_messages=%d",
		perApiTPS,
		finalGlobalTPS,
		totalPerAPI,
		totalMessages,
	)

	for totalMessages == 0 || sent < totalMessages {
		if err := limiter.Wait(ctx); err != nil {
			logrus.Warn("[SCHED] cancelled in steady state: ", err)
			return
		}

		api := apis[apiIdx]
		apiIdx = (apiIdx + 1) % apiCount

		seq := atomic.AddInt64(&globalSeq, 1)

		select {
		case pending <- Job{API: api, Seq: seq}:
			sent++
			logrus.Debugf(
				"[SCHED][STEADY] api=%s seq=%d sent=%d/%d",
				api.Name, seq, sent, totalMessages,
			)
		case <-ctx.Done():
			logrus.Warn("[SCHED] context done in steady state")
			if closePending != nil {
				closePending()
			}
			return
		}
	}

	logrus.Infof("[SCHED] completed sending %d messages (per api=%d) — closing", sent, totalPerAPI)
	if closePending != nil {
		closePending()
	}
}
*/
// ---------- dispatcher & send ----------

func dispatcher(ctx context.Context, client *http.Client, pending <-chan Job, sem chan struct{},
	inFlightWG *sync.WaitGroup, cfg *Config, stats *Stats) {
	for {
		select {
		case <-ctx.Done():
			for job := range pending {
				startSend(ctx, job, client, sem, inFlightWG, cfg, stats)
			}
			return

		case job, ok := <-pending:
			if !ok {
				return
			}
			startSend(ctx, job, client, sem, inFlightWG, cfg, stats)
		}
	}
}

func startSend(ctx context.Context, job Job, client *http.Client, sem chan struct{},
	inFlightWG *sync.WaitGroup, cfg *Config, stats *Stats) {

	inFlightWG.Add(1)

	go func(j Job) {
		defer inFlightWG.Done()

		// Acquire semaphore FIRST (before delay)
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}

		defer func() {
			<-sem
			metricInFlight.Dec()
			atomic.AddInt64(&inFlight, -1)
		}()

		metricInFlight.Inc()
		atomic.AddInt64(&inFlight, 1)
		metricAttempted.WithLabelValues(j.API.Name).Inc()

		// Apply delay before dispatching the request
		if j.API.DelayMs > 0 {
			time.Sleep(time.Duration(j.API.DelayMs) * time.Millisecond)
		}

		// Single-attempt send (no retries)
		rctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Load.RequestTimeout)*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(rctx, j.API.Method, j.API.URL, bytes.NewReader([]byte(j.API.Body)))
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "client_error").Inc()
			stats.IncClientError(j.API.Name)
			logrus.Errorf("[ERR] %s seq=%d err=%v\n", j.API.Name, j.Seq, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		logrus.Debugf("[SEND] time=%s api=%s seq=%d method=%s url=%s attempt=1",
			time.Now().Format(time.RFC3339Nano), j.API.Name, j.Seq, j.API.Method, j.API.URL)

		start := time.Now()
		resp, err := client.Do(req)
		stats.IncTotalSent(j.API.Name)
		lat := time.Since(start).Seconds()
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "transport_error").Inc()
			stats.IncClientError(j.API.Name)
			logrus.Errorf("[ERR] %s seq=%d err=%v\n", j.API.Name, j.Seq, err)
			return
		}

		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		stats.IncTotalResponse(j.API.Name)
		stats.AddLatency(j.API.Name, lat)
		metricLatency.WithLabelValues(j.API.Name).Observe(lat)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			stats.IncSuccess(j.API.Name)
			metricSuccessful.WithLabelValues(j.API.Name).Inc()
		} else if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			stats.IncRedirect(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "3xx").Inc()
		} else if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			stats.IncClientError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "4xx").Inc()
		} else if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			stats.IncServerError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "5xx").Inc()
		}
	}(job)
}

// ---------- HTTP2 client ----------

func newHTTP2Client(maxIdlePerHost int, idleTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // kept original behavior
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     idleTimeout,
	}
	_ = http2.ConfigureTransport(tr)
	return &http.Client{Transport: tr, Timeout: 0}
}

// ---------- utils ----------

func printMetrics(stats *Stats, pending int, tps map[string]float64, avgLatency map[string]float64) {
	now := time.Now()
	timestamp := now.Format("15:04:05")
	fmt.Print("\033[H\033[2J")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "--- Live Stats --- ", timestamp)
	fmt.Fprintln(w, "API\tTPS\tSent\tResponses\t2xx\t3xx\t4xx\t5xx\tAvg Latency (ms)")

	apiMetrics := stats.GetAPIMetricsOrdered()
	for _, entry := range apiMetrics {
		name := entry.Name
		m := entry.Metric
		fmt.Fprintf(w, "%s\t%.2f\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\n",
			name, tps[name], m.TotalSent, m.TotalResponse, m.Success, m.Redirect, m.ClientError, m.ServerError, avgLatency[name]*1000)
	}

	fmt.Fprintln(w, "--------------------")
	fmt.Fprintf(w, "Pending queue:\t%d\n", pending)
	fmt.Fprintf(w, "In-flight:\t%d\n", atomic.LoadInt64(&inFlight))
	w.Flush()
}

func waitForSig(cancel func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	fmt.Println("Signal received: shutting down…")
	cancel()
}
