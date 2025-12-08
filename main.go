// main.go
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	dto "github.com/prometheus/client_model/go"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"golang.org/x/time/rate"
)

// ---------------- Types & Config ----------------

type API struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Method  string `json:"method"`
	Body    string `json:"body"`
	DelayMs int    `json:"delay_ms"`
}

type Config struct {
	Server struct {
		MetricsAddr     string `json:"metrics_addr"`
		MaxIdlePerHost  int    `json:"max_idle_per_host"`
		MaxConnsPerHost int    `json:"max_conns_per_host"`
		IdleConnSec     int    `json:"idle_conn_seconds"`
		Connum          int    `json:"connum"`
		Log             struct {
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
		TotalMsg           int     `json:"total_msg"`
	} `json:"load"`
	Debug struct {
		ShowHostStats bool `json:"show_host_stats"`
	} `json:"debug"`
	APIs []API `json:"apis"`
}

type Job struct {
	API API
	Seq int64
}

var (
	globalSeq int64
	inFlight  int64
)

// ---------------- Prometheus metrics ----------------

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
		Help: "Total requests attempted",
	}, []string{"api"})
	metricSuccessful = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_responses_successful_total",
		Help: "Total successful responses (2xx)",
	}, []string{"api"})
	metricFailed = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "client_responses_failed_total",
		Help: "Failed responses",
	}, []string{"api", "status_class"})
	metricLatency = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "client_request_duration_seconds",
		Help:    "Request latency",
		Buckets: prometheus.ExponentialBuckets(0.001, 2, 15),
	}, []string{"api"})
)

func init() {
	prometheus.MustRegister(metricQueued, metricInFlight, metricAttempted, metricSuccessful, metricFailed, metricLatency)
}

// ---------------- Client pool ----------------

type ClientPool struct {
	clients []*http.Client
	idx     uint64
}

func NewClientPool(c []*http.Client) *ClientPool {
	return &ClientPool{clients: c}
}

func (p *ClientPool) Next() *http.Client {
	if len(p.clients) == 0 {
		return http.DefaultClient
	}
	i := atomic.AddUint64(&p.idx, 1)
	return p.clients[int((i-1)%uint64(len(p.clients)))]
}

func newHTTP2Client(maxIdle, maxConns int, idleTimeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: maxIdle,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     idleTimeout,
		DialContext:         dialer.DialContext,
	}
	_ = http2.ConfigureTransport(tr)
	return &http.Client{Transport: tr}
}

// ---------------- Helpers ----------------

func hostKeyFromURL(u string) string {
	if idx := strings.Index(u, "://"); idx != -1 {
		rem := u[idx+3:]
		if sidx := strings.Index(rem, "/"); sidx != -1 {
			return rem[:sidx]
		}
		return rem
	}
	if sidx := strings.Index(u, "/"); sidx != -1 {
		return u[:sidx]
	}
	return u
}

