package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

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
}

// Config defines configuration fields
type Config struct {
	Global struct {
		TPS            float64 `json:"tps"` // TOTAL TPS across ALL APIs
		MaxInflight    int     `json:"max_inflight"`
		PendingBuf     int     `json:"pending_buf"`
		RequestTimeout int     `json:"request_timeout_seconds"`
		MaxRetries     int     `json:"max_retries"`
		BackoffMs      int     `json:"backoff_ms"`
		MetricsAddr    string  `json:"metrics_addr"`
		MaxIdlePerHost int     `json:"max_idle_per_host"`
		IdleConnSec    int     `json:"idle_conn_seconds"`
	} `json:"global"`
	APIs []API `json:"apis"`
}

type Job struct {
	API API
	Seq int64
}

var globalSeq int64

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
	}, []string{"api"})
	metricFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_requests_failed_total",
		Help: "Total failed requests",
	}, []string{"api"})
	metricLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "client_request_duration_seconds",
		Help:    "Latency distribution",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
	}, []string{"api"})
)

func init() {
	prometheus.MustRegister(metricQueued, metricInFlight, metricSent, metricFailed, metricLatency)
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if len(cfg.APIs) == 0 {
		log.Fatalf("No APIs configured")
	}

	client := newHTTP2Client(cfg.Global.MaxIdlePerHost, time.Duration(cfg.Global.IdleConnSec)*time.Second)

	pending := make(chan Job, cfg.Global.PendingBuf)
	sem := make(chan struct{}, cfg.Global.MaxInflight)
	var inFlightWG sync.WaitGroup

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go waitForSig(cancel)

	// Start metrics server
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Printf("Metrics at %s/metrics", cfg.Global.MetricsAddr)
		if err := http.ListenAndServe(cfg.Global.MetricsAddr, nil); err != nil {
			log.Printf("Metrics server error: %v", err)
		}
	}()

	// Scheduler (global TPS)
	go schedulerGlobal(ctx, cfg.Global.TPS, cfg.APIs, pending)

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
	log.Println("Shutdown requested — draining pending queue")

	close(pending)
	inFlightWG.Wait()

	if tr, ok := client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}

	log.Println("Exited cleanly")
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

	return &cfg, nil
}

func schedulerGlobal(ctx context.Context, totalTPS float64, apis []API, pending chan<- Job) {
	ticker := time.NewTicker(1 * time.Millisecond)
	defer ticker.Stop()

	acc := 0.0
	apiIdx := 0
	count := len(apis)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			acc += totalTPS / 1000.0
			toEmit := int(math.Floor(acc))
			if toEmit <= 0 {
				continue
			}
			acc -= float64(toEmit)

			for i := 0; i < toEmit; i++ {
				api := apis[apiIdx]
				apiIdx = (apiIdx + 1) % count

				seq := atomic.AddInt64(&globalSeq, 1)
				job := Job{API: api, Seq: seq}

				select {
				case pending <- job:
				case <-ctx.Done():
					return
				}
			}
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

	select {
	case sem <- struct{}{}:
	case <-ctx.Done():
		return
	}

	inFlightWG.Add(1)
	metricInFlight.Inc()

	go func(j Job) {
		defer func() {
			<-sem
			inFlightWG.Done()
			metricInFlight.Dec()
		}()

		rctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Global.RequestTimeout)*time.Second)
		defer cancel()

		start := time.Now()
		err := sendWithRetry(rctx, client, j, cfg.Global.MaxRetries, time.Duration(cfg.Global.BackoffMs)*time.Millisecond)
		lat := time.Since(start).Seconds()

		if err != nil {
			metricFailed.WithLabelValues(j.API.Name).Inc()
			log.Printf("[ERR] %s seq=%d err=%v\n", j.API.Name, j.Seq, err)
		} else {
			metricSent.WithLabelValues(j.API.Name).Inc()
			metricLatency.WithLabelValues(j.API.Name).Observe(lat)
		}
	}(job)
}

func sendWithRetry(ctx context.Context, client *http.Client, job Job,
	maxAttempts int, backoffBase time.Duration) error {

	var lastErr error

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		req, err := http.NewRequestWithContext(ctx, job.API.Method, job.API.URL,
			bytes.NewReader([]byte(job.API.Body)))
		if err != nil {
			return err
		}

		req.Header.Set("Content-Type", "application/json")

		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("status=%d", resp.StatusCode)
		}

		lastErr = err

		if attempt < maxAttempts {
			j := time.Duration(rand.Intn(100)) * time.Millisecond
			sleep := backoffBase + j

			select {
			case <-time.After(sleep):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}

	return lastErr
}

func newHTTP2Client(maxIdlePerHost int, idleTimeout time.Duration) *http.Client {
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
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
