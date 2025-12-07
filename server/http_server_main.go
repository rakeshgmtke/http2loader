package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime/pprof"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// RouteConfig represents the configuration for a specific route
type RouteConfig struct {
	Path                      string            `json:"path"`
	Method                    string            `json:"method"`
	Status                    int               `json:"status"`
	Delay                     int               `json:"delay,omitempty"`
	IncludeRspBodyfromReqBody bool              `json:"includeRspBodyfromReqBody,omitempty"`
	Body                      json.RawMessage   `json:"body,omitempty"`
	Headers                   map[string]string `json:"headers"`
}

// Config represents the server configuration
type Config struct {
	Routes []RouteConfig `json:"routes"`
}

// LoadConfig loads the configuration from a JSON file
func LoadConfig(configPath string) (*Config, error) {
	file, err := os.Open(configPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var config Config
	if err := json.NewDecoder(file).Decode(&config); err != nil {
		return nil, err
	}
	return &config, nil
}

// Metrics holds the metrics data
type Metrics struct {
	RouteMetrics  map[string]*RouteMetric
	RouteOrder    []string
	TotalReceived int
	//UnknownMethod int
	UnknownAPI int
	Lock       sync.Mutex
}

// RouteMetric holds the metrics for a specific route
type RouteMetric struct {
	Count          int // total http/http2 msgs count
	Http2          int // total only http2 msgs count
	previous_Http2 int // previous http/http2 msgs count recevied
	//Timestamp      []time.Time
}

// Initialize metrics based on the routes in the config
func initializeMetrics(config *Config) *Metrics {
	metrics := &Metrics{
		RouteMetrics: make(map[string]*RouteMetric),
		RouteOrder:   make([]string, len(config.Routes)),
	}
	for i, route := range config.Routes {
		//metrics.RouteMetrics[route.Path] = &RouteMetric{}
		//metrics.RouteOrder[i] = route.Path
		method_api := route.Method + " ---> " + route.Path
		metrics.RouteMetrics[method_api] = &RouteMetric{}
		metrics.RouteOrder[i] = method_api
	}
	return metrics
}

/*
// Calculate TPS (Transactions Per Second) for each route
//below TPS Calculatation will have go routine stuck when calculateTPS due metric.Timestamp is huge data.. so come up with simple soultion to Calculate TPS
func calculateTPS(metrics *Metrics, interval time.Duration) map[string]float64 {
	tps := make(map[string]float64)
	now := time.Now()
	for path, metric := range metrics.RouteMetrics {
		count := 0
		for _, timestamp := range metric.Timestamp {
			if now.Sub(timestamp) <= interval {
				count++
			}
		}
		tps[path] = float64(count) / interval.Seconds()
	}
	return tps
}
*/

// Print metrics every second in a column format
func printMetrics(metrics *Metrics, ip, ports string) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	fmt.Printf("------------------------------------------------------------------------------------------------------\n")
	fmt.Println("Metrics Report:")
	fmt.Printf("------------------------------------------------------------------------------------------------------\n")
	fmt.Printf("%-30s %-15s %-15s %-25s\n", "Metric", "Count", "TPS", "Date Time")

	for {
		<-ticker.C
		now := time.Now().Format("2006-01-02 15:04:05")
		fmt.Printf("\033[H\033[2J") // Clear the terminal screen (works in most terminals)
		fmt.Printf("------------------------------------------------------------------------------------------------------\n")
		fmt.Println("Metrics Report: ", now)
		fmt.Printf("Listening on IP: %s, Ports: %s\n", ip, ports)
		fmt.Printf("------------------------------------------------------------------------------------------------------\n")
		fmt.Printf("%-60s %-15s %-15s %-15s\n", "API", "Count", "TPS", "HTTP2")
		fmt.Printf("======================================================================================================\n")
		metrics.Lock.Lock()
		//tps := calculateTPS(metrics, time.Second)
		for _, path := range metrics.RouteOrder {
			if metric, ok := metrics.RouteMetrics[path]; ok {
				count := metric.Count
				http2 := metric.Http2
				tps := count - metric.previous_Http2
				metric.previous_Http2 = count
				//fmt.Printf("%-60s %-15d %-15.2f %-15d\n", path, count, tps[path], http2)
				fmt.Printf("%-60s %-15d %-15d %-15d\n", path, count, tps, http2)
			}
		}
		fmt.Printf("======================================================================================================\n")
		fmt.Printf("%-60s %-15d %-15.2f\n", "Total Received", metrics.TotalReceived, 00.0)
		//fmt.Printf("%-60s %-15d %-15.2f\n", "Unknown Method", metrics.UnknownMethod, 00.0)
		fmt.Printf("%-60s %-15d %-15.2f\n", "Unknown API", metrics.UnknownAPI, 00.0)
		fmt.Printf("======================================================================================================\n")
		metrics.Lock.Unlock()
	}
}

