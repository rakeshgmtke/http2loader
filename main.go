// main.go
package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	stdlog "log"
	"net"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
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
	Name    string            `json:"name"`
	URL     string            `json:"url"`
	Method  string            `json:"method"`
	Body    string            `json:"body"`
	DelayMs int               `json:"delay_ms"`
	Headers map[string]string `json:"headers"`
}

type RangeSpec struct {
	Prefix       string `json:"prefix"`
	Template     string `json:"template,omitempty"`
	RandomHexLen int    `json:"random_hex_length,omitempty"`
}

type Config struct {
	Server struct {
		MetricsAddr     string `json:"metrics_addr"`
		MaxIdlePerHost  int    `json:"max_idle_per_host"`
		MaxConnsPerHost int    `json:"max_conns_per_host"`
		IdleConnSec     int    `json:"idle_conn_seconds"`
		Connum          int    `json:"connum"`
		Log             struct {
			Level            string `json:"level"`
			Format           string `json:"format"`
			File             string `json:"file"`
			EnableStdout     bool   `json:"enable_stdout"`
			RotateMaxSizeMB  int    `json:"rotate_max_size_mb,omitempty"`
			RotateMaxBackups int    `json:"rotate_max_backups,omitempty"`
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
	APIs   []API                `json:"apis"`
	Ranges map[string]RangeSpec `json:"ranges"`
}

// ---------------- Globals & Metrics ----------------

var (
	globalSeq int64
	inFlight  int64

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

// ---------------- Conn tracker ----------------

type ConnTracker struct {
	m sync.Map // host -> *int64
}

func NewConnTracker() *ConnTracker { return &ConnTracker{} }

func (ct *ConnTracker) inc(host string) {
	v, _ := ct.m.LoadOrStore(host, new(int64))
	p := v.(*int64)
	atomic.AddInt64(p, 1)
}

func (ct *ConnTracker) dec(host string) {
	v, ok := ct.m.Load(host)
	if !ok {
		return
	}
	p := v.(*int64)
	atomic.AddInt64(p, -1)
}

func (ct *ConnTracker) get(host string) int64 {
	v, ok := ct.m.Load(host)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(v.(*int64))
}

type trackedConn struct {
	net.Conn
	host    string
	tracker *ConnTracker
	closed  int32
}

func (tc *trackedConn) Close() error {
	if atomic.CompareAndSwapInt32(&tc.closed, 0, 1) {
		tc.tracker.dec(tc.host)
	}
	return tc.Conn.Close()
}

// ---------------- Helpers ----------------

// NOTE: renamed per request
// TemplatedDigits — fills '?' in format from right using n
func TemplatedDigits(format string, n int) string {
	nstring := fmt.Sprintf("%020d", n)
	out := make([]byte, len(format))
	nIdx := len(nstring) - 1
	for i := 0; i < len(format); i++ {
		formatIdx := len(format) - i - 1
		if format[formatIdx] == '?' {
			out[formatIdx] = nstring[nIdx]
			nIdx--
		} else {
			out[formatIdx] = format[formatIdx]
		}
	}
	return string(out)
}

func randomHexDigits(n int) string {
	if n <= 0 {
		return ""
	}
	tmp := make([]byte, n/2+1)
	_, _ = rand.Read(tmp)
	return hex.EncodeToString(tmp)[:n]
}

func replacePlaceholdersWithValues(input string, values map[string]string, encodeForURL bool) string {
	var b strings.Builder
	i := 0
	for {
		start := strings.Index(input[i:], "{$")
		if start == -1 {
			b.WriteString(input[i:])
			break
		}
		start += i
		b.WriteString(input[i:start])
		end := strings.Index(input[start:], "}")
		if end == -1 {
			b.WriteString(input[start:])
			break
		}
		end += start
		name := input[start+2 : end]
		if val, ok := values[name]; ok {
			if encodeForURL {
				val = url.PathEscape(val)
			}
			b.WriteString(val)
		} else {
			b.WriteString(input[start : end+1])
		}
		i = end + 1
	}
	return b.String()
}

func rangeValueSimple(r RangeSpec, round int64) string {
	if r.Template != "" {
		return r.Prefix + TemplatedDigits(r.Template, int(round))
	}
	if r.RandomHexLen > 0 {
		return r.Prefix + randomHexDigits(r.RandomHexLen)
	}
	return r.Prefix + strconv.FormatInt(round, 10)
}

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

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		if lines[i] != "" {
			lines[i] = prefix + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(truncated)"
}

// ---------------- Stats ----------------

type Job struct {
	API API
	Seq int64
}

type Stats struct {
	mu sync.Mutex
	by map[string]*APIMetric
}

type APIMetric struct {
	Name          string
	TotalSent     uint64
	TotalResponse uint64
	Success       uint64
	Redirect      uint64
	ClientError   uint64
	ServerError   uint64
	TotalLatency  float64
	MaxLatency    float64
	InFlight      int
}

func NewStats(apis []API) *Stats {
	s := &Stats{by: map[string]*APIMetric{}}
	for _, a := range apis {
		s.by[a.Name] = &APIMetric{Name: a.Name}
	}
	return s
}

func (s *Stats) IncInFlight(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.InFlight++
	}
}
func (s *Stats) DecInFlight(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.InFlight--
	}
}
func (s *Stats) IncTotalSent(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.TotalSent++
	}
}
func (s *Stats) IncTotalResponse(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.TotalResponse++
	}
}
func (s *Stats) IncSuccess(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.Success++
	}
}
func (s *Stats) IncRedirect(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.Redirect++
	}
}
func (s *Stats) IncClientError(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.ClientError++
	}
}
func (s *Stats) IncServerError(api string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.ServerError++
	}
}
func (s *Stats) AddLatency(api string, lat float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.by[api]; ok {
		m.TotalLatency += lat
		if lat > m.MaxLatency {
			m.MaxLatency = lat
		}
	}
}
func (s *Stats) GetAPIMetricsOrdered() []struct {
	Name   string
	Metric APIMetric
} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]struct {
		Name   string
		Metric APIMetric
	}, 0, len(s.by))
	for _, m := range s.by {
		out = append(out, struct {
			Name   string
			Metric APIMetric
		}{Name: m.Name, Metric: *m})
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].Name > out[j].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// ---------------- Logging: rotating plain active + compressed backups ----------------

