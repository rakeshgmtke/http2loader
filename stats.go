// stats.go
package main

import (
	"sync"
	"sync/atomic"
)

// APIMetric holds a snapshot of counters + latency (latency in seconds).
type APIMetric struct {
	TotalSent     uint64
	TotalResponse uint64
	Success       uint64
	Redirect      uint64
	ClientError   uint64
	ServerError   uint64
	TotalLatency  float64 // seconds
	MaxLatency    float64 // seconds

	// internal mutex for latency fields
	mu sync.RWMutex
}

// APIMetricEntry pairs a name with its snapshot metric (preserves order).
type APIMetricEntry struct {
	Name   string
	Metric APIMetric
}

// Stats holds per-API live metrics.
type Stats struct {
	APIMetrics map[string]*apiMetricInternal
	Order      []string
	mu         sync.RWMutex
}

// apiMetricInternal is the internal live structure (atomic counters + latency lock).
type apiMetricInternal struct {
	// atomic counters
	TotalSent     uint64
	TotalResponse uint64
	Success       uint64
	Redirect      uint64
	ClientError   uint64
	ServerError   uint64

	// latency fields (protected by latencyMu)
	TotalLatency float64
	MaxLatency   float64

	latencyMu sync.Mutex
}

// NewStats constructs Stats initialized for the provided APIs.
func NewStats(apis []API) *Stats {
	s := &Stats{
		APIMetrics: make(map[string]*apiMetricInternal, len(apis)),
		Order:      make([]string, 0, len(apis)),
	}
	for _, api := range apis {
		s.APIMetrics[api.Name] = &apiMetricInternal{}
		s.Order = append(s.Order, api.Name)
	}
	return s
}

// IncTotalSent increments the sent counter for an API.
func (s *Stats) IncTotalSent(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.TotalSent, 1)
}

// IncTotalResponse increments the response counter for an API.
func (s *Stats) IncTotalResponse(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.TotalResponse, 1)
}

// IncSuccess increments the 2xx counter.
func (s *Stats) IncSuccess(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.Success, 1)
}

// IncRedirect increments the 3xx counter.
func (s *Stats) IncRedirect(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.Redirect, 1)
}

// IncClientError increments the 4xx counter.
func (s *Stats) IncClientError(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.ClientError, 1)
}

// IncServerError increments the 5xx counter.
func (s *Stats) IncServerError(apiName string) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	atomic.AddUint64(&m.ServerError, 1)
}

// AddLatency records latency (in seconds) for the given API and updates max.
func (s *Stats) AddLatency(apiName string, latency float64) {
	s.mu.RLock()
	m, ok := s.APIMetrics[apiName]
	s.mu.RUnlock()
	if !ok {
		return
	}
	m.latencyMu.Lock()
	m.TotalLatency += latency
	if latency > m.MaxLatency {
		m.MaxLatency = latency
	}
	m.latencyMu.Unlock()
}

// GetAPIMetricsOrdered returns a snapshot slice preserving the configured API order.
func (s *Stats) GetAPIMetricsOrdered() []APIMetricEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]APIMetricEntry, 0, len(s.Order))
	for _, name := range s.Order {
		if m, ok := s.APIMetrics[name]; ok {
			// copy atomic counters
			totalSent := atomic.LoadUint64(&m.TotalSent)
			totalResp := atomic.LoadUint64(&m.TotalResponse)
			success := atomic.LoadUint64(&m.Success)
			redirect := atomic.LoadUint64(&m.Redirect)
			clientErr := atomic.LoadUint64(&m.ClientError)
			serverErr := atomic.LoadUint64(&m.ServerError)

			// copy latency under lock
			m.latencyMu.Lock()
			totalLatency := m.TotalLatency
			maxLatency := m.MaxLatency
			m.latencyMu.Unlock()

			snap := APIMetric{
				TotalSent:     totalSent,
				TotalResponse: totalResp,
				Success:       success,
				Redirect:      redirect,
				ClientError:   clientErr,
				ServerError:   serverErr,
				TotalLatency:  totalLatency,
				MaxLatency:    maxLatency,
			}
			out = append(out, APIMetricEntry{Name: name, Metric: snap})
		}
	}
	return out
}
