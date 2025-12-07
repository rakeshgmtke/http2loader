package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Test loadConfig with valid configuration
func TestLoadConfigValid(t *testing.T) {
	configJSON := `{
		"global": {
			"tps": 100,
			"max_inflight": 500,
			"pending_buf": 1000,
			"request_timeout_seconds": 10,
			"max_retries": 2,
			"backoff_ms": 50,
			"metrics_addr": ":9090",
			"max_idle_per_host": 200,
			"idle_conn_seconds": 90
		},
		"apis": [
			{
				"name": "test_api",
				"url": "http://localhost:3000/test",
				"method": "POST",
				"body": "{\"test\":true}"
			}
		]
	}`

	tmpfile, err := os.CreateTemp("", "config*.json")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	tmpfile.WriteString(configJSON)
	tmpfile.Close()

	cfg, err := loadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if cfg.Global.TPS != 100 {
		t.Errorf("Expected TPS=100, got %v", cfg.Global.TPS)
	}
	if cfg.Global.MaxInflight != 500 {
		t.Errorf("Expected MaxInflight=500, got %v", cfg.Global.MaxInflight)
	}
	if len(cfg.APIs) != 1 {
		t.Errorf("Expected 1 API, got %d", len(cfg.APIs))
	}
}

// Test loadConfig applies defaults correctly
func TestLoadConfigDefaults(t *testing.T) {
	configJSON := `{
		"global": {},
		"apis": [
			{
				"name": "api1",
				"url": "http://localhost:3000/api",
				"method": "GET",
				"body": ""
			}
		]
	}`

	tmpfile, err := os.CreateTemp("", "config*.json")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	tmpfile.WriteString(configJSON)
	tmpfile.Close()

	cfg, err := loadConfig(tmpfile.Name())
	if err != nil {
		t.Fatalf("loadConfig failed: %v", err)
	}

	if cfg.Global.TPS != 100 {
		t.Errorf("Default TPS should be 100, got %v", cfg.Global.TPS)
	}
	if cfg.Global.MaxInflight != 1000 {
		t.Errorf("Default MaxInflight should be 1000, got %v", cfg.Global.MaxInflight)
	}
	if cfg.Global.RequestTimeout != 15 {
		t.Errorf("Default RequestTimeout should be 15, got %v", cfg.Global.RequestTimeout)
	}
}

// Test loadConfig fails with no APIs
func TestLoadConfigNoAPIs(t *testing.T) {
	configJSON := `{"global": {"tps": 100}, "apis": []}`

	tmpfile, err := os.CreateTemp("", "config*.json")
	if err != nil {
		t.Fatalf("Failed to create temp file: %v", err)
	}
	defer os.Remove(tmpfile.Name())

	tmpfile.WriteString(configJSON)
	tmpfile.Close()

	_, err = loadConfig(tmpfile.Name())
	if err == nil {
		t.Error("Expected error for empty APIs, got nil")
	}
}

// Test loadConfig fails with missing file
func TestLoadConfigMissingFile(t *testing.T) {
	_, err := loadConfig("/nonexistent/path/config.json")
	if err == nil {
		t.Error("Expected error for missing file, got nil")
	}
}

// Test newHTTP2Client creates valid client
func TestNewHTTP2Client(t *testing.T) {
	client := newHTTP2Client(200, 90*time.Second)

	if client == nil {
		t.Error("newHTTP2Client returned nil")
	}

	if client.Transport == nil {
		t.Error("HTTP transport is nil")
	}
}

// Test Config JSON unmarshaling
func TestConfigJSONUnmarshal(t *testing.T) {
	configJSON := `{
		"global": {
			"tps": 500,
			"max_inflight": 2000,
			"pending_buf": 5000,
			"request_timeout_seconds": 20,
			"max_retries": 3,
			"backoff_ms": 100
		},
		"apis": [
			{
				"name": "api_post",
				"url": "https://api.example.com/post",
				"method": "POST",
				"body": "{\"payload\":\"data\"}"
			},
			{
				"name": "api_get",
				"url": "https://api.example.com/get",
				"method": "GET",
				"body": ""
			}
		]
	}`

	var cfg Config
	err := json.Unmarshal([]byte(configJSON), &cfg)
	if err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	if cfg.Global.TPS != 500 {
		t.Errorf("Expected TPS=500, got %v", cfg.Global.TPS)
	}
	if len(cfg.APIs) != 2 {
		t.Errorf("Expected 2 APIs, got %d", len(cfg.APIs))
	}
	if cfg.APIs[0].Method != "POST" {
		t.Errorf("Expected POST, got %v", cfg.APIs[0].Method)
	}
}

// Test Job and API structures
func TestJobStructure(t *testing.T) {
	api := API{
		Name:   "test_api",
		URL:    "http://localhost:3000/api",
		Method: "POST",
		Body:   "{\"test\":true}",
	}

	job := Job{
		API: api,
		Seq: 123,
	}

	if job.Seq != 123 {
		t.Errorf("Expected job.Seq=123, got %d", job.Seq)
	}
	if job.API.Name != "test_api" {
		t.Errorf("Expected API name 'test_api', got %v", job.API.Name)
	}
}

// Benchmark: schedulerGlobal job generation at 10k TPS
func BenchmarkSchedulerGlobal(b *testing.B) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	apis := []API{
		{Name: "api1", URL: "http://localhost/1", Method: "GET", Body: ""},
	}

	pending := make(chan Job, 1000)

	b.ResetTimer()
	go schedulerGlobal(ctx, 10000.0, apis, pending)

	for i := 0; i < b.N; i++ {
		<-pending
	}

	cancel()
}

// Benchmark: Config loading
func BenchmarkLoadConfig(b *testing.B) {
	configJSON := `{
		"global": {"tps": 100, "max_inflight": 500},
		"apis": [{"name": "api1", "url": "http://localhost/test", "method": "GET", "body": ""}]
	}`

	tmpfile, _ := os.CreateTemp("", "config*.json")
	defer os.Remove(tmpfile.Name())
	tmpfile.WriteString(configJSON)
	tmpfile.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		loadConfig(tmpfile.Name())
	}
}
