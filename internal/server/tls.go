package server

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"armdash/internal/config"
)

const (
	tlsCertKey = "core.tls_cert"
	tlsKeyKey  = "core.tls_key"
)

// TLSFiles returns the certificate and key paths, both empty when TLS is off.
// One without the other is an error rather than a quiet fall back to HTTP.
func TLSFiles(cfg *config.Config) (cert, key string, err error) {
	cert, key = cfg.Get(tlsCertKey), cfg.Get(tlsKeyKey)
	if (cert == "") != (key == "") {
		return "", "", fmt.Errorf("%s and %s must be set together",
			config.EnvName(tlsCertKey), config.EnvName(tlsKeyKey))
	}
	return cert, key, nil
}

// PlainHandler serves the plain HTTP listener once TLS is on. /metrics stays
// here so Prometheus keeps scraping over loopback without having to trust the
// certificate, everything else is sent to the HTTPS listener at tlsAddr.
//
// There is deliberately no Strict-Transport-Security header: a pin on a LAN
// host name signed by a private CA would lock out every device that does not
// trust that CA.
func (s *Server) PlainHandler(tlsAddr string) http.Handler {
	_, port, err := net.SplitHostPort(tlsAddr)
	if err != nil || port == "" {
		port = "443"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) { s.tree().handleMetrics(w, r) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		host = strings.Trim(host, "[]")
		target := strings.TrimSuffix(net.JoinHostPort(host, port), ":443")
		http.Redirect(w, r, "https://"+target+r.URL.RequestURI(), http.StatusMovedPermanently)
	})
	return mux
}
