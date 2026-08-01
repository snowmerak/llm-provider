package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/snowmerak/llm-provider/gateway"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath := flag.String("config", "llm-provider.json", "path to the JSON configuration file")
	listenOverride := flag.String("listen", "", "override the configured listen address")
	flag.Parse()

	config, err := gateway.LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *listenOverride != "" {
		config.Listen = *listenOverride
	}
	if config.Listen == "" {
		config.Listen = "127.0.0.1:8080"
	}
	router, err := gateway.New(config)
	if err != nil {
		return err
	}
	defer router.Close()

	server := &http.Server{
		Addr:              config.Listen,
		Handler:           router.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownContext)
	}()
	fmt.Printf("llm-provider listening on %s\n", config.Listen)
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
