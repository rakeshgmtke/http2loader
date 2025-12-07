package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/net/http2"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
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

// GlobalConfig holds settings applicable to the entire run.
type GlobalConfig struct {
	TPS                   float64 `json:"tps"` // TPS per API (each API receives this rate)
	MaxInflight           int     `json:"max_inflight"`
	PendingBuf            int     `json:"pending_buf"`
	RequestTimeoutSeconds int     `json:"request_timeout_seconds"`
	MaxRetries            int     `json:"max_retries"`
	BackoffMs             int     `json:"backoff_ms"`
	TotalMsg              int     `json:"total_msg"` // Total number of messages to send across all APIs. If 0, scheduler runs indefinitely according to TPS.
	MaxIdlePerHost        int     `json:"max_idle_per_host"`
	IdleConnSeconds       int     `json:"idle_conn_seconds"`
	MetricsAddr           string  `json:"metrics_addr"`
	LogLevel              string  `json:"log_level"`
}

// Config defines configuration fields
type Config struct {
	Global GlobalConfig `json:"global"`
	APIs   []API        `json:"apis"`
}

type Job struct {
	API API
	Seq int64
}

var globalSeq int64
var logger = logrus.New()

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
	metricSent = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_requests_sent_total",
		Help: "Total successful requests",
	}, []string{"api"}) // This metric will be superseded but kept for now.
	metricFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_requests_failed_total",
		Help: "Total failed requests",
	}, []string{"api", "reason"})
	metricLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "client_request_duration_seconds",
		Help:    "Latency distribution",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
	}, []string{"api"})
	metricResponses = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_responses_total",
		Help: "Total responses received by status code.",
	}, []string{"api", "status_code"})
)

func init() {
	prometheus.MustRegister(metricQueued, metricInFlight, metricSent, metricFailed, metricLatency, metricResponses)
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		logger.Fatalf("failed to load config: %v", err)
	}

	// Setup logger
	logger.SetFormatter(&logrus.JSONFormatter{})
	logger.SetOutput(os.Stdout)
	level, err := logrus.ParseLevel(cfg.Global.LogLevel)
	if err != nil {
		logger.Warnf("Invalid log level '%s', defaulting to 'info'", cfg.Global.LogLevel)
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)

	if len(cfg.APIs) == 0 {
		logger.Fatal("No APIs configured")
	}

	client := newHTTP2Client(cfg.Global.MaxIdlePerHost, time.Duration(cfg.Global.IdleConnSeconds)*time.Second)

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
		mux.Handle("/debug/pprof/mutex", pprof.Handler("mutex")) // This was missing
		logger.Infof("Metrics at %s/metrics, pprof at %s/debug/pprof/", cfg.Global.MetricsAddr, cfg.Global.MetricsAddr)
		if err := http.ListenAndServe(cfg.Global.MetricsAddr, mux); err != nil {
			logger.Errorf("Metrics server error: %v", err)
		}
	}()

	// Scheduler (global TPS)
	go schedulerGlobal(ctx, cfg.Global.TPS, cfg.APIs, pending, cfg.Global.TotalMsg, closePending)

	// Dispatcher
	go dispatcher(ctx, client, pending, sem, &inFlightWG, cfg)

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

	<-ctx.Done()
	logger.Info("Shutdown requested — draining pending queue")

	// Ensure pending is closed (scheduler may already have closed it)
	closePending()
	inFlightWG.Wait()

	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}

	logger.Info("Exited cleanly")
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
		cfg.Global.TPS = 100
	}
	if cfg.Global.MaxInflight <= 0 {
		cfg.Global.MaxInflight = 1000
	}
	if cfg.Global.PendingBuf <= 0 {
		cfg.Global.PendingBuf = cfg.Global.MaxInflight * 2
	}
	if cfg.Global.RequestTimeoutSeconds <= 0 {
		cfg.Global.RequestTimeoutSeconds = 15
	}
	if cfg.Global.MaxRetries < 0 {
		cfg.Global.MaxRetries = 1
	}
	if cfg.Global.BackoffMs <= 0 {
		cfg.Global.BackoffMs = 50
	}
	if cfg.Global.MetricsAddr == "" {
		cfg.Global.MetricsAddr = ":9091"
	}
	if cfg.Global.MaxIdlePerHost <= 0 {
		cfg.Global.MaxIdlePerHost = 200
	}
	if cfg.Global.IdleConnSeconds <= 0 {
		cfg.Global.IdleConnSeconds = 90
	}
	if cfg.Global.TotalMsg < 0 {
		cfg.Global.TotalMsg = 0
	}
	if cfg.Global.LogLevel == "" {
		cfg.Global.LogLevel = "info"
	}

	return &cfg, nil
}

