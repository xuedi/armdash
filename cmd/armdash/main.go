// Command armdash serves the dashboard.
//
// Development runs on the desktop against whatever Prometheus is reachable;
// deployment to the server is the same binary cross-compiled for arm64. Ports 80
// and 443 are a deployment concern, set in the server's env file. The defaults
// stay on high loopback ports so a development checkout never collides with
// anything else on the desktop.
package main

import (
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"armdash/internal/config"
	"armdash/internal/server"
	"armdash/internal/system"
	"armdash/internal/systems/fritzhome"
	"armdash/internal/systems/host"
	"armdash/internal/version"
)

// systems are every compiled-in system, in navbar order. Whether each appears
// is a config decision, see internal/system. The order is written here rather
// than left to each package's own init, which Go runs in an order the source
// does not show.
var systems = []func() system.System{
	func() system.System { return &host.Host{} },
	func() system.System { return &fritzhome.FritzHome{} },
}

func init() {
	for _, s := range systems {
		system.Register(s)
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "passwd" {
		if err := passwd(os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "armdash passwd:", err)
			os.Exit(1)
		}
		return
	}

	addr := flag.String("addr", "",
		"listen address, overrides AD_CORE_ADDR; use :8080 to accept connections from the LAN")
	envFiles := flag.String("env", ".env.dist,.env.local",
		"comma-separated env files, later ones win; the real environment wins over all")
	debug := flag.Bool("debug", false, "verbose logging")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()

	// A deployed binary has to say what it is. Without this the only way to tell
	// which build is running is to checksum it against a local build.
	if *showVersion {
		fmt.Println(version.Version)
		return
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	files := strings.Split(*envFiles, ",")
	cfg, err := config.Load(files...)
	if err != nil {
		log.Error("loading config", "err", err)
		os.Exit(1)
	}

	// Settings saved on the page live in the data directory. Without one the
	// page shows them read-only, as the env files have them.
	if dir := server.DataDir(cfg); dir != "" {
		if err := cfg.UseStore(filepath.Join(dir, "settings.json")); err != nil {
			log.Error("loading saved settings", "err", err)
			os.Exit(1)
		}
	}

	// The flag wins when given, so a one-off override still works, but a packaged
	// install keeps the address in the same file as every other setting.
	listen := *addr
	if listen == "" {
		listen = cfg.GetOr("core.addr", "127.0.0.1:9494")
	}
	certFile, keyFile, err := server.TLSFiles(cfg)
	if err != nil {
		log.Error("loading config", "err", err)
		os.Exit(1)
	}

	srv, err := server.New(cfg, log, files)
	if err != nil {
		log.Error("starting server", "err", err)
		os.Exit(1)
	}

	if certFile == "" {
		log.Info("armdash listening", "version", version.Version, "addr", listen, "tls", "off", "env", *envFiles)
		exitOnError(log, newHTTPServer(listen, srv).ListenAndServe())
		return
	}

	// Loaded here rather than by ListenAndServeTLS so a wrong path fails before
	// anything listens, with both file names in the log.
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Error("loading TLS certificate", "cert", certFile, "key", keyFile, "err", err)
		os.Exit(1)
	}
	tlsListen := cfg.GetOr("core.tls_addr", "127.0.0.1:9495")
	secure := newHTTPServer(tlsListen, srv)
	secure.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	plain := newHTTPServer(listen, srv.PlainHandler(tlsListen))

	log.Info("armdash listening", "version", version.Version, "addr", listen, "tls_addr", tlsListen, "env", *envFiles)
	// Whichever listener stops first takes the process down, so systemd restarts
	// both rather than leaving half a server running.
	errc := make(chan error, 2)
	go func() { errc <- plain.ListenAndServe() }()
	go func() { errc <- secure.ListenAndServeTLS("", "") }()
	exitOnError(log, <-errc)
}

func newHTTPServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a slow Prometheus range query over a year of data
		// can legitimately take a while, and cutting it off mid-response would
		// look like a bug in the chart.
		IdleTimeout: 60 * time.Second,
	}
}

func exitOnError(log *slog.Logger, err error) {
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
