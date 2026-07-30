package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/JobinBai/codebuddycli-proxy-go/internal/proxy"
)

var version = "dev"

func main() {
	cfg, err := proxy.LoadConfigFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	h := proxy.New(cfg, version)
	srv := &http.Server{
		Addr:              cfg.Host + ":" + cfg.Port,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	fmt.Printf("codebuddycli-proxy %s listening on http://%s (upstream: %s)\n", version, srv.Addr, cfg.BaseURL)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