func schedulerGlobal(ctx context.Context, totalTPS float64, apis []API, pending chan<- Job, totalRounds int, closePending func()) {
	defer closePending()

	count := len(apis)
	if count == 0 || totalTPS <= 0 {
		logger.Warn("[SCHED] No APIs or TPS is zero, scheduler stopping.")
		return
	}

	// Calculate the total number of messages to send. If totalRounds is 0, it runs indefinitely.
	var totalToEmit int64
	if totalRounds > 0 {
		totalToEmit = int64(totalRounds)
	}

	// The total TPS for all APIs combined.
	globalTPS := totalTPS * float64(count)
	interval := time.Duration(1e9/globalTPS) * time.Nanosecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	apiIdx := 0
	var emitted int64

	for {
		select {
		case <-ctx.Done():
			logger.Info("[SCHED] Context cancelled, stopping scheduler.")
			return
		case <-ticker.C:
			if totalToEmit > 0 && emitted >= totalToEmit {
				logger.Infof("[SCHED] reached total messages=%d, stopping scheduler", totalToEmit)
				return
			}

			api := apis[apiIdx]
			apiIdx = (apiIdx + 1) % count
			seq := atomic.AddInt64(&globalSeq, 1)
			job := Job{API: api, Seq: seq}

			// Handle delayed jobs by spawning a goroutine that waits.
			if api.DelayMs > 0 {
				go func(j Job) {
					select {
					case <-time.After(time.Duration(j.API.DelayMs) * time.Millisecond):
						pending <- j
						logger.WithFields(logrus.Fields{"api": j.API.Name, "seq": j.Seq}).Debug("Queued job (delayed)")
					case <-ctx.Done():
						return
					}
				}(job)
			} else {
				// Send non-delayed jobs immediately.
				select {
				case pending <- job:
					logger.WithFields(logrus.Fields{"api": api.Name, "seq": seq}).Debug("Queued job")
				case <-ctx.Done():
					logger.Info("[SCHED] Context cancelled while queueing job, stopping.")
					return
				}
			}

			emitted++
		}
	}
}

func dispatcher(ctx context.Context, client *http.Client, pending <-chan Job, sem chan struct{},
	inFlightWG *sync.WaitGroup, cfg *Config) {
	for {
		select {
		case <-ctx.Done():
			for job := range pending {
				startSend(ctx, job, client, sem, inFlightWG, cfg)
			}
			return

		case job, ok := <-pending:
			if !ok {
				return
			}
			startSend(ctx, job, client, sem, inFlightWG, cfg)
		}
	}
}

func startSend(ctx context.Context, job Job, client *http.Client, sem chan struct{},
	inFlightWG *sync.WaitGroup, cfg *Config) {

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
		}()

		metricInFlight.Inc()
		// Single-attempt send
		rctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Global.RequestTimeoutSeconds)*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(rctx, j.API.Method, j.API.URL, bytes.NewReader([]byte(j.API.Body)))
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "request_creation").Inc()
			logger.WithFields(logrus.Fields{
				"api":   j.API.Name,
				"seq":   j.Seq,
				"error": err,
			}).Error("Request creation failed")
			return
		}
		req.Header.Set("Content-Type", "application/json")

		logger.WithFields(logrus.Fields{
			"api":    j.API.Name,
			"seq":    j.Seq,
			"method": j.API.Method,
			"url":    j.API.URL,
		}).Debug("Sending request")

		start := time.Now()
		resp, err := client.Do(req)
		lat := time.Since(start).Seconds()
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "http_do_error").Inc()
			logger.WithFields(logrus.Fields{"api": j.API.Name, "seq": j.Seq, "error": err}).Error("Request failed")
			return
		}
		defer resp.Body.Close()

		metricResponses.WithLabelValues(j.API.Name, fmt.Sprintf("%d", resp.StatusCode)).Inc()

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			metricSent.WithLabelValues(j.API.Name).Inc()
			metricLatency.WithLabelValues(j.API.Name).Observe(lat)
		} else {
			metricFailed.WithLabelValues(j.API.Name, "bad_status_code").Inc()
			logger.WithFields(logrus.Fields{"api": j.API.Name, "seq": j.Seq, "status": resp.StatusCode}).Warn("Received non-2xx status")
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
	logger.Info("Signal received: shutting down…")
	cancel()
}
