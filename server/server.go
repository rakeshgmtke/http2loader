package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"syscall"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// lightweight subset of config: only APIs are needed by the mock server
type API struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Method string `json:"method"`
	Body   string `json:"body"`
}

type Config struct {
	APIs []API `json:"apis"`
}

func main() {
	// try a few locations for config.json (cwd, parent, executable dir)
	var cfg *Config
	var err error
	tryPaths := []string{"./config.json", "../config.json"}
	// executable dir
	if exe, eerr := os.Executable(); eerr == nil {
		tryPaths = append(tryPaths, filepath.Join(filepath.Dir(exe), "config.json"))
	}

	for _, p := range tryPaths {
		if cfg, err = loadConfig(p); err == nil {
			break
		}
	}
	if cfg == nil {
		log.Printf("failed to load config.json from tried locations: %v", tryPaths)
	}

	mux := http.NewServeMux()

	// register handlers for configured APIs; fall back to a generic handler if config is missing
	if cfg != nil && len(cfg.APIs) > 0 {
		for _, a := range cfg.APIs {
			u, err := url.Parse(a.URL)
			if err != nil {
				log.Printf("invalid URL for api %s: %v", a.Name, err)
				continue
			}
			p := u.Path
			// register per-path handler
			registerAPIHandler(mux, p, a)
			log.Printf("registered mock endpoint %s %s -> %s", a.Method, p, a.Name)
		}
	} else {
		// generic handler when no config present
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "h2c OK: %s %s", r.Method, r.URL.Path)
		})
	}

	srv := &http.Server{
		Addr:         ":3000",
		Handler:      h2c.NewHandler(mux, &http2.Server{}),
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	go func() {
		log.Printf("h2c mock server running on http://localhost%v", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()

	// graceful shutdown
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	log.Println("shutting down server...")
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("graceful shutdown failed: %v", err)
	}
	log.Println("server stopped")
}

func registerAPIHandler(mux *http.ServeMux, p string, a API) {
	// ensure no trailing slash differences
	p = path.Clean(p)
	mux.HandleFunc(p, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != a.Method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write([]byte(a.Body)); err != nil {
			log.Printf("write response error for %s: %v", a.Name, err)
		}
	})
	// also register with trailing slash to be permissive
	if p != "/" {
		mux.HandleFunc(p+"/", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != a.Method {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if _, err := w.Write([]byte(a.Body)); err != nil {
				log.Printf("write response error for %s: %v", a.Name, err)
			}
		})
	}
}

func loadConfig(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var cfg Config
	dec := json.NewDecoder(f)
	if err := dec.Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