type RotatingWriter struct {
	mu           sync.Mutex
	path         string
	file         *os.File
	bufWriter    *bufio.Writer
	curSize      int64
	maxSizeBytes int64
	maxBackups   int
}

func NewRotatingWriter(path string, maxSizeMB int, maxBackups int) (*RotatingWriter, error) {
	rw := &RotatingWriter{
		path:         path,
		maxSizeBytes: int64(maxSizeMB) * 1024 * 1024,
		maxBackups:   maxBackups,
	}
	if err := rw.openNew(); err != nil {
		return nil, err
	}
	return rw, nil
}

func (rw *RotatingWriter) openNew() error {
	if err := os.MkdirAll(filepath.Dir(rw.path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(rw.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	rw.file = f
	rw.bufWriter = bufio.NewWriterSize(f, 32*1024)
	if fi, err := f.Stat(); err == nil {
		rw.curSize = fi.Size()
	} else {
		rw.curSize = 0
	}
	return nil
}

func (rw *RotatingWriter) writeAndMaybeRotate(p []byte) (int, error) {
	n, err := rw.bufWriter.Write(p)
	if err != nil {
		return n, err
	}
	rw.curSize += int64(n)
	if err := rw.bufWriter.Flush(); err != nil {
		return n, err
	}
	if rw.maxSizeBytes > 0 && rw.curSize >= rw.maxSizeBytes {
		if err := rw.rotate(); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (rw *RotatingWriter) Write(p []byte) (int, error) {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.writeAndMaybeRotate(p)
}

func (rw *RotatingWriter) rotate() error {
	if rw.bufWriter != nil {
		_ = rw.bufWriter.Flush()
	}
	if rw.file != nil {
		_ = rw.file.Sync()
		_ = rw.file.Close()
	}
	ts := time.Now().Format("20060102-150405")
	dir := filepath.Dir(rw.path)
	base := filepath.Base(rw.path)
	rotated := filepath.Join(dir, fmt.Sprintf("%s.%s.gz", base, ts))
	_ = compressFileToGzip(rw.path, rotated)
	_ = os.Remove(rw.path)
	rw.removeOldBackups()
	if err := rw.openNew(); err != nil {
		return err
	}
	return nil
}

func (rw *RotatingWriter) removeOldBackups() {
	if rw.maxBackups <= 0 {
		return
	}
	dir := filepath.Dir(rw.path)
	base := filepath.Base(rw.path)
	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := base + "."
	var matches []os.FileInfo
	for _, fi := range files {
		if strings.HasPrefix(fi.Name(), prefix) && strings.HasSuffix(fi.Name(), ".gz") {
			matches = append(matches, fi)
		}
	}
	if len(matches) <= rw.maxBackups {
		return
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].ModTime().Before(matches[j].ModTime())
	})
	toRemove := len(matches) - rw.maxBackups
	for i := 0; i < toRemove; i++ {
		_ = os.Remove(filepath.Join(dir, matches[i].Name()))
	}
}

func (rw *RotatingWriter) Close() error {
	rw.mu.Lock()
	defer rw.mu.Unlock()
	if rw.bufWriter != nil {
		_ = rw.bufWriter.Flush()
	}
	if rw.file != nil {
		err := rw.file.Close()
		rw.file = nil
		return err
	}
	return nil
}

func compressFileToGzip(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer func() {
		out.Sync()
		out.Close()
	}()
	gw := gzip.NewWriter(out)
	defer gw.Close()
	_, err = io.Copy(gw, in)
	if err != nil {
		return err
	}
	return nil
}

func uniqueLogPath(path string) string {
	if path == "" {
		return path
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	pid := os.Getpid()
	ppid := os.Getppid()
	newBase := fmt.Sprintf("%s.%d.%d%s", name, pid, ppid, ext)
	return filepath.Join(dir, newBase)
}

// ---------------- Metrics helpers & printer ----------------

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

// printMetricsSimple — cleaned output, live TCP_Conn_Count via tracker
func printMetricsSimple(stats *Stats, tps map[string]float64, cfg *Config, pool *ClientPool, tracker *ConnTracker) {
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

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)

	fmt.Fprintf(w, "---- http2loader Live Stats @ %s UTC ----\n", now)
	fmt.Fprintln(w, "[KEYS] h=help | q=quit | p=pause | s=hoststats | +=TPS+10 | -=TPS-10 | tps <n>=set TPS")
	fmt.Fprintln(w)

	// updated line label per request
	fmt.Fprintf(w, "Configured \tTPS=%.2f/API\tTotalMsg=%d/API\tAPIs=%d\n", perApiTps, cfg.Load.TotalMsg, apiCount)

	// sorted hosts
	hostSet := map[string]struct{}{}
	for _, api := range cfg.APIs {
		h := hostKeyFromURL(api.URL)
		hostSet[h] = struct{}{}
	}
	hosts := make([]string, 0, len(hostSet))
	for h := range hostSet {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	connum := cfg.Server.Connum
	if connum <= 0 {
		connum = 1
	}

	if atomic.LoadInt32(&showHostStatsFlag) == 1 {
		for _, h := range hosts {
			live := tracker.get(h)
			fmt.Fprintf(w, "HostStats:\t%s\tTCP_Conn_Count=%d\tConfiguredConns=%d\n",
				h, live, connum)
		}
	}

	fmt.Fprintf(w, "\nCOUNT:\tSent=%d\tResp=%d\t2xx=%d\tErr=%d\tGoroutines=%d\tHeap=%.1fMB\n\n",
		totalSent, totalResp, total2xx, totalErr,
		runtime.NumGoroutine(), float64(ms.Alloc)/1024.0/1024.0)

	fmt.Fprintln(w, "API\tTPS\tSent\tResp\t2xx\t3xx\t4xx\t5xx\tInFlt\tAvg(ms)\tMax(ms)")
	for _, e := range ordered {
		m := e.Metric
		name := e.Name
		t := tps[name]

		avgMs := 0.0
		if m.TotalResponse > 0 {
			avgMs = (m.TotalLatency / float64(m.TotalResponse)) * 1000.0
		}
		maxMs := m.MaxLatency * 1000.0

		hAvg, hMax, _ := getAvgAndMaxFromHistogram(name)
		if hAvg > 0 {
			avgMs = hAvg
		}
		if hMax > 0 {
			maxMs = hMax
		}

		fmt.Fprintf(w, "%s\t%.2f\t%d\t%d\t%d\t%d\t%d\t%d\t%d\t%.2f\t%.2f\n",
			name, t,
			m.TotalSent, m.TotalResponse,
			m.Success, m.Redirect, m.ClientError, m.ServerError,
			m.InFlight, avgMs, maxMs)
	}
	fmt.Fprintln(w, "--------------------------------------------------------------------------------\n")
	w.Flush()
}

// ---------------- Scheduler & dispatcher ----------------

var (
	showHostStatsFlag int32 = 1
	pausedFlag        int32 = 0
	currentPerApiTPS        = atomic.Value{} // float64
)

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
	round := int64(0)

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
			globalRate := desired * float64(apiCount)
			if globalRate < 1.0 {
				globalRate = 1.0
			}
			limiter = rate.NewLimiter(rate.Limit(desired), apiCount)
			applied = desired
			logrus.Infof("[SCHED] perAPI=%.2f global=%.2f", desired, globalRate)
		}

		if err := limiter.Wait(ctx); err != nil {
			if closePending != nil {
				closePending()
			}
			return
		}

		values := map[string]string{}
		for rname, rspec := range cfg.Ranges {
			values[rname] = rangeValueSimple(rspec, round)
		}

		for _, api := range apis {
			select {
			case <-ctx.Done():
				if closePending != nil {
					closePending()
				}
				return
			default:
			}

			finalURL := replacePlaceholdersWithValues(api.URL, values, true)
			finalBody := replacePlaceholdersWithValues(api.Body, values, false)

			finalHeaders := map[string]string{}
			for hk, hv := range api.Headers {
				finalHeaders[hk] = replacePlaceholdersWithValues(hv, values, false)
			}

			apiCopy := API{
				Name:    api.Name,
				URL:     finalURL,
				Method:  api.Method,
				Body:    finalBody,
				DelayMs: api.DelayMs,
				Headers: finalHeaders,
			}

			seq := atomic.AddInt64(&globalSeq, 1)

			select {
			case pending <- Job{API: apiCopy, Seq: seq}:
				sent++
			case <-ctx.Done():
				if closePending != nil {
					closePending()
				}
				return
			}

			if totalMessages > 0 && sent >= totalMessages {
				break
			}
		}

		round++
		time.Sleep(1 * time.Millisecond)
		if totalMessages > 0 && sent >= totalMessages {
			break
		}
	}

	if closePending != nil {
		closePending()
	}
}

func sendHTTPRequest(client *http.Client, j Job, cfg *Config) (bool, time.Duration) {
	req, err := http.NewRequest(j.API.Method, j.API.URL, bytes.NewBufferString(j.API.Body))
	if err != nil {
		if logrus.IsLevelEnabled(logrus.DebugLevel) {
			logrus.Debugf("[SEND][%s] ERR creating request: %v", j.API.Name, err)
		}
		return false, 0
	}
	for k, v := range j.API.Headers {
		req.Header.Set(k, v)
	}
	if j.API.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		var h strings.Builder
		for k, vals := range req.Header {
			for _, v := range vals {
				h.WriteString(fmt.Sprintf("%s: %s\n", k, v))
			}
		}
		logrus.Debugf(
			"[SEND][%s] Request:\n  URL: %s\n  Method: %s\n  Headers:\n%s  Body: %s\n",
			j.API.Name,
			req.URL.String(),
			req.Method,
			indent(h.String(), "    "),
			j.API.Body,
		)
	}
	start := time.Now()
	resp, err := client.Do(req)
	lat := time.Since(start)
	if err != nil {
		if logrus.IsLevelEnabled(logrus.DebugLevel) {
			logrus.Debugf("[SEND][%s] ERR during HTTP call: %v", j.API.Name, err)
		}
		return false, lat
	}
	defer resp.Body.Close()

	var respBody string
	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		respBody = string(buf)
	} else {
		io.Copy(io.Discard, resp.Body)
	}

	if logrus.IsLevelEnabled(logrus.DebugLevel) {
		var rh strings.Builder
		for k, vals := range resp.Header {
			for _, v := range vals {
				rh.WriteString(fmt.Sprintf("%s: %s\n", k, v))
			}
		}
		logrus.Debugf(
			"[SEND][%s] Response:\n  Status: %d\n  Headers:\n%s  Body: %s\n  Latency: %s\n",
			j.API.Name,
			resp.StatusCode,
			indent(rh.String(), "    "),
			truncate(respBody, 1000),
			lat.String(),
		)
	}

	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	return ok, lat
}