func getAvgAndMaxFromHistogram(api string) (avgMs float64, maxMs float64, err error) {
	fams, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, 0, err
	}
	var mf *dto.MetricFamily
	for _, f := range fams {
		if f.GetName() == "client_request_duration_seconds" {
			mf = f
			break
		}
	}
	if mf == nil {
		return 0, 0, nil
	}
	for _, m := range mf.Metric {
		found := false
		for _, l := range m.Label {
			if l.GetName() == "api" && l.GetValue() == api {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		h := m.GetHistogram()
		if h == nil || h.GetSampleCount() == 0 {
			return 0, 0, nil
		}
		avg := float64(h.GetSampleSum()) / float64(h.GetSampleCount())
		var prev uint64
		var approxMax float64
		for _, b := range h.Bucket {
			if b.GetCumulativeCount() > prev {
				approxMax = b.GetUpperBound()
			}
			prev = b.GetCumulativeCount()
		}
		return avg * 1000.0, approxMax * 1000.0, nil
	}
	return 0, 0, nil
}

// ---------------- runtime toggles ----------------

var showHostStatsFlag int32 = 1
var pausedFlag int32 = 0
var currentPerApiTPS atomic.Value // stores float64

// ---------------- Line-mode REPL ----------------

func lineModeListener(ctx context.Context, cancel func()) {
	r := bufio.NewReader(os.Stdin)
	printHelp := func() {
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  h               - help (this)")
		fmt.Fprintln(os.Stderr, "  s               - toggle host stats")
		fmt.Fprintln(os.Stderr, "  p               - pause/resume scheduler")
		fmt.Fprintln(os.Stderr, "  +               - increase per-API TPS by 10")
		fmt.Fprintln(os.Stderr, "  -               - decrease per-API TPS by 10 (min 1)")
		fmt.Fprintln(os.Stderr, "  tps <n>         - set per-API TPS to n (e.g. 'tps 10')")
		fmt.Fprintln(os.Stderr, "  tps +<n> / -<n> - relative adjust (e.g. 'tps +10')")
		fmt.Fprintln(os.Stderr, "  q               - quit")
	}
	fmt.Fprintln(os.Stderr, "[CTRL] type a command then Enter. Type 'h' for help.")
	printHelp()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			fmt.Fprint(os.Stderr, "> ")
			line, err := r.ReadString('\n')
			if err != nil {
				fmt.Fprintf(os.Stderr, "[ERR] read: %v\n", err)
				return
			}
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			parts := strings.Fields(line)
			cmd := strings.ToLower(parts[0])

			switch cmd {
			case "h":
				printHelp()
			case "s":
				atomic.StoreInt32(&showHostStatsFlag, 1-atomic.LoadInt32(&showHostStatsFlag))
				fmt.Fprintf(os.Stderr, "[KEY] HostStats -> %v\n", atomic.LoadInt32(&showHostStatsFlag) == 1)
			case "p":
				atomic.StoreInt32(&pausedFlag, 1-atomic.LoadInt32(&pausedFlag))
				fmt.Fprintf(os.Stderr, "[KEY] Paused -> %v\n", atomic.LoadInt32(&pausedFlag) == 1)
			case "+":
				v := currentPerApiTPS.Load().(float64)
				v += 10.0
				currentPerApiTPS.Store(v)
				fmt.Fprintf(os.Stderr, "[KEY] TPS -> %.2f per API\n", v)
			case "-":
				v := currentPerApiTPS.Load().(float64)
				v -= 10.0
				if v < 1 {
					v = 1
				}
				currentPerApiTPS.Store(v)
				fmt.Fprintf(os.Stderr, "[KEY] TPS -> %.2f per API\n", v)
			case "tps":
				if len(parts) < 2 {
					fmt.Fprintln(os.Stderr, "[ERR] usage: tps <n> | tps +<n> | tps -<n>")
					continue
				}
				arg := parts[1]
				if strings.HasPrefix(arg, "+") || strings.HasPrefix(arg, "-") {
					d, err := strconv.ParseFloat(arg, 64)
					if err != nil {
						fmt.Fprintf(os.Stderr, "[ERR] invalid tps delta: %v\n", err)
						continue
					}
					v := currentPerApiTPS.Load().(float64)
					v += d
					if v < 1 {
						v = 1
					}
					currentPerApiTPS.Store(v)
					fmt.Fprintf(os.Stderr, "[KEY] TPS adjusted -> %.2f per API\n", v)
				} else {
					n, err := strconv.ParseFloat(arg, 64)
					if err != nil || n <= 0 {
						fmt.Fprintf(os.Stderr, "[ERR] invalid tps value: %v\n", err)
						continue
					}
					currentPerApiTPS.Store(n)
					fmt.Fprintf(os.Stderr, "[KEY] TPS set -> %.2f per API\n", n)
				}
			case "q":
				fmt.Fprintln(os.Stderr, "[KEY] Quit requested")
				cancel()
				return
			default:
				fmt.Fprintf(os.Stderr, "[ERR] unknown command: %s. Type 'h' for help\n", cmd)
			}
		}
	}
}

// ---------------- Host connection estimation ----------------

type HostConnStats struct {
	Active   int
	Idle     int
	InUse    int
	MaxIdle  int
	MaxConns int
}

