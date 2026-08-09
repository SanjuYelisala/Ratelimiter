package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"ratelimiter/internal/config"
	"ratelimiter/internal/handler"
	"ratelimiter/internal/middleware"
	"ratelimiter/internal/redis"

	"syscall"
	"time"
)

func startServer(name string, server *http.Server) {
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("%s server error: %v", name, err)
		}
	}()
}

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load configuration: %v", err)
	}

	client := redis.NewClient(cfg.RedisAddr)
	defer client.Close()

	pingContext, cancelPing := context.WithTimeout(
		context.Background(),
		5*time.Second,
	)

	if err := client.Ping(pingContext).Err(); err != nil {
		cancelPing()
		log.Fatalf("Failed to connect to Redis: %v", err)
	}

	cancelPing()

	appRouter := http.NewServeMux()

	appRouter.HandleFunc("/ping", handler.Ping)

	metricsRouter := http.NewServeMux()
	metricsRouter.Handle("/metrics", handler.Metrics())

	var rateLimitedHandler http.Handler
	switch cfg.Algorithm {
	case "token_bucket":
		rateLimitedHandler = middleware.TokenBucket(client, cfg.RateLimit, cfg.Window, cfg.JWTSecret)(appRouter)
	default:
		rateLimitedHandler = middleware.RateLimit(client, cfg.RateLimit, cfg.Window, cfg.JWTSecret)(appRouter)
	}
	log.Printf("Using algorithm: %s", cfg.Algorithm)
	appServer := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           rateLimitedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("App SERVER Starting to Listen on the port %s\n", cfg.Port)

	startServer("Application", appServer)
	metricsServer := &http.Server{
		Addr:              ":" + cfg.MetricsPort,
		Handler:           metricsRouter,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	fmt.Printf("METRICS SERVER Starting to Listen on the port %s\n", cfg.MetricsPort)

	startServer("Metrics", metricsServer)

	shutdownChannel := make(chan os.Signal, 1)
	signal.Notify(shutdownChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdownChannel)

	<-shutdownChannel

	appCtx, appCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer appCancel()

	if err := appServer.Shutdown(appCtx); err != nil {
		log.Printf("Graceful Shutdown failed: %v", err)
	} else {
		log.Println("Application Server shut down cleanly")
	}

	metricsCtx, metricsCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer metricsCancel()
	if err := metricsServer.Shutdown(metricsCtx); err != nil {
		log.Printf("Graceful Shutdown failed: %v", err)
	} else {
		log.Println("Metrics Server shut down cleanly")
	}
}
