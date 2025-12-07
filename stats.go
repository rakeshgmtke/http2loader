package main

import (
	"sync"
	"sync/atomic"
)

// APIMetric holds counters for a single API.
type APIMetric struct {
	Sent          uint64
	TotalResponse uint64
	Success       uint64 // 2xx
	Redirect      uint64 // 3xx
	ClientError   uint64 // 4xx
	ServerError   uint64 // 5xx
	TotalLatency  float64 // in seconds
	mu            sync.RWMutex
}

// Stats tracks key performance metrics.
type Stats struct {
	APIMetrics map[string]*APIMetric
	mu         sync.RWMutex
}

// NewStats creates a new Stats object.
func NewStats(apis []API) *Stats {
	s := &Stats{
		APIMetrics: make(map[string]*APIMetric),
	}
	for _, api := range apis {
		s.APIMetrics[api.Name] = &APIMetric{}
	}
	return s
}

// IncSent increments the sent counter for a given API.
func (s *Stats) IncSent(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.Sent, 1)
	}
}

// IncSuccess increments the success counter for a given API.
func (s *Stats) IncSuccess(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.Success, 1)
	}
}

// IncRedirect increments the redirect counter for a given API.
func (s *Stats) IncRedirect(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.Redirect, 1)
	}
}

// IncClientError increments the client error counter for a given API.
func (s *Stats) IncClientError(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.ClientError, 1)
	}
}

// IncServerError increments the server error counter for a given API.
func (s *Stats) IncServerError(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.ServerError, 1)
	}
}

// AddLatency adds latency for a given API.
func (s *Stats) AddLatency(apiName string, latency float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		m.mu.Lock()
		m.TotalLatency += latency
		m.mu.Unlock()
	}
}

// IncTotalResponse increments the total response counter for a given API.
func (s *Stats) IncTotalResponse(apiName string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if m, ok := s.APIMetrics[apiName]; ok {
		atomic.AddUint64(&m.TotalResponse, 1)
	}
}

// GetAPIMetrics returns a copy of the API metrics.
func (s *Stats) GetAPIMetrics() map[string]APIMetric {
	s.mu.RLock()
	defer s.mu.RUnlock()
	metrics := make(map[string]APIMetric)
	for name, m := range s.APIMetrics {
		m.mu.RLock()
		metrics[name] = APIMetric{
			Sent:          atomic.LoadUint64(&m.Sent),
			TotalResponse: atomic.LoadUint64(&m.TotalResponse),
			Success:       atomic.LoadUint64(&m.Success),
			Redirect:      atomic.LoadUint64(&m.Redirect),
			ClientError:   atomic.LoadUint64(&m.ClientError),
			ServerError:   atomic.LoadUint64(&m.ServerError),
			TotalLatency:  m.TotalLatency,
		}
		m.mu.RUnlock()
	}
	return metrics
}
