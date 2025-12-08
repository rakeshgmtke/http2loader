// stats.go
package main

import (
	"sync"
	"sync/atomic"
)

// APIMetric holds counters for a single API (snapshot view).
// Latency fields are in seconds.
type APIMetric struct {
	TotalSent     uint64
	TotalResponse uint64
	Success       uint64
	Redirect      uint64
	ClientError   uint64
	ServerError   uint64

	// Current in-flight requests for this API (snapshot).
	InFlight uint64

	// Lifetime latency aggregates (seconds)
	TotalLatency float64
	MaxLatency   float64

	// internal mutex not part of snapshot semantics (not exported)
	mu sync.RWMutex
}

// APIMetricEntry is a named snapshot used to preserve ordering in displays.
type APIMetricEntry struct {
	Name   string
	Metric APIMetric
}

// Stats tracks per-API metrics.
type Stats struct {
	APIMetrics map[string]*apiMetricInternal
	Order      []string
	mu         sync.RWMutex
}

// apiMetricInternal stores the live counters.
// Thread-safe operations must use the embedded locks / atomics.
type apiMetricInternal struct {
	// atomic counters
	TotalSent     uint64
	TotalResponse uint64
	Success       uint64
	Redirect      uint64
	ClientError   uint64
	ServerError   uint64

	// InFlight (atomic)
	InFlight uint64

	// latency aggregates (protected by latencyMu)
	TotalLatency float64
	MaxLatency   float64

	latencyMu sync.Mutex // protects latency fields
}

// NewStats creates a Stats object and initializes per-API structures.
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

// IncTotalSent increments the sent counter for the given API.
func (s *Stats) IncTotalSent(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.TotalSent, 1)
	}
}

// IncTotalResponse increments the response counter for the given API.
func (s *Stats) IncTotalResponse(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.TotalResponse, 1)
	}
}

// IncSuccess increments the 2xx counter for the given API.
func (s *Stats) IncSuccess(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.Success, 1)
	}
}

// IncRedirect increments the 3xx counter for the given API.
func (s *Stats) IncRedirect(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.Redirect, 1)
	}
}

// IncClientError increments the 4xx counter for the given API.
func (s *Stats) IncClientError(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.ClientError, 1)
	}
}

// IncServerError increments the 5xx counter for the given API.
func (s *Stats) IncServerError(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.ServerError, 1)
	}
}

// IncInFlight increments the in-flight counter when a request starts.
func (s *Stats) IncInFlight(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.InFlight, 1)
	}
}

// DecInFlight decrements the in-flight counter when a request finishes.
// Ensures it does not underflow.
func (s *Stats) DecInFlight(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		for {
			old := atomic.LoadUint64(&m.InFlight)
			if old == 0 {
				return
			}
			if atomic.CompareAndSwapUint64(&m.InFlight, old, old-1) {
				return
			}
		}
	}
}

// AddLatency records latency (in seconds) for the given API.
// It updates TotalLatency and MaxLatency (protected by latencyMu).
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

// GetAPIMetrics returns a copy of the API metrics (snapshot).
func (s *Stats) GetAPIMetrics() map[string]APIMetric {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]APIMetric, len(s.APIMetrics))
	for name, m := range s.APIMetrics {
		// atomic loads
		totalSent := atomic.LoadUint64(&m.TotalSent)
		totalResp := atomic.LoadUint64(&m.TotalResponse)
		success := atomic.LoadUint64(&m.Success)
		redirect := atomic.LoadUint64(&m.Redirect)
		clientErr := atomic.LoadUint64(&m.ClientError)
		serverErr := atomic.LoadUint64(&m.ServerError)
		inF := atomic.LoadUint64(&m.InFlight)

		// latency aggregates
		m.latencyMu.Lock()
		totalLatency := m.TotalLatency
		maxLatency := m.MaxLatency
		m.latencyMu.Unlock()

		out[name] = APIMetric{
			TotalSent:     totalSent,
			TotalResponse: totalResp,
			Success:       success,
			Redirect:      redirect,
			ClientError:   clientErr,
			ServerError:   serverErr,
			InFlight:      inF,
			TotalLatency:  totalLatency,
			MaxLatency:    maxLatency,
		}
	}
	return out
}

// GetAPIMetricsOrdered returns snapshots preserving the order provided to NewStats.
func (s *Stats) GetAPIMetricsOrdered() []APIMetricEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]APIMetricEntry, 0, len(s.Order))
	for _, name := range s.Order {
		if m, ok := s.APIMetrics[name]; ok {
			totalSent := atomic.LoadUint64(&m.TotalSent)
			totalResp := atomic.LoadUint64(&m.TotalResponse)
			success := atomic.LoadUint64(&m.Success)
			redirect := atomic.LoadUint64(&m.Redirect)
			clientErr := atomic.LoadUint64(&m.ClientError)
			serverErr := atomic.LoadUint64(&m.ServerError)
			inF := atomic.LoadUint64(&m.InFlight)

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
				InFlight:      inF,
				TotalLatency:  totalLatency,
				MaxLatency:    maxLatency,
			}
			out = append(out, APIMetricEntry{Name: name, Metric: snap})
		}
	}
	return out
}