func collectHostStats(cfg *Config, pool *ClientPool) map[string]HostConnStats {
	out := map[string]HostConnStats{}
	if cfg == nil || pool == nil {
		return out
	}
	hostSet := map[string]struct{}{}
	for _, api := range cfg.APIs {
		h := hostKeyFromURL(api.URL)
		hostSet[h] = struct{}{}
	}
	numClients := len(pool.clients)
	if numClients == 0 {
		numClients = 1
	}
	for h := range hostSet {
		out[h] = HostConnStats{Active: numClients, Idle: 0, InUse: 0, MaxIdle: cfg.Server.MaxIdlePerHost, MaxConns: cfg.Server.MaxConnsPerHost}
	}
	return out
}

// ---------------- Metrics printer ----------------

func printMetricsSimple(stats *Stats, pending int, tps map[string]float64, cfg *Config, pool *ClientPool) {
	ordered := stats.GetAPIMetricsOrdered()
	var totalSent, totalResp, total2xx, totalErr uint64
	for _, e := range ordered {
		m := e.Metric
		totalSent += m.TotalSent
		totalResp += m.TotalResponse
		total2xx += m.Success
		totalErr += m.Redirect + m.ClientError + m.ServerError
	}

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	apiCount := len(cfg.APIs)
	perApiTps := currentPerApiTPS.Load().(float64)
	targetGlobal := perApiTps * float64(apiCount)

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "---- http2loader Live Stats @ %s UTC ----\n", now)
	fmt.Fprintln(w, "[KEYS] h=help | q=quit | p=pause | s=hoststats | +=TPS+10 | -=TPS-10 | tps <n>=set TPS")
	fmt.Fprintln(w)

	fmt.Fprintf(w, "CFG:\tTPS=%.2f/API\tTotalMsg=%d/API\tAPIs=%d\tTargetGlobalTPS=%.2f\n", perApiTps, cfg.Load.TotalMsg, apiCount, targetGlobal)
	hostStats := collectHostStats(cfg, pool)
	fmt.Fprintf(w, "TCP:\tExpectedConns=%d\tClients=%d\tHosts=%d\tPending=%d\tInFlight=%d\n",
		cfg.Server.Connum*len(hostStats), cfg.Server.Connum, len(hostStats), pending, atomic.LoadInt64(&inFlight))

	if atomic.LoadInt32(&showHostStatsFlag) == 1 {
		for h, hs := range hostStats {
			fmt.Fprintf(w, "HostStats:\t%s\tActive=%d\tIdle=%d\tInUse=%d\tMaxIdle=%d\tMaxConns=%d\n",
				h, hs.Active, hs.Idle, hs.InUse, hs.MaxIdle, hs.MaxConns)
		}
	}

	fmt.Fprintf(w, "COUNT:\tSent=%d\tResp=%d\t2xx=%d\tErr=%d\tGoroutines=%d\tHeap=%.1fMB\n\n",
		totalSent, totalResp, total2xx, totalErr, runtime.NumGoroutine(), float64(ms.Alloc)/1024.0/1024.0)

	fmt.Fprintln(w, "API\tTPS\tSent\tResp\t2xx\t3xx\t4xx\t5xx\tInFlt\tAvg(ms)\tMax(ms)")
	for _, e := range ordered {
		m := e.Metric
		name := e.Name
		t := tps[name]
		inF := int64(m.InFlight)
		avgMs := 0.0
		if m.TotalResponse > 0 {
			avgMs = (m.TotalLatency / float64(m.TotalResponse)) * 1000.0
		}
		maxMs := m.MaxLatency * 1000.0

		// If histogram has values, prefer histogram-derived avg & approx max
		hAvg, hMax, _ := getAvgAndMaxFromHistogram(name)
		if hAvg > 0 {
			avgMs = hAvg
		}
		if hMax > 0 {
			maxMs = hMax
		}

		fmt.Fprintf(w, "%s\t%.2f\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%.2f\n",
			name, t, m.TotalSent, m.TotalResponse,
			m.Success, m.Redirect, m.ClientError, m.ServerError,
			inF, avgMs, maxMs)
	}
	fmt.Fprintln(w, "--------------------------------------------------------------------------------\n")
	w.Flush()
}

// ---------------- Scheduler & dispatcher ----------------

