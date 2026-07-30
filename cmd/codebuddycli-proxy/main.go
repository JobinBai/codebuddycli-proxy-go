package main

import (
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/JobinBai/codebuddycli-proxy-go/internal/proxy"
)

var version = "dev"

func main() {
	cfg, err := proxy.LoadConfigFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s ERROR [codebuddy-proxy] configuration error=%q\n", time.Now().Format("2006-01-02 15:04:05.000"), err)
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