// Handle the response based on route configuration
func handlerResponse(w http.ResponseWriter, r *http.Request, route RouteConfig, reqBody []byte, enableLogging bool) {
	// Set the response headers
	w.Header().Set("Content-Type", "application/json")
	for key, value := range route.Headers {
		w.Header().Set(key, value)
	}

	// Set the status code
	w.WriteHeader(route.Status)

	// Determine the response body
	var responseBody []byte
	if route.IncludeRspBodyfromReqBody {
		responseBody = reqBody
	} else if len(route.Body) > 1 {
		// Validate JSON
		var jsonData map[string]interface{}
		if err := json.Unmarshal([]byte(route.Body), &jsonData); err != nil {
			// Log the error if logging is enabled
			if enableLogging {
				log.Printf("Error in JSON configuration for path %s: %v\n", r.URL.Path, err)
			}
			http.Error(w, "Invalid JSON in configuration", http.StatusInternalServerError)
			return
		}
		responseBody = []byte(route.Body)
	}

	// Log response details if logging is enabled
	if enableLogging {
		log.Printf("Response sent delayed: %d ms with status: %d at %s\nURL: %s\nHeader: %s \nIncludeRspBodyfromReqBody: %t \nResponse body: %s\n", route.Delay, route.Status, time.Now().Format("2006-01-02 15:04:05"), r.URL, route.Headers, route.IncludeRspBodyfromReqBody, responseBody)
	}

	// Apply the delay if specified and if based on Percentage delay
	if route.Delay > 0 { //&& PercentageDelay(float64(route.PercentageDelay)){
		if enableLogging {
			log.Printf("delay is applied for URL: %s\nHeader: %s \nIncludeRspBodyfromReqBody: %t \nResponse body: %s\n Delayed By: %d ms", r.URL, route.Headers, route.IncludeRspBodyfromReqBody, responseBody, route.Delay)
		}

		delayDuration := time.Duration(route.Delay) * time.Millisecond
		done := make(chan struct{})

		time.AfterFunc(delayDuration, func() {
			// Write the response body after the delay
			if _, err := w.Write(responseBody); err != nil {
				// Log the error Write is failed
				http.Error(w, "Unable to write response body", http.StatusInternalServerError)
				return
			}

			// Flush the response if HTTP/2
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}

			close(done)
		})

		// Wait for the delay to complete
		<-done

	} else {
		// If no delay, write the response immediately
		if _, err := w.Write(responseBody); err != nil {
			// Log the error Write is failed
			http.Error(w, "Unable to write response body", http.StatusInternalServerError)
			return
		}

		// Flush the response if HTTP/2
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

	}

}

