package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	// "log" // Remove standard log package
	"math"
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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// API describes a target endpoint (no TPS here — global TPS used)
type API struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method"`
	Body   string `json:"body"`
	// Delay in milliseconds to wait before dispatching this API's request.
	DelayMs int `json:"delay_ms"`
}

// Config defines configuration fields
type Config struct {
	Global struct {
		TPS            float64 `json:"tps"` // TPS per API (each API receives this rate)
		MaxInflight    int     `json:"max_inflight"`
		PendingBuf     int     `json:"pending_buf"`
		RequestTimeout int     `json:"request_timeout_seconds"`
		MaxRetries     int     `json:"max_retries"`
		BackoffMs      int     `json:"backoff_ms"`
		MetricsAddr    string  `json:"metrics_addr"`
		// Total number of messages to send across all APIs. If 0, scheduler runs indefinitely according to TPS.
		TotalMsg       int `json:"total_msg"`
		MaxIdlePerHost int `json:"max_idle_per_host"`
		IdleConnSec    int `json:"idle_conn_seconds"`
	} `json:"global"`
	APIs []API `json:"apis"`
}

type Job struct {
	API API
	Seq int64
}

var globalSeq int64
var inFlight int64

// printMetrics prints the statistics every second.
func printMetrics(stats *Stats, pending int, tps map[string]float64, avgLatency map[string]float64) {
	// Clear screen
	fmt.Print("\033[H\033[2J")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "--- Live Stats ---")
	fmt.Fprintln(w, "API\tTPS\tSent\tResponses\t2xx\t3xx\t4xx\t5xx\tAvg Latency (ms)")

	// Use ordered metrics snapshot so UI shows APIs in configured order.
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

// Metrics
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

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	logLevelStr := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	logFile := flag.String("log-file", "", "path to log file (e.g., app.log)")
	flag.Parse()

	// Configure Logrus
	logrus.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})
	level, err := logrus.ParseLevel(*logLevelStr)
	if err != nil {
		logrus.Fatalf("invalid log level: %v", err)
	}
	logrus.SetLevel(level)

	if *logFile != "" {
		f, err := os.OpenFile(*logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644)
		if err != nil {
			logrus.Fatalf("failed to open log file: %v", err)
		}
		mw := io.MultiWriter(os.Stdout, f)
		logrus.SetOutput(mw)
	} else {
		logrus.SetOutput(os.Stdout)
	}

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logrus.Fatalf("failed to load config: %v", err)
	}

	if len(cfg.APIs) == 0 {
		logrus.Fatalf("No APIs configured")
	}

	client := newHTTP2Client(cfg.Global.MaxIdlePerHost, time.Duration(cfg.Global.IdleConnSec)*time.Second)

	pending := make(chan Job, cfg.Global.PendingBuf)
	sem := make(chan struct{}, cfg.Global.MaxInflight)
	var inFlightWG sync.WaitGroup
	var pendingCloser sync.Once
	closePending := func() {
		pendingCloser.Do(func() { close(pending) })
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForSig(cancel)

	// Start metrics server with pprof
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		// Register pprof handlers properly using http package
		mux.HandleFunc("/debug/pprof/", http.HandlerFunc(pprof.Index))
		mux.HandleFunc("/debug/pprof/cmdline", http.HandlerFunc(pprof.Cmdline))
		mux.HandleFunc("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))
		mux.HandleFunc("/debug/pprof/symbol", http.HandlerFunc(pprof.Symbol))
		mux.HandleFunc("/debug/pprof/trace", http.HandlerFunc(pprof.Trace))
		mux.Handle("/debug/pprof/heap", pprof.Handler("heap"))
		mux.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
		mux.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))
		mux.Handle("/debug/pprof/block", pprof.Handler("block"))
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex"))
		logrus.Infof("Metrics at %s/metrics, pprof at %s/debug/pprof/", cfg.Global.MetricsAddr, cfg.Global.MetricsAddr)
		if err := http.ListenAndServe(cfg.Global.MetricsAddr, mux); err != nil {
			logrus.Errorf("Metrics server error: %v", err)
		}
	}()

	stats := NewStats(cfg.APIs)
	// Scheduler (global TPS)
	go schedulerGlobal(ctx, cfg.Global.TPS, cfg.APIs, pending, cfg.Global.TotalMsg, closePending)

	// Dispatcher
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

	// Metrics printer
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
					// Use ordered snapshot to compute TPS and average latency in configured order.
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

					// Convert ordered snapshot into map for next iteration's diffing.
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

	// Ensure pending is closed (scheduler may already have closed it)
	closePending()
	inFlightWG.Wait()

	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}

	logrus.Info("Exited cleanly")
}