func startSend(ctx context.Context, pool *ClientPool, job Job, sem chan struct{}, wg *sync.WaitGroup, cfg *Config, stats *Stats) {
	wg.Add(1)
	go func(j Job) {
		defer wg.Done()
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return
		}
		metricInFlight.Inc()
		stats.IncInFlight(j.API.Name)
		atomic.AddInt64(&inFlight, 1)
		defer func() {
			<-sem
			metricInFlight.Dec()
			stats.DecInFlight(j.API.Name)
			atomic.AddInt64(&inFlight, -1)
		}()

		metricAttempted.WithLabelValues(j.API.Name).Inc()
		if j.API.DelayMs > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(j.API.DelayMs) * time.Millisecond):
			}
		}

		ok, lat := sendHTTPRequest(pool.Next(), j, cfg)
		stats.IncTotalSent(j.API.Name)
		latSec := lat.Seconds()
		if ok {
			stats.IncSuccess(j.API.Name)
			metricSuccessful.WithLabelValues(j.API.Name).Inc()
		} else {
			stats.IncServerError(j.API.Name)
			metricFailed.WithLabelValues(j.API.Name, "err").Inc()
		}
		stats.IncTotalResponse(j.API.Name)
		stats.AddLatency(j.API.Name, latSec)
		metricLatency.WithLabelValues(j.API.Name).Observe(latSec)
	}(job)
}