func dynamicScheduler(ctx context.Context, cfg *Config, pending chan<- Job, closePending func()) {
	apis := cfg.APIs
	apiCount := len(apis)
	if apiCount == 0 {
		if closePending != nil {
			closePending()
		}
		return
	}
	totalPerAPI := cfg.Load.TotalMsg
	totalMessages := 0
	if totalPerAPI > 0 {
		totalMessages = totalPerAPI * apiCount
	}
	sent := 0
	apiIdx := 0

	var limiter *rate.Limiter
	var applied float64 = -1.0

	for totalMessages == 0 || sent < totalMessages {
		if atomic.LoadInt32(&pausedFlag) == 1 {
			select {
			case <-ctx.Done():
				if closePending != nil {
					closePending()
				}
				return
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}

		desired := currentPerApiTPS.Load().(float64)
		if desired < 1.0 {
			desired = 1.0
			currentPerApiTPS.Store(1.0)
		}

		if limiter == nil || desired != applied {
			final := desired * float64(apiCount)
			if final < 1 {
				final = 1
			}
			limiter = rate.NewLimiter(rate.Limit(final), apiCount)
			applied = desired
			logrus.Infof("[SCHED] limiter perAPI=%.2f global=%.2f", desired, final)
		}

		if err := limiter.Wait(ctx); err != nil {
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
		case <-ctx.Done():
			if closePending != nil {
				closePending()
			}
			return
		}
	}

	if closePending != nil {
		closePending()
	}
}

func dispatcher(ctx context.Context, pool *ClientPool, pending <-chan Job, sem chan struct{},
	wg *sync.WaitGroup, cfg *Config, stats *Stats) {
	for {
		select {
		case <-ctx.Done():
			for j := range pending {
				startSend(ctx, pool, j, sem, wg, cfg, stats)
			}
			return
		case j, ok := <-pending:
			if !ok {
				return
			}
			startSend(ctx, pool, j, sem, wg, cfg, stats)
		}
	}
}

func startSend(ctx context.Context, pool *ClientPool, job Job, sem chan struct{},
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

		// Increment per-API in-flight + global in-flight + prom metric
		stats.IncInFlight(j.API.Name)
		atomic.AddInt64(&inFlight, 1)
		metricInFlight.Inc()

		// Ensure release and decrement in any exit path
		defer func() {
			<-sem
			metricInFlight.Dec()
			atomic.AddInt64(&inFlight, -1)
			stats.DecInFlight(j.API.Name)
		}()

		metricAttempted.WithLabelValues(j.API.Name).Inc()

		// API-level delay
		if j.API.DelayMs > 0 {
			time.Sleep(time.Duration(j.API.DelayMs) * time.Millisecond)
		}

		// request timeout
		rctx, cancel := context.WithTimeout(ctx, time.Duration(cfg.Load.RequestTimeout)*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(rctx, j.API.Method, j.API.URL, bytes.NewReader([]byte(j.API.Body)))
		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "client_error").Inc()
			stats.IncClientError(j.API.Name)
			logrus.Errorf("[ERR] %s seq=%d err=%v", j.API.Name, j.Seq, err)
			return
		}
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		resp, err := pool.Next().Do(req)
		stats.IncTotalSent(j.API.Name)
		lat := time.Since(start).Seconds()

		if err != nil {
			metricFailed.WithLabelValues(j.API.Name, "transport_error").Inc()
			stats.IncClientError(j.API.Name)
			logrus.Errorf("[ERR] %s seq=%d err=%v", j.API.Name, j.Seq, err)
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
		} else {
			stats.IncServerError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "5xx").Inc()
		}
	}(job)
}

// ---------------- Main ----------------

