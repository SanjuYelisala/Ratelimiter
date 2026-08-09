# Distributed Rate Limiter

A production-grade distributed rate limiter built in Go, featuring a sliding window counter algorithm, JWT-based per-client identity, atomic Redis operations via Lua scripting, and a full observability stack.

---

## Architecture

```
Clients → Nginx (load balancer) → RL Node 1 ┐
                                 → RL Node 2 ├→ Redis (shared state) → Raft KV Store
                                 → RL Node 3 ┘

Prometheus → scrapes RL Node 1, 2, 3 (:9091/metrics)
Grafana    → visualizes request rate + p99 latency
```

**Request flow:**
1. Client sends a request with a JWT in the `Authorization: Bearer <token>` header
2. Nginx load balances across 3 RL nodes (round-robin)
3. The RL node verifies the JWT signature (HS256) and extracts the `sub` claim as the client identity
4. An atomic Lua script runs against shared Redis to check the sliding window counter
5. If the client is within the limit → request is forwarded; if over → `429 Too Many Requests` with a `Retry-After` header is returned immediately

---

## Algorithm: Sliding Window Counter

The rate limiter uses a **sliding window counter** backed by a Redis sorted set.

Each request is stored as a member with its Unix timestamp as the score. On every incoming request:

1. Remove all entries older than `now - window` via `ZREMRANGEBYSCORE`
2. Count remaining entries via `ZCARD`
3. If count ≥ limit → deny and return `0`
4. Otherwise add the current request via `ZADD` and return `1`

The entire script runs atomically inside Redis — no race conditions between the increment and expiry steps.

**Why sliding window counter over alternatives:**

| Algorithm | Memory | Accuracy | Notes |
|---|---|---|---|
| Fixed window counter | O(1) | Low | Vulnerable to boundary attacks — 2× traffic possible at window edges |
| Sliding window counter | O(1) | High (approximation) | Prevents boundary attacks; trades perfect accuracy for O(1) memory |
| Window log | O(n) | Exact | Stores every request timestamp; memory grows with traffic |

Sliding window counter was chosen because it prevents boundary attacks with O(1) memory per client — the right tradeoff at scale.

---

## Key Design Decisions

**Atomic Lua script over INCR + EXPIRE**

A naive `INCR` followed by `EXPIRE` has a race condition: if the process crashes between the two calls, the key lives in Redis forever with no TTL, permanently blocking that client. The Lua script runs atomically — Redis guarantees no other command executes between its lines.

**JWT subject as Redis key**

Rate limiting by IP address breaks behind proxies and NATs where many clients share a single IP. Using the JWT `sub` claim gives per-client limiting that survives network topology changes.

**Separate metrics port**

The `/metrics` endpoint runs on port `9091`, separate from the main application on port `8080`. If metrics shared the rate-limited port, Prometheus scraping would consume request budget and eventually get blocked — breaking observability precisely when you need it most under high load.

**Fail open on Redis error**

If Redis is unavailable, the middleware returns `500 Internal Server Error` rather than silently allowing all traffic through. This is a deliberate choice: silent fail-open masks infrastructure failures and can allow abuse during outages.

---

## Stack

| Component | Technology |
|---|---|
| Rate limiter nodes | Go (`net/http`, `go-redis/v9`) |
| Shared state | Redis 7 (sorted sets + Lua scripting) |
| Load balancer | Nginx |
| Auth | JWT (HS256 via `golang-jwt/jwt/v5`) |
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
│   │   └── ratelimit.go     # Sliding window counter middleware + Lua script
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

**5. Test rate limiting (default limit: 10 requests per 60s):**
```bash
for i in {1..12}; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    http://localhost/ping
done
# Expected: 10x 200, 2x 429
```

**Services:**
| Service | URL |
|---|---|
| Rate limiter (via Nginx) | http://localhost |
| Prometheus | http://localhost:9090 |
| Grafana | http://localhost:3000 |

---

## Configuration

All configuration is environment-variable driven:

| Variable | Default | Description |
|---|---|---|
| `PORT` | `8080` | RL node application port |
| `METRICS_PORT` | `9091` | Prometheus metrics port |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `JWT_SECRET` | required | HMAC secret for JWT verification |
| `RATE_LIMIT` | `10` | Max requests per window |
| `WINDOW` | `60s` | Window size (e.g. `60s`, `1m`, `2m30s`) |

---

## Observability

Prometheus scrapes all 3 RL nodes every 15 seconds on `:9091/metrics`.

**Custom metrics:**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `ratelimiter_requests_total` | Counter | `client_id`, `result` | Total requests, labeled allowed/denied |
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

**Test: Node failure mid-traffic**

Killed `rl2` after ~10 requests while running 50 concurrent requests through Nginx:

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
- 50/50 requests returned `200` — **0% error rate**
- Nginx detected the failure and rerouted to rl1 and rl3 within one request cycle
- Rate limit counts remained accurate on surviving nodes (shared Redis state)

**Test: Node recovery**

Restarted rl2 with `docker start ratelimiter-rl2-1`:
- rl2 rejoined the cluster automatically
- Subsequent 50-request test: 50/50 `200` responses
- No manual intervention required beyond the start command

---

## What's Next

- [ ] Token bucket algorithm implementation
- [ ] Window log algorithm implementation  
- [ ] Benchmark all three algorithms at load (p99 latency, memory per client, accuracy)
- [ ] Unit tests for middleware and Lua script logic
- [ ] AWS deployment (ECS + ElastiCache)