// Main handler function
func handler(w http.ResponseWriter, r *http.Request, config *Config, metrics *Metrics, enableLogging bool) {

	// request Body is storing to send in response Body
	reqBody, err := io.ReadAll(r.Body)
	if err != nil {
		reqBody = []byte{}
	}
	// Log the error if logging is enabled
	if enableLogging {
		log.Printf("Request received: %s %s %s %s\n", r.Method, r.URL.Path, r.Header, reqBody)
	}

	metrics.Lock.Lock()
	metrics.TotalReceived++
	metrics.Lock.Unlock()

	for _, route := range config.Routes {
		matched_path, _ := regexp.MatchString(route.Path, r.URL.Path)
		matched_method, _ := regexp.MatchString(route.Method, r.Method)
		if matched_path && matched_method {
			method_api := route.Method + " ---> " + route.Path
			metrics.Lock.Lock()
			metrics.RouteMetrics[method_api].Count++
			//fmt.Println("Printing Proto:", r.Proto)
			if r.Proto == "HTTP/2.0" {
				metrics.RouteMetrics[method_api].Http2++
			}
			//metrics.RouteMetrics[method_api].Timestamp = append(metrics.RouteMetrics[method_api].Timestamp, time.Now())
			metrics.Lock.Unlock()
			handlerResponse(w, r, route, reqBody, enableLogging)
			return
		}
	}

	metrics.Lock.Lock()
	metrics.UnknownAPI++
	metrics.Lock.Unlock()
	//fmt.Println("API is Not Allowed for Request received, Sending 404 Not Found: %s %s\n", r.Method, r.URL.Path)
	log.Printf("API is Not Allowed for Request received, Sending 404 Not Found: %s %s\n", r.Method, r.URL.Path)
	http.Error(w, "Not Found", http.StatusNotFound)
}

func startServer(wg *sync.WaitGroup, addr string, config *Config, metrics *Metrics, enableLogging bool) *http.Server {
	// Create HTTP handler (replace with your actual handler)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, config, metrics, enableLogging)
	})

	// Create an HTTP/2 server instance
	h2s := &http2.Server{}

	// Create the HTTP server
	server := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(mux, h2s), // Wrap mux in h2c handler for HTTP/2 support
	}

	// Start the server in a goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	log.Printf("Server started on %s\n", addr)
	return server
}

// gracefulShutdown handles graceful shutdown of the server
func gracefulShutdown(servers []*http.Server, wg *sync.WaitGroup, shutdownTimeout time.Duration) {

	// Create a channel to listen for interrupt signals
	stop := make(chan os.Signal, 1)
	// Listen for interrupt and termination signals
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	// Block until a signal is received
	<-stop

	// Log shutdown start
	log.Println("Shutting down server...")

	// Create a context with timeout for the shutdown process
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	// Attempt to gracefully shut down each server
	for _, server := range servers {
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("Server on %s forced to shutdown: %v", server.Addr, err)
		}
	}
	// Wait for the server goroutine to finish
	wg.Wait()

	log.Println("Server shutdown completed.")
}

func main() {
	configPath := flag.String("config", "config.json", "Path to configuration file")
	ip := flag.String("ip", "0.0.0.0", "IP address to listen on")
	ports := flag.String("ports", "8080", "Comma-separated list of ports to listen on for HTTP/1.1 and HTTP2")
	enableLogging := flag.Bool("log", false, "Enable logging")
	cpuprof := flag.Bool("cpupprof", false, "Enable CPU profiling")
	logPath := flag.String("log_path", "/tmp/server.log", "Path to log file")
	flag.Parse()

	// Open log file if logging is enabled
	//if *enableLogging {
	logFile, err := os.OpenFile(*logPath, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
	if err != nil {
		log.Fatalf("Error opening log file: %v", err)
	}
	defer logFile.Close()
	log.SetOutput(logFile)
	//}

	log.Printf("Starting HTTP/1.1 and HTTP/2 server on %s:%s \n", *ip, *ports)

	// Load the configuration
	config, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Error loading configuration: %v", err)
	}

	// Initialize metrics
	metrics := initializeMetrics(config)

	var wg sync.WaitGroup

	// To store server instances
	var servers []*http.Server

	// Split the ports by comma
	portList := strings.Split(*ports, ",")

	// Start a server for each port in the portList
	for _, port := range portList {
		addr := *ip + ":" + strings.TrimSpace(port)
		server := startServer(&wg, addr, config, metrics, *enableLogging)
		// Collect the server instance
		servers = append(servers, server)
	}

	// Start metrics printing
	go printMetrics(metrics, *ip, *ports)

	// CPU Profiling if its enabled
	if *cpuprof == true {
		profilerFile, _ := os.Create("httpserver.cpuprof")
		pprof.StartCPUProfile(profilerFile)
		defer pprof.StopCPUProfile()
	}

	// Handle graceful shutdown
	gracefulShutdown(servers, &wg, 5*time.Second)

}
