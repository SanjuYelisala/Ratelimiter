package middleware

import (
	"net/http"
	"strconv"
	"time"

	redisClient "github.com/redis/go-redis/v9"
)

var tokenBucketScript = redisClient.NewScript(`
	local key = KEYS[1]
	local now = tonumber(ARGV[1])
	local capacity = tonumber(ARGV[2])
	local refill_rate = tonumber(ARGV[3])
	local ttl = tonumber(ARGV[4])

	-- Read current state
	local data = redis.call("HGETALL", key)
	local tokens = capacity
	local last_refill = now

	-- If key exists, parse stored values
	if #data > 0 then
		for i = 1, #data, 2 do
			if data[i] == "tokens" then
				tokens = tonumber(data[i+1])
			elseif data[i] == "last_refill" then
				last_refill = tonumber(data[i+1])
			end
		end

		-- Calculate tokens to add since last refill
		local elapsed = (now - last_refill) / 1000.0
		local new_tokens = elapsed * refill_rate
		tokens = math.min(capacity, tokens + new_tokens)
	end

	-- Deny if no tokens available
	if tokens < 1 then
		return 0
	end

	-- Consume one token and persist state
	tokens = tokens - 1
	redis.call("HSET", key, "tokens", tokens, "last_refill", now)
	redis.call("EXPIRE", key, ttl)

	return 1
`)

func TokenBucket(client *redisClient.Client, limit int, window time.Duration, secret string) func(http.Handler) http.Handler {
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
			redisKey := "tb:" + clientID

			//refill rate
			refillRate := float64(limit) / window.Seconds()
			now := time.Now()

			start := time.Now()
			result, err := tokenBucketScript.Run(
				ctx,
				client,
				[]string{redisKey},
				now.UnixMilli(),
				limit,
				refillRate,
				(int(window.Seconds()))*2,
			).Int()
			duration := time.Since(start)

			if err != nil {
				http.Error(response, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			// Only observe duration and increment counter on successful Redis calls
			requestDuration.WithLabelValues(clientID).Observe(duration.Seconds())
			if result == 0 {

				requestsTotal.WithLabelValues(clientID, "denied").Inc()
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