func dispatcher(ctx context.Context, pool *ClientPool, pending <-chan Job, sem chan struct{}, wg *sync.WaitGroup, cfg *Config, stats *Stats) {
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

// ---------------- REPL ----------------

func lineModeListener(ctx context.Context, cancel func()) {
	r := bufio.NewReader(os.Stdin)
	printHelp := func() {
		fmt.Fprintln(os.Stderr, "Commands:")
		fmt.Fprintln(os.Stderr, "  h               - help")
		fmt.Fprintln(os.Stderr, "  p               - pause/resume")
		fmt.Fprintln(os.Stderr, "  s               - toggle hoststats on/off")
		fmt.Fprintln(os.Stderr, "  +               - increase per-API TPS by 10")
		fmt.Fprintln(os.Stderr, "  -               - decrease per-API TPS by 10")
		fmt.Fprintln(os.Stderr, "  tps <n>         - set per-API TPS to n")
		fmt.Fprintln(os.Stderr, "  q               - quit")
	}
	fmt.Fprintln(os.Stderr, "[CTRL] type a command then Enter. Type 'h' for help.")
	printHelp()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
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
		case "p":
			atomic.StoreInt32(&pausedFlag, 1-atomic.LoadInt32(&pausedFlag))
			fmt.Fprintf(os.Stderr, "[KEY] Paused -> %v\n", atomic.LoadInt32(&pausedFlag) == 1)
		case "s":
			atomic.StoreInt32(&showHostStatsFlag, 1-atomic.LoadInt32(&showHostStatsFlag))
			fmt.Fprintf(os.Stderr, "[KEY] HostStats -> %v\n", atomic.LoadInt32(&showHostStatsFlag) == 1)
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
				fmt.Fprintln(os.Stderr, "[ERR] usage: tps <n>")
				continue
			}
			n, err := strconv.ParseFloat(parts[1], 64)
			if err != nil || n <= 0 {
				fmt.Fprintln(os.Stderr, "[ERR] invalid tps value")
				continue
			}
			currentPerApiTPS.Store(n)
			fmt.Fprintf(os.Stderr, "[KEY] TPS set -> %.2f per API\n", n)
		case "q":
			fmt.Fprintln(os.Stderr, "[KEY] Quit requested")
			cancel()
			return
		default:
			fmt.Fprintf(os.Stderr, "[ERR] unknown: %s\n", cmd)
		}
	}
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
	return &cfg, nil
}

