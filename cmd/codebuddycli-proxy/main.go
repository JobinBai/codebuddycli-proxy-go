package main

import (
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/JobinBai/codebuddycli-proxy-go/internal/proxy"
)

var version = "dev"

func main() {
	cfg, err := proxy.LoadConfigFromEnv()
	if err != nil {
		slog.New(slog.NewJSONHandler(os.Stdout, nil)).Error("configuration_error", "error", err)
		os.Exit(1)
	}
	h := proxy.New(cfg, version)
	srv := &http.Server{
		Addr:              cfg.Host + ":" + cfg.Port,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	h.LogStartup(srv.Addr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		h.LogServerError(err)
		os.Exit(1)
	}
}
