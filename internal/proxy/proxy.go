package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"

	"github.com/armon/go-socks5"
	"go.uber.org/zap"
)

func init() {
	logger, _ := zap.NewDevelopment()
	zap.ReplaceGlobals(logger)
}

var (
	logger      = zap.S().Named("proxy")
	httpLogger  = zap.S().Named("http")
	socksLogger = zap.S().Named("socks")
)

// StartHTTPProxy starts an HTTP proxy server on the specified port
func StartHTTPProxy(port int) error {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zap.S().Named("http").Debugw("client connected",
			"remote_addr", r.RemoteAddr,
			"method", r.Method,
			"url", r.URL.String(),
		)

		if r.Method == http.MethodConnect {
			handleTunneling(w, r)
		} else {
			handleHTTP(w, r)
		}
	})

	server := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: handler,
	}

	zap.S().Named("http").Infow("starting proxy server", "port", port)
	return server.ListenAndServe()
}

func handleTunneling(w http.ResponseWriter, r *http.Request) {
	dest_conn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		zap.S().Named("http").Errorw("tunnel connection failed", "error", err)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	client_conn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	go transfer(dest_conn, client_conn)
	go transfer(client_conn, dest_conn)
}

func transfer(destination io.WriteCloser, source io.ReadCloser) {
	defer destination.Close()
	defer source.Close()
	io.Copy(destination, source)
}

func handleHTTP(w http.ResponseWriter, r *http.Request) {
	// If the request URL is absolute (proxy request), parse it
	// Otherwise, construct URL from the Host header (direct request)
	targetURL, err := url.Parse(r.URL.String())
	if err != nil {
		httpLogger.Errorw("failed to parse URL", "error", err)
		return
	}

	// Ensure scheme is set for the outgoing request
	if targetURL.Scheme == "" {
		targetURL.Scheme = "http"
	}

	// Log if request contains authentication
	if auth := r.Header.Get("Authorization"); auth != "" {
		zap.S().Named("http").Infow("Forwarding authenticated request", "host", targetURL.Host)
	}

	// Set proper headers
	r.URL = targetURL
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Proto", targetURL.Scheme)
	r.Header.Set("X-Real-IP", r.RemoteAddr)
	r.Host = targetURL.Host

	httpLogger.Debugw("proxying request",
		"method", r.Method,
		"url", targetURL.String(),
		"remote_ip", r.RemoteAddr,
		"headers", r.Header,
	)

	// Create a new proxy handler
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			targetURL, err := url.Parse(req.URL.String())
			if err != nil {
				zap.S().Named("http").Errorw("failed to parse URL", "error", err)
				return
			}
			if targetURL.Scheme == "" {
				targetURL.Scheme = "http"
			}

			httpLogger.Debugw("proxying request",
				"method", req.Method,
				"url", targetURL.String(),
				"remote_ip", req.RemoteAddr,
				"headers", req.Header,
			)

			req.URL = targetURL
			req.Header.Set("X-Forwarded-Host", req.Host)
			req.Header.Set("X-Forwarded-Proto", targetURL.Scheme)
			req.Header.Set("X-Real-IP", req.RemoteAddr)
			req.Host = targetURL.Host
		},
		ModifyResponse: func(resp *http.Response) error {
			// Add CORS headers to allow browser requests
			resp.Header.Set("Access-Control-Allow-Origin", "*")
			resp.Header.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			resp.Header.Set("Access-Control-Allow-Headers", "*")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			zap.S().Named("http").Errorw("proxy error", "error", err)
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("Proxy Error"))
		},
	}
	proxy.ServeHTTP(w, r)
}

// StartSocksProxy starts a SOCKS5 proxy server on the specified port
func StartSocksProxy(port int) error {
	// Create listener first to log connections
	addr := fmt.Sprintf(":%d", port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to start SOCKS listener: %v", err)
	}

	// Create SOCKS5 server with connection logging
	server, err := socks5.New(&socks5.Config{
		Logger: log.New(os.Stdout, "[SOCKS5] ", log.LstdFlags),
		// Add connection callback
		Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
			zap.S().Named("socks").Debugw("connecting to destination",
				"network", network,
				"address", addr,
			)
			return net.Dial(network, addr)
		},
	})
	if err != nil {
		return fmt.Errorf("failed to create SOCKS5 server: %v", err)
	}

	zap.S().Named("socks").Infow("starting proxy server", "port", port)
	return server.Serve(&loggingListener{listener})
}

type loggingListener struct {
	net.Listener
}

func (l *loggingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		zap.S().Named("socks").Debugw("client connected",
			"remote_addr", conn.RemoteAddr().String(),
		)
	}
	return conn, err
}