func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}

	// Defaults
	if cfg.Global.TPS <= 0 {
		cfg.Global.TPS = 1000
	}
	if cfg.Global.MaxInflight <= 0 {
		cfg.Global.MaxInflight = 1000
	}
	if cfg.Global.PendingBuf <= 0 {
		cfg.Global.PendingBuf = cfg.Global.MaxInflight * 2
	}
	if cfg.Global.RequestTimeout <= 0 {
		cfg.Global.RequestTimeout = 15
	}
	if cfg.Global.MaxRetries < 0 {
		cfg.Global.MaxRetries = 1
	}
	if cfg.Global.BackoffMs <= 0 {
		cfg.Global.BackoffMs = 50
	}
	if cfg.Global.MetricsAddr == "" {
		cfg.Global.MetricsAddr = ":9090"
	}
	if cfg.Global.MaxIdlePerHost <= 0 {
		cfg.Global.MaxIdlePerHost = 200
	}
	if cfg.Global.IdleConnSec <= 0 {
		cfg.Global.IdleConnSec = 90
	}

	if cfg.Global.TotalMsg < 0 {
		cfg.Global.TotalMsg = 0
	}

	return &cfg, nil
}

func schedulerGlobal(ctx context.Context, totalTPS float64, apis []API, pending chan<- Job, totalRounds int, closePending func()) {
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	// Queue of delayed jobs (API, sequence, readyTime)
	type delayedJob struct {
		api   API
		seq   int64
		ready time.Time
	}
	delayedQueue := make([]delayedJob, 0, 100)

	acc := 0.0
	apiIdx := 0
	count := len(apis)
	var emitted int64
	var totalToEmit int64
	if totalRounds > 0 {
		totalToEmit = int64(totalRounds) * int64(count)
	}

	// Emit an initial burst: round(TPS) jobs per second per API (burst across all APIs)
	if totalTPS > 0 && count > 0 {
		burstPerSec := int(math.Round(totalTPS))
		burst := burstPerSec * count
		for i := 0; i < burst; i++ {
			api := apis[i%count]
			seq := atomic.AddInt64(&globalSeq, 1)
			if api.DelayMs > 0 {
				delayedQueue = append(delayedQueue, delayedJob{api, seq, time.Now().Add(time.Duration(api.DelayMs) * time.Millisecond)})
				continue
			}
			job := Job{API: api, Seq: seq}

			select {
			case pending <- job:
				if totalRounds > 0 {
					logrus.Debugf("[SCHED] queued api=%s seq=%d emitted=%d/%d", api.Name, seq, emitted+1, totalToEmit)
				} else {
					logrus.Debugf("[SCHED] queued api=%s seq=%d", api.Name, seq)
				}
				if totalRounds > 0 {
					emitted++
					if emitted >= totalToEmit {
						logrus.Infof("[SCHED] reached total rounds=%d (total messages=%d), stopping scheduler", totalRounds, totalToEmit)
						if closePending != nil {
							closePending()
						}
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()

			// Process delayed queue: emit jobs whose delay has expired
			var remaining []delayedJob
			for _, dj := range delayedQueue {
				if now.After(dj.ready) {
					job := Job{API: dj.api, Seq: dj.seq}
					select {
					case pending <- job:
						if totalRounds > 0 {
							logrus.Debugf("[SCHED] queued api=%s seq=%d emitted=%d/%d (delayed)", dj.api.Name, dj.seq, emitted+1, totalToEmit)
						}
						if totalRounds > 0 {
							emitted++
							if emitted >= totalToEmit {
								logrus.Infof("[SCHED] reached total rounds=%d (total messages=%d), stopping scheduler", totalRounds, totalToEmit)
								if closePending != nil {
									closePending()
								}
								return
							}
						}
					case <-ctx.Done():
						return
					}
				} else {
					remaining = append(remaining, dj)
				}
			}
			delayedQueue = remaining

			// totalTPS is now interpreted as per-API TPS, scale by number of APIs
			acc += (totalTPS * float64(count)) / 1000.0
			toEmit := int(math.Floor(acc))
			if toEmit <= 0 {
				continue
			}
			acc -= float64(toEmit)

			for i := 0; i < toEmit; i++ {
				api := apis[apiIdx]
				apiIdx = (apiIdx + 1) % count

				seq := atomic.AddInt64(&globalSeq, 1)

				// Queue delayed jobs separately
				if api.DelayMs > 0 {
					delayedQueue = append(delayedQueue, delayedJob{api, seq, now.Add(time.Duration(api.DelayMs) * time.Millisecond)})
					continue
				}

				job := Job{API: api, Seq: seq}

				select {
				case pending <- job:
					if totalRounds > 0 {
						logrus.Debugf("[SCHED] queued api=%s seq=%d emitted=%d/%d", api.Name, seq, emitted+1, totalToEmit)
					} else {
						logrus.Debugf("[SCHED] queued api=%s seq=%d", api.Name, seq)
					}
					if totalRounds > 0 {
						emitted++
						if emitted >= totalToEmit {
							logrus.Infof("[SCHED] reached total rounds=%d (total messages=%d), stopping scheduler", totalRounds, totalToEmit)
							if closePending != nil {
								closePending()
							}
							return
						}
					}
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

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
		metricAttempted.WithLabelValues(j.API.Name).Inc() // Increment attempted requests
		// Single-attempt send (no retries)
		rctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Global.RequestTimeout)*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(rctx, j.API.Method, j.API.URL, bytes.NewReader([]byte(j.API.Body)))
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "client_error").Inc() // Label for request creation error
			stats.IncClientError(j.API.Name) // Treat request creation errors as client errors
			logrus.Errorf("[ERR] %s seq=%d err=%v\n", j.API.Name, j.Seq, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		logrus.Debugf("[SEND] time=%s api=%s seq=%d method=%s url=%s attempt=1",
			time.Now().Format(time.RFC3339Nano), j.API.Name, j.Seq, j.API.Method, j.API.URL)

		start := time.Now()
		resp, err := client.Do(req)
		lat := time.Since(start).Seconds()
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "transport_error").Inc() // Label for transport error
			stats.IncClientError(j.API.Name) // Treat transport errors as client errors
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
			metricSuccessful.WithLabelValues(j.API.Name).Inc() // Now uses metricSuccessful
		} else if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			stats.IncRedirect(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "3xx").Inc() // Label for 3xx
		} else if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			stats.IncClientError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "4xx").Inc() // Label for 4xx
		} else if resp.StatusCode >= 500 && resp.StatusCode < 600 {
			stats.IncServerError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "5xx").Inc() // Label for 5xx
		}
	}(job)
}

func newHTTP2Client(maxIdlePerHost int, idleTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // Skip cert verification for self-signed certs
		},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: maxIdlePerHost,
		IdleConnTimeout:     idleTimeout,
	}
	_ = http2.ConfigureTransport(tr)
	return &http.Client{Transport: tr, Timeout: 0}
}

func waitForSig(cancel func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	<-ch
	fmt.Println("Signal received: shutting down…")
	cancel()
}
