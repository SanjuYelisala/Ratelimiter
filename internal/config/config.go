package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Port      string
	RedisAddr string
	RateLimit int
	Window    time.Duration
	JWTSecret string
}

func Load() (Config, error) {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
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

	return Config{
		Port:      port,
		RedisAddr: redisAddr,
		RateLimit: rateLimit,
		Window:    window,
		JWTSecret: jwtSecret,
	}, nil
}
