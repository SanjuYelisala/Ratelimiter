# Distributed Rate Limiter

A production-grade distributed rate limiter built in Go, featuring two pluggable rate limiting algorithms, JWT-based per-client identity, atomic Redis operations via Lua scripting, and a full observability stack.

---

## Architecture

```
Clients → Nginx (load balancer) → RL Node 1 ┐
                                 → RL Node 2 ├→ Redis (shared state)
                                 → RL Node 3 ┘

Prometheus → scrapes RL Node 1, 2, 3 (:9091/metrics)
Grafana    → visualizes request rate + p99 latency
```

**Request flow:**
1. Client sends a request with a JWT in the `Authorization: Bearer <token>` header
2. Nginx load balances across 3 RL nodes (round-robin)
3. The RL node verifies the JWT signature (HS256) and extracts the `sub` claim as the client identity
4. An atomic Lua script runs against shared Redis to check the rate limit
5. If within limit → request is forwarded; if over → `429 Too Many Requests` with a `Retry-After` header is returned immediately

---

## Algorithms

Two algorithms are implemented and selectable via the `ALGORITHM` environment variable.

### Sliding Window Counter (`ALGORITHM=sliding_window`)

Each request is stored in a Redis sorted set with its Unix timestamp as the score. On every incoming request:

1. Remove all entries older than `now - window` via `ZREMRANGEBYSCORE`
2. Count remaining entries via `ZCARD`
3. If count ≥ limit → deny, return `0`
4. Otherwise add current request via `ZADD`, return `1`

**Why sorted set over a simple counter:**
A simple counter can't implement sliding window — it has no memory of *when* requests happened. The sorted set stores each request with its timestamp as the score, enabling "how many requests in the last N seconds" queries.

**The approximation tradeoff:**
Sliding window counter is an approximation, not exact. It assumes requests were evenly distributed across the previous window. The true sliding window log stores every request timestamp and is perfectly accurate but uses O(n) memory per client. Sliding window counter trades a small accuracy margin for O(1) memory — the right tradeoff at scale.

### Token Bucket (`ALGORITHM=token_bucket`)

Each client has a bucket with a maximum capacity equal to the rate limit. Tokens refill continuously at a fixed rate (`limit / window`). Each request consumes one token.

State stored per client in a Redis Hash:
- `tokens` — current token count
- `last_refill` — Unix timestamp of last request

On every incoming request:
1. Read `tokens` and `last_refill` from Redis
2. Calculate elapsed time since last refill
3. Add `elapsed * refill_rate` tokens, capped at capacity
4. If tokens < 1 → deny
5. Otherwise consume one token and persist updated state

---

## Algorithm Comparison

### Behavioral difference — proven experimentally

**Setup:** limit = 10 requests, window = 60 seconds

**Test:** Exhaust the limit, wait 30 seconds, send 5 more requests.

| Algorithm | After 30s wait | Result |
|---|---|---|
| Token bucket | 5 x 200 | Tokens refilled continuously at 1 token/6s |
| Sliding window counter | 5 x 429 | All 10 original requests still within the 60s window |

Sliding window counter blocked everything because the 60 second window hadn't expired — all 10 original requests were still counted. Token bucket allowed 5 because tokens refill independently of when original requests happened.

### When to use each

**Use sliding window counter when:**
- You need strict rate enforcement regardless of client behavior
- Protecting backend resources from any spike, including bursty legitimate traffic
- Memory efficiency is critical (O(1) per client)
- Boundary attack prevention matters — sliding window eliminates the fixed window exploit where a client can send 2× traffic at window edges

**Use token bucket when:**
- Clients legitimately need burst capacity (e.g. batch operations, mobile sync)
- You want to allow accumulated capacity to be spent at once
- Smoother traffic shaping is preferred over hard cutoffs

### Refill rate calculation

```
refill_rate = limit / window_seconds
```

Example: 10 requests per 60 seconds → 1 token refills every 6 seconds. After a 30 second wait, 5 tokens are available.

### Key tradeoffs

