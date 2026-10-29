// geoproxy is a small WMS server that serves Google satellite tiles
// to WMS clients such as AutoCAD Map 3D and QGIS.
package main

import (
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

type config struct {
	addr    string // listen address, e.g. 127.0.0.1:8085
	logFile string // empty = log to stdout
}

func main() {
	var cfg config
	flag.StringVar(&cfg.addr, "addr", "127.0.0.1:8085", "listen address (host:port)")
	flag.StringVar(&cfg.logFile, "log", "", "log file path (default: stdout)")
	flag.Parse()

	if cfg.logFile != "" {
		f, err := os.OpenFile(cfg.logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			log.Fatalf("open log file: %v", err)
		}
		defer f.Close()
		log.SetOutput(f)
	} else {
		log.SetOutput(os.Stdout)
	}

	srv := newServer(cfg)
	log.Printf("geoproxy listening on http://%s/wms", cfg.addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server: %v", err)
	}
}

// newServer builds the HTTP server with all routes.
func newServer(cfg config) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/wms", handleWMS)

	return &http.Server{
		Addr:              cfg.addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// handleWMS is a placeholder until step 2 (WMS parsing).
func handleWMS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotImplemented)
	io.WriteString(w, "geoproxy: WMS not implemented yet\n")
}

// statusRecorder remembers the status code so we can log it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// logRequests logs every request: method, path, full query string,
// status and duration.
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s?%s -> %d (%s)",
			r.Method, r.URL.Path, r.URL.RawQuery, rec.status,
			time.Since(start).Round(time.Millisecond))
	})
}
