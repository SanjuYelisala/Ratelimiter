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

	router := http.NewServeMux()

	router.HandleFunc("/ping", handler.Ping)

	rateLimitedHandler := middleware.RateLimit(client, cfg.RateLimit, cfg.Window, cfg.JWTSecret)(router)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           rateLimitedHandler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	fmt.Printf("SERVER Starting to Listen on the port %s\n", cfg.Port)

	go func() {

		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server error %v", err)
		}
	}()

	shutdownChannel := make(chan os.Signal, 1)
	signal.Notify(shutdownChannel, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(shutdownChannel)

	<-shutdownChannel

	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("Graceful Shutdown failed: %v", err)
	} else {
		log.Println("Server shut down cleanly")
	}

}
