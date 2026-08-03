package middleware

import (
	"errors"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/prometheus/client_golang/prometheus"
	redisClient "github.com/redis/go-redis/v9"
)

var rateLimitScript = redisClient.NewScript(`
	local key = KEYS[1]
	local now = tonumber(ARGV[1])
	local window = tonumber(ARGV[2])
	local limit = tonumber(ARGV[3])
	local member = ARGV[4]

	local windowStart = now - window

	redis.call("ZREMRANGEBYSCORE", key, "-inf", windowStart)

	local count = redis.call("ZCARD", key)

	if count >= limit then
		return 0
	end

	redis.call("ZADD", key, now, member)
	redis.call("EXPIRE", key, window)

	return 1
`)

// Extracts the client ID ("sub" claim) from the JWT.
func extractClientID(request *http.Request, secret string) (string, error) {

	// 1. Read the Authorization header.
	authHeader := request.Header.Get("Authorization")
	if authHeader == "" {
		return "", errors.New("missing Authorization header")
	}

	// 2. Verify the Authorization header format:
	//    Authorization: Bearer <JWT>
	parts := strings.Split(authHeader, " ")
	if len(parts) != 2 || parts[0] != "Bearer" {
		return "", errors.New("invalid Authorization header")
	}

	// Extract the JWT.
	tokenString := parts[1]

	// 3. Parse and verify the JWT.
	token, err := jwt.Parse(tokenString, func(token *jwt.Token) (any, error) {

		// Ensure the JWT was signed using an HMAC algorithm (HS256).
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("unexpected signing method")
		}

		// Return the secret used to verify the signature.
		return []byte(secret), nil
	})

	if err != nil {
		return "", err
	}

	if !token.Valid {
		return "", errors.New("invalid token")
	}

	// 4. Read the verified claims.
	claims, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return "", errors.New("invalid claims")
	}

	// 5. Extract the subject ("sub") claim.
	sub, ok := claims["sub"].(string)
	if !ok {
		return "", errors.New("missing sub claim")
	}

	return sub, nil
}

func RateLimit(
	client *redisClient.Client,
	limit int,
	window time.Duration,
	secret string,
) func(http.Handler) http.Handler {

	return func(next http.Handler) http.Handler {

		return http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {

			ctx := request.Context()

			// Authenticate the request and extract the client ID.
			clientID, err := extractClientID(request, secret)
			if err != nil {
				http.Error(response, "Unauthorized", http.StatusUnauthorized)
				return
			}

			// Use the JWT subject as the Redis key.
			redisKey := "rl:" + clientID

			now := time.Now()

			member := strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.Itoa(rand.Intn(1000000))

			start := time.Now()
			result, err := rateLimitScript.Run(
				ctx,
				client,
				[]string{redisKey},
				now.Unix(),
				int(window.Seconds()),
				limit,
				member,
			).Int()
			duration := time.Since(start)

			if err != nil {
				http.Error(response, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			// Only observe duration and increment counter on successful Redis calls
			requestDuration.WithLabelValues(clientID).Observe(duration.Seconds())
			if result == 0 {

				requestsTotal.
					WithLabelValues(clientID, "denied").
					Inc()
				response.Header().Set("Retry-After", strconv.Itoa(int(window.Seconds())))
				http.Error(response, "Too Many Requests", http.StatusTooManyRequests)
				return
			}

			requestsTotal.
				WithLabelValues(clientID, "allowed").
				Inc()
			next.ServeHTTP(response, request)
		})
	}
}

var requestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "ratelimiter_requests_total",
		Help: "Total requests processed by the rate limiter",
	},
	[]string{"client_id", "result"},
)

var requestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Name:    "ratelimiter_request_duration_seconds",
		Help:    "Time spent processing rate limit requests",
		Buckets: []float64{.0001, .0005, .001, .005, .01, .025, .05, .1},
	},
	[]string{"client_id"},
)

func init() {
	prometheus.MustRegister(requestsTotal)
	prometheus.MustRegister(requestDuration)
}
