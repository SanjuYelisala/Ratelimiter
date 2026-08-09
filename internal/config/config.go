package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port        string
	MetricsPort string
	RedisAddr   string
	RateLimit   int
	Window      time.Duration
	JWTSecret   string
	Algorithm   string
}

func Load() (Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	metricsPort := os.Getenv("METRICS_PORT")

	if metricsPort == "" {
		metricsPort = "9091"
	}
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	jwtSecret := os.Getenv("JWT_SECRET")

	if jwtSecret == "" {
		return Config{}, fmt.Errorf("JWT_SECRET environment variable is required")
	}

	rateLimit := 10

	rateLimitEnv := os.Getenv("RATE_LIMIT")
	if rateLimitEnv != "" {
		value, err := strconv.Atoi(rateLimitEnv)
		if err != nil {
			return Config{}, fmt.Errorf(
				"invalid RATE_LIMIT %q: must be a whole number",
				rateLimitEnv,
			)

		}

		if value <= 0 {
			return Config{}, fmt.Errorf(
				"invalid RATE_LIMIT %q: must be greater than 0",
				rateLimitEnv,
			)
		}
		rateLimit = value
	}

	windowEnv := os.Getenv("WINDOW")

	var window time.Duration

	if windowEnv == "" {
		window = 60 * time.Second
	} else {
		parsed, err := time.ParseDuration(windowEnv)
		if err != nil {
			return Config{}, fmt.Errorf(
				"invalid WINDOW %q: must be duration like 60s, 1m",
				windowEnv,
			)
		}
		window = parsed
	}

	algorithm := os.Getenv("ALGORITHM")
	if algorithm == "" {
		algorithm = "sliding_window"
	}
	if algorithm != "sliding_window" && algorithm != "token_bucket" {
		return Config{}, fmt.Errorf(
			"invalid ALGORITHM %q: must be sliding_window or token_bucket",
			algorithm,
		)
	}
	return Config{
		Port:        port,
		MetricsPort: metricsPort,
		RedisAddr:   redisAddr,
		RateLimit:   rateLimit,
		Window:      window,
		JWTSecret:   jwtSecret,
		Algorithm:   algorithm,
	}, nil
}