| Property | Sliding Window Counter | Token Bucket |
|---|---|---|
| Memory per client | O(1) | O(1) |
| Accuracy | Approximation | Exact |
| Burst handling | No — strict enforcement | Yes — up to bucket capacity |
| Boundary attacks | Prevented | Prevented |
| Implementation complexity | Moderate | Moderate |
| Redis data structure | Sorted set | Hash |

---

## Benchmark Results

**Setup:** 200 requests, 10 concurrent workers, localhost, limit = 10 req/60s.
Each run used a unique JWT subject to ensure a fresh Redis state with no carry-over from previous runs.
Tool: [hey](https://github.com/rakyll/hey)

### Sliding Window Counter

| Run | p50 | p99 | Req/sec |
|---|---|---|---|
| 1 | 0.7ms | 11.0ms | 7,209 |
| 2 | 0.7ms | 9.2ms | 8,439 |
| 3 | 0.7ms | 6.1ms | 10,338 |
| **Average** | **0.7ms** | **8.8ms** | **8,662** |

### Token Bucket

| Run | p50 | p99 | Req/sec |
|---|---|---|---|
| 1 | 1.0ms | 23.2ms | 4,878 |
| 2 | 0.7ms | 7.6ms | 8,990 |
| 3 | 0.7ms | 7.5ms | 8,601 |
| **Average** | **0.8ms** | **12.8ms** | **7,490** |

### Analysis

| Metric | Sliding Window Counter | Token Bucket | Winner |
|---|---|---|---|
| p50 latency | 0.7ms | 0.8ms | Sliding window |
| p99 latency | 8.8ms | 12.8ms | Sliding window |
| Throughput | 8,662 req/sec | 7,490 req/sec | Sliding window |

Sliding window counter outperforms token bucket at both p99 latency and throughput. The likely reason: sorted set operations (`ZREMRANGEBYSCORE` + `ZCARD` + `ZADD`) are simpler for Redis to execute than hash read-modify-write (`HGETALL` + `HSET`) under concurrent load, because hash operations require reading and deserializing multiple fields before writing back.

**Caveat:** These benchmarks run on localhost with no network latency. In a real deployment where Go nodes and Redis are on separate machines (1-2ms network latency), both algorithms would show higher absolute latency. The relative difference between them is expected to hold.

**Production recommendation:** Use sliding window counter as the default. Switch to token bucket only when clients have legitimate burst requirements (e.g. batch jobs, mobile sync operations that accumulate offline).

---

## Key Design Decisions

**Atomic Lua scripts over INCR + EXPIRE**

A naive `INCR` followed by `EXPIRE` has a race condition: if the process crashes between the two calls, the key lives in Redis forever with no TTL, permanently blocking that client. Both Lua scripts run atomically — Redis guarantees no other command executes between script lines.

**JWT subject as Redis key**

Rate limiting by IP address breaks behind proxies and NATs where many clients share a single IP. Using the JWT `sub` claim gives per-client limiting that survives network topology changes.

**Separate metrics port**

The `/metrics` endpoint runs on port `9091`, separate from the main application on port `8080`. If metrics shared the rate-limited port, Prometheus scraping would consume request budget and eventually get blocked — breaking observability precisely when you need it most under high load.

**Fail closed on Redis error**

If Redis is unavailable, the middleware returns `500 Internal Server Error`. Silent fail-open would mask infrastructure failures and allow abuse during outages.

**`service_healthy` condition in Docker Compose**

RL nodes wait for Redis to pass its healthcheck before starting, not just for the container to exist. This prevents startup failures when Redis takes a moment to initialize.

---

## Stack

| Component | Technology |
|---|---|
| Rate limiter nodes | Go (`net/http`, `go-redis/v9`) |
| Shared state | Redis 7 (sorted sets + hashes + Lua scripting) |
| Load balancer | Nginx |
| Auth | JWT HS256 (`golang-jwt/jwt/v5`) |
| Metrics | Prometheus + Grafana |
| Orchestration | Docker Compose |

---

## Project Structure

```
ratelimiter/
├── cmd/
│   ├── rlnode/
│   │   └── main.go          # Server entrypoint — wires config, Redis, middleware, servers
│   └── tokengen/
│       └── main.go          # CLI tool to generate signed JWTs for testing
├── internal/
│   ├── config/
│   │   └── config.go        # Environment-based config with validation
│   ├── handler/
│   │   ├── ping.go          # GET /ping handler
│   │   └── metrics.go       # GET /metrics handler (Prometheus)
│   ├── middleware/
│   │   ├── ratelimit.go     # Sliding window counter middleware + Lua script
│   │   └── tokenbucket.go   # Token bucket middleware + Lua script
│   └── redis/
│       └── client.go        # Redis client factory
├── nginx/
│   └── nginx.conf           # Load balancer config
├── prometheus/
│   └── prometheus.yml       # Scrape config for all 3 RL nodes
├── Dockerfile               # Multi-stage build (golang:1.23-alpine → alpine:3.22)
└── docker-compose.yaml      # Full stack: Redis, rl1/rl2/rl3, Nginx, Prometheus, Grafana
```

---

## Running Locally

**Prerequisites:** Docker, Docker Compose, Go 1.23+

**1. Set environment variables:**
```bash
cp .env.example .env
# Edit .env and set JWT_SECRET
```

**2. Start the full stack:**
```bash
docker compose up --build
```

**3. Generate a test token:**
```bash
TOKEN=$(JWT_SECRET=your-secret go run cmd/tokengen/main.go --subject client-a)
```

**4. Send a request:**
```bash
curl -H "Authorization: Bearer $TOKEN" http://localhost/ping
```

**5. Test rate limiting:**
```bash
for i in {1..12}; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    http://localhost/ping
done
# Expected: 10x 200, 2x 429
```

**6. Switch algorithms:**
```bash
# In docker-compose.yaml, set ALGORITHM=token_bucket on each RL node
# Or locally:
ALGORITHM=token_bucket JWT_SECRET=your-secret go run cmd/rlnode/main.go
```

**Services:**
| Service | URL |
|---|---|
| Rate limiter (via Nginx) | http://localhost |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 |

---

## Configuration

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | RL node application port |
| `METRICS_PORT` | `9091` | Prometheus metrics port |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `JWT_SECRET` | required | HMAC secret for JWT verification |
| `RATE_LIMIT` | `10` | Max requests per window |
| `WINDOW` | `60s` | Window size (e.g. `60s`, `1m`, `2m30s`) |
| `ALGORITHM` | `sliding_window` | Algorithm: `sliding_window` or `token_bucket` |

---

## Observability

Prometheus scrapes all 3 RL nodes every 15 seconds on `:9091/metrics`.

**Custom metrics:**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `ratelimiter_requests_total` | Counter | `client_id`, `result` | Total requests labeled allowed/denied |
| `ratelimiter_request_duration_seconds` | Histogram | `client_id` | Lua script execution latency |

**Grafana queries:**

Request rate per node:
```
rate(ratelimiter_requests_total[1m])
```

p99 latency per node:
```
histogram_quantile(0.99, rate(ratelimiter_request_duration_seconds_bucket[1m]))
```

**Observed latency:** p99 ~20ms through Docker networking (dominated by Redis round-trip).

---

## Chaos Testing

**Test 1: Node failure mid-traffic**

Killed `rl2` after ~10 requests while running 50 requests through Nginx:

```bash
# Terminal 1 — traffic
for i in {1..50}; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    http://localhost/ping
  sleep 0.5
done

# Terminal 2 — kill node after 5s
sleep 5 && docker stop ratelimiter-rl2-1
```

**Results:**
- 50/50 requests returned `200` — 0% error rate
- Nginx detected failure and rerouted to rl1 and rl3 within one request cycle
- Rate limit counts remained accurate on surviving nodes via shared Redis state

**Test 2: Node recovery**

```bash
docker start ratelimiter-rl2-1
```

- rl2 rejoined the cluster automatically
- Subsequent 50-request test: 50/50 `200` responses
- No manual intervention required

---

## What's Next

- [ ] Unit tests for middleware and Lua script logic
- [ ] AWS deployment (ECS + ElastiCache)
- [ ] Benchmark results under real network latency (cross-region nodes)