// ---------------- HTTP2 client factory (uses tracker) ----------------

func newHTTP2Client(maxIdle, maxConns int, idleTimeout time.Duration, tracker *ConnTracker) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2:   true,
		MaxIdleConns:        10000,
		MaxIdleConnsPerHost: maxIdle,
		MaxConnsPerHost:     maxConns,
		IdleConnTimeout:     idleTimeout,
	}
	origDial := dialer.DialContext
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		c, err := origDial(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		host := addr
		if i := strings.LastIndex(addr, ":"); i != -1 {
			host = addr[:i]
		}
		tracker.inc(host)
		return &trackedConn{Conn: c, host: host, tracker: tracker}, nil
	}
	_ = http2.ConfigureTransport(tr)
	return &http.Client{Transport: tr}
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

	// context
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// logging
	if cfg.Server.Log.File == "" {
		cfg.Server.Log.File = "http2loader.log"
	}
	activePath := uniqueLogPath(cfg.Server.Log.File)
	rotateSizeMB := cfg.Server.Log.RotateMaxSizeMB
	if rotateSizeMB <= 0 {
		rotateSizeMB = 50
	}
	rotateBackups := cfg.Server.Log.RotateMaxBackups
	if rotateBackups <= 0 {
		rotateBackups = 7
	}
	rotWriter, err := NewRotatingWriter(activePath, rotateSizeMB, rotateBackups)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize rotating logger: %v\n", err)
		logrus.SetOutput(os.Stderr)
	} else {
		if cfg.Server.Log.Format == "json" {
			logrus.SetFormatter(&logrus.JSONFormatter{TimestampFormat: time.RFC3339})
		} else {
			logrus.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		}
		lvl, perr := logrus.ParseLevel(cfg.Server.Log.Level)
		if perr != nil {
			lvl = logrus.InfoLevel
		}
		logrus.SetLevel(lvl)
		if cfg.Server.Log.EnableStdout {
			logrus.SetOutput(io.MultiWriter(os.Stdout, rotWriter))
			stdlog.SetOutput(io.MultiWriter(os.Stdout, rotWriter))
		} else {
			logrus.SetOutput(rotWriter)
			stdlog.SetOutput(rotWriter)
		}
		go func() {
			<-ctx.Done()
			_ = rotWriter.Close()
		}()
		logrus.Infof("logging initialized file=%s size_mb=%d backups=%d pid=%d ppid=%d",
			activePath, rotateSizeMB, rotateBackups, os.Getpid(), os.Getppid())
	}

	currentPerApiTPS.Store(cfg.Load.TPS)
	if currentPerApiTPS.Load().(float64) < 1 {
		currentPerApiTPS.Store(1.0)
	}

	if len(cfg.APIs) == 0 {
		logrus.Fatalf("No APIs configured")
	}

	// connection tracker and client pool
	tracker := NewConnTracker()
	connum := cfg.Server.Connum
	if connum <= 0 {
		connum = 1
	}
	clients := make([]*http.Client, 0, connum)
	for i := 0; i < connum; i++ {
		clients = append(clients, newHTTP2Client(cfg.Server.MaxIdlePerHost, cfg.Server.MaxConnsPerHost, time.Duration(cfg.Server.IdleConnSec)*time.Second, tracker))
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

	// REPL
	go lineModeListener(ctx, cancel)

	// signals
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
		<-ch
		logrus.Info("Signal received: shutting down…")
		cancel()
	}()

	// stats + run
	stats := NewStats(cfg.APIs)
	go dynamicScheduler(ctx, cfg, pending, closePending)
	go dispatcher(ctx, pool, pending, sem, &wg, cfg, stats)

	// queued gauge
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
				printMetricsSimple(stats, tps, cfg, pool, tracker)
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