func main() {
	cfgPath := flag.String("config", "config.json", "path to config.json")
	flag.Parse()

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(2)
	}

	currentPerApiTPS.Store(cfg.Load.TPS)
	if currentPerApiTPS.Load().(float64) < 1 {
		currentPerApiTPS.Store(1.0)
	}

	// logging
	if cfg.Server.Log.Format == "json" {
		logrus.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339})
	} else {
		logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
	}
	level, err := logrus.ParseLevel(cfg.Server.Log.Level)
	if err != nil {
		level = logrus.InfoLevel
	}
	logrus.SetLevel(level)
	logrus.SetOutput(os.Stderr)
	if cfg.Server.Log.File != "" {
		if f, ferr := os.OpenFile(cfg.Server.Log.File, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0644); ferr == nil {
			if cfg.Server.Log.EnableStdout {
				logrus.SetOutput(io.MultiWriter(os.Stderr, f))
			} else {
				logrus.SetOutput(f)
			}
		}
	}

	if len(cfg.APIs) == 0 {
		logrus.Fatalf("No APIs configured")
	}

	// client pool
	connum := cfg.Server.Connum
	if connum <= 0 {
		connum = 1
	}
	clients := make([]*http.Client, 0, connum)
	for i := 0; i < connum; i++ {
		clients = append(clients, newHTTP2Client(cfg.Server.MaxIdlePerHost, cfg.Server.MaxConnsPerHost, time.Duration(cfg.Server.IdleConnSec)*time.Second))
	}
	pool := NewClientPool(clients)
	logrus.Infof("ClientPool created: %d clients", connum)

	// metrics server
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/debug/pprof/", http.HandlerFunc(pprof.Index))
		mux.HandleFunc("/debug/pprof/profile", http.HandlerFunc(pprof.Profile))

		addr := cfg.Server.MetricsAddr
		if addr == "" {
			addr = ":9090"
		}
		logrus.Infof("Metrics at %s/metrics", addr)
		if err := http.ListenAndServe(addr, mux); err != nil {
			logrus.Errorf("Metrics server error: %v", err)
		}
	}()

	// pending queue & semaphore
	pending := make(chan Job, cfg.Load.PendingBuf)
	sem := make(chan struct{}, cfg.Load.MaxInflight)
	var wg sync.WaitGroup
	var once sync.Once
	closePending := func() { once.Do(func() { close(pending) }) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// keyboard REPL
	go lineModeListener(ctx, cancel)

	// signal handler
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		fmt.Fprintln(os.Stderr, "Signal received: shutting down…")
		cancel()
	}()

	// stats + run
	stats := NewStats(cfg.APIs)
	go dynamicScheduler(ctx, cfg, pending, closePending)
	go dispatcher(ctx, pool, pending, sem, &wg, cfg, stats)

	// queued gauge updater
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

	// UI printer
	go func() {
		var last map[string]APIMetric
		var lastT time.Time
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				ordered := stats.GetAPIMetricsOrdered()
				now := time.Now()
				tps := make(map[string]float64)

				if last != nil {
					d := now.Sub(lastT).Seconds()
					if d <= 0 {
						d = 1
					}
					for _, e := range ordered {
						name := e.Name
						if prev, ok := last[name]; ok {
							tps[name] = float64(e.Metric.TotalResponse-prev.TotalResponse) / d
						}
					}
				}

				cur := make(map[string]APIMetric)
				for _, e := range ordered {
					cur[e.Name] = e.Metric
				}
				last = cur
				lastT = now

				printMetricsSimple(stats, len(pending), tps, cfg, pool)
			}
		}
	}()

	<-ctx.Done()
	logrus.Info("Shutdown requested — draining pending queue")
	closePending()
	wg.Wait()

	for _, c := range clients {
		if tr, ok := c.Transport.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	logrus.Info("Exited cleanly")
}

// ---------------- Config loader ----------------

func loadConfig(path string) (*Config, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	// defaults
	if cfg.Server.MetricsAddr == "" {
		cfg.Server.MetricsAddr = ":9090"
	}
	if cfg.Server.MaxIdlePerHost <= 0 {
		cfg.Server.MaxIdlePerHost = 200
	}
	if cfg.Server.MaxConnsPerHost <= 0 {
		cfg.Server.MaxConnsPerHost = 200
	}
	if cfg.Server.IdleConnSec <= 0 {
		cfg.Server.IdleConnSec = 90
	}
	if cfg.Server.Connum <= 0 {
		cfg.Server.Connum = 1
	}
	if cfg.Load.TPS <= 0 {
		cfg.Load.TPS = 100
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
