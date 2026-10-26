// Standalone server for local development of the same plugin GUI/API.
package main

import (
	"context"
	"crypto/subtle"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/txsdx/cpa-plugin-request-recorder/internal/recorder"
	"github.com/txsdx/cpa-plugin-request-recorder/internal/wire"
	"github.com/txsdx/cpa-plugin-request-recorder/web"
)

func main() {
	addr := flag.String("listen", "127.0.0.1:8787", "development listen address")
	db := flag.String("db", "data/request-recorder.db", "database")
	logs := flag.String("logs-dir", "", "CPA logs directory")
	flag.Parse()
	token := os.Getenv("RECORDER_MANAGEMENT_KEY")
	if token == "" {
		log.Fatal("set RECORDER_MANAGEMENT_KEY (development server only)")
	}
	c := recorder.DefaultConfig()
	c.DBPath = *db
	c.LogsDir = *logs
	e, err := recorder.New(c)
	if err != nil {
		log.Fatal(err)
	}
	defer e.Close()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			http.Redirect(w, r, recorder.ResourceBase+"/index.html", 302)
			return
		}
		if strings.HasPrefix(r.URL.Path, recorder.APIBase+"/") {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "unauthorized", 401)
				return
			}
		} else if !strings.HasPrefix(r.URL.Path, recorder.ResourceBase+"/") {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8192))
		if err != nil {
			http.Error(w, "request too large", 413)
			return
		}
		resp := e.Management(wire.ManagementRequest{Method: r.Method, Path: r.URL.Path, Headers: r.Header, Query: r.URL.Query(), Body: body}, web.Files)
		for k, vs := range resp.Headers {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(resp.Body)
	})
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("development server listening on %s", ln.Addr())
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Print(err)
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(ctx)
}
