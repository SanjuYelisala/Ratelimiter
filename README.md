# Distributed Rate Limiter

A production-grade distributed rate limiter built in Go — benchmarked on AWS, chaos tested, and fully observable. Features two pluggable algorithms, JWT-based per-client identity, atomic Redis operations via Lua scripting, and a complete observability stack.

---

## Architecture

```
                    ┌─────────────────────────────────────────┐
                    │              AWS / Local                 │
                    │                                         │
  Clients ──────▶  ALB / Nginx  ──────▶  RL Node 1  ─┐      │
                    │                    RL Node 2  ──┼──▶  Redis
                    │                    RL Node 3  ─┘      │
                    │                                         │
                    │  Prometheus ◀── /metrics (port 9091)   │
                    │  Grafana    ◀── Prometheus              │
                    └─────────────────────────────────────────┘
```

**Request flow:**
1. Client sends a JWT in the `Authorization: Bearer <token>` header
2. ALB (AWS) or Nginx (local) load balances across 3 RL nodes (round-robin)
3. The RL node verifies the JWT signature (HS256) and extracts the `sub` claim as client identity
4. An atomic Lua script runs against shared Redis to check the rate limit
5. Within limit → request forwarded; over limit → `429 Too Many Requests` with `Retry-After` header

---

## Algorithms

Select via the `ALGORITHM` environment variable.

### Sliding Window Counter (`ALGORITHM=sliding_window`)

Stores each request in a Redis sorted set with its Unix timestamp as the score.

**On every request:**
1. `ZREMRANGEBYSCORE` — remove entries older than `now - window`
2. `ZCARD` — count remaining entries
3. If count ≥ limit → deny
4. Otherwise `ZADD` current request → allow

**The approximation tradeoff:** Sliding window counter is an approximation, not exact. It assumes requests were evenly distributed across the previous window. The true sliding window *log* stores every timestamp with perfect accuracy, but uses O(n) memory per client. The counter variant uses O(1) — the right tradeoff at scale.

### Token Bucket (`ALGORITHM=token_bucket`)

Each client has a bucket at capacity = limit. Tokens refill continuously at `limit / window` per second. Each request consumes one token.

**State stored per client in a Redis Hash:**
- `tokens` — current token count
- `last_refill` — Unix timestamp of last request

**On every request:**
1. `HGETALL` — read current state
2. Calculate tokens added since last refill: `elapsed × refill_rate`
3. Cap at capacity
4. If tokens < 1 → deny
5. Otherwise consume one token, `HSET` updated state → allow

---

## Algorithm Comparison

### Behavioral difference — proven experimentally

**Setup:** limit = 10, window = 60s  
**Test:** Exhaust the limit, wait 30 seconds, send 5 more requests.

| Algorithm | After 30s wait | Explanation |
|---|---|---|
| Token bucket | ✅ 5 × 200 | Tokens refilled at 1/6s — 5 tokens accumulated |
| Sliding window counter | ❌ 5 × 429 | All 10 original requests still within the 60s window |

Sliding window enforces strictly — no memory of gaps in traffic. Token bucket rewards clients who pace themselves by letting tokens accumulate.

### When to use each

**Sliding window counter** — strict enforcement, boundary attack prevention, O(1) memory. Best for protecting backend resources from any spike.

**Token bucket** — allows burst capacity, rewards well-behaved clients. Best for APIs where clients legitimately accumulate and spend capacity (batch jobs, mobile sync).

### Performance tradeoffs

| Property | Sliding Window Counter | Token Bucket |
|---|---|---|
| Memory per client | O(1) | O(1) |
| Accuracy | Approximation | Exact |
| Burst handling | ❌ Strict enforcement | ✅ Up to bucket capacity |
| Boundary attack prevention | ✅ | ✅ |
| Redis data structure | Sorted set | Hash |
| Redis operations per request | 3 (ZREMRANGE + ZCARD + ZADD) | 2 (HGETALL + HSET) |

---

## Benchmark Results

### Local (Docker Compose, localhost)

**Setup:** 200 requests, 10 concurrent workers, unique client per run.  
Tool: [hey](https://github.com/rakyll/hey)

| Algorithm | p50 | p99 | Req/sec |
|---|---|---|---|
| Sliding window counter | 0.7ms | 8.8ms | 8,662 |
| Token bucket | 0.8ms | 12.8ms | 7,490 |

### AWS Production (ECS Fargate + ElastiCache, us-east-1)

**Setup:** 3 Fargate tasks (0.25 vCPU / 512MB each), ElastiCache Redis t3.micro, ALB.  
Client location: Tallahassee, FL → us-east-1 (N. Virginia).

**10,000 requests, 50 concurrent:**

| Algorithm | p50 | p99 | Req/sec |
|---|---|---|---|
| Sliding window counter | 61ms | 292ms | 536 |
| Token bucket | 74ms | 509ms | 419 |

**25,000 requests, 100 concurrent:**

| Algorithm | p50 | p99 | Req/sec |
|---|---|---|---|
| Sliding window counter | 68ms | 408ms | 926 |
| Token bucket | 110ms | 931ms | 546 |

### Analysis

Sliding window counter outperforms token bucket consistently — **40% higher throughput and 55% lower p99** at 25K requests on AWS. The gap widens under higher concurrency.

**Why:** Sorted set operations (`ZREMRANGEBYSCORE` + `ZCARD` + `ZADD`) are cheaper for Redis under concurrent load than hash read-modify-write (`HGETALL` + `HSET`), which requires deserializing multiple fields before writing back.

**Localhost vs AWS delta:** p50 jumps from 0.7ms to 61ms — this is real network latency (client → ALB → Fargate → ElastiCache → back). The relative difference between algorithms holds in both environments.

**Production recommendation:** Use sliding window counter as the default. Switch to token bucket only when clients have legitimate burst requirements.

---

## Key Design Decisions

**Atomic Lua scripts over INCR + EXPIRE**

A naive `INCR` followed by `EXPIRE` has a race condition — if the process crashes between the two calls, the key lives in Redis forever with no TTL, permanently blocking that client. Both Lua scripts run atomically: Redis guarantees no other command executes between script lines.

**JWT subject as Redis key**

Rate limiting by IP breaks behind proxies and NATs where many clients share one IP. Using the JWT `sub` claim gives true per-client limiting that survives network topology changes.

**Separate metrics port**

`/metrics` runs on port `9091`, separate from the app on port `8080`. If metrics shared the rate-limited port, Prometheus scraping would consume request budget and get blocked under high load — breaking observability exactly when you need it most.

**Dedicated `/health` endpoint**

The ALB health check hits `/health`, which bypasses JWT verification and rate limiting entirely. Without this, the health check returns `401` and the ALB never marks tasks as healthy.

**Fail closed on Redis error**

If Redis is unavailable, the middleware returns `500 Internal Server Error`. Silent fail-open masks infrastructure failures and allows abuse during outages.

**`service_healthy` in Docker Compose**

RL nodes wait for Redis to pass its healthcheck before starting — not just for the container to exist. Prevents startup failures when Redis takes a moment to initialize.

---

## Stack

| Component | Local | AWS |
|---|---|---|
| Rate limiter nodes | Go (`net/http`) | ECS Fargate (3 tasks) |
| Load balancer | Nginx | Application Load Balancer |
| Shared state | Redis 7 (Docker) | ElastiCache Redis t3.micro |
| Auth | JWT HS256 (`golang-jwt/jwt/v5`) | Same |
| Metrics | Prometheus + Grafana | Same (port 9091) |
| Orchestration | Docker Compose | AWS CLI / ECS |

---

## Project Structure

```
ratelimiter/
├── cmd/
│   ├── rlnode/
│   │   └── main.go          # Entrypoint — config, Redis, middleware, dual servers
│   └── tokengen/
│       └── main.go          # CLI tool to generate signed JWTs for testing
├── internal/
│   ├── config/
│   │   └── config.go        # Environment-based config with validation
│   ├── handler/
│   │   ├── ping.go          # GET /ping — rate limited, JWT required
│   │   ├── health.go        # GET /health — no auth, for ALB health checks
│   │   └── metrics.go       # GET /metrics — Prometheus, separate port
│   ├── middleware/
│   │   ├── ratelimit.go     # Sliding window counter + Lua script
│   │   └── tokenbucket.go   # Token bucket + Lua script
│   └── redis/
│       └── client.go        # Redis client factory
├── nginx/
│   └── nginx.conf           # Load balancer config (local)
├── prometheus/
│   └── prometheus.yml       # Scrape config for 3 RL nodes
├── Dockerfile               # Multi-stage build (golang:1.26-alpine → alpine:3.22)
└── docker-compose.yaml      # Full local stack
```

---

## Running Locally

**Prerequisites:** Docker, Docker Compose, Go 1.26+

```bash
# 1. Configure environment
cp .env.example .env
# Set JWT_SECRET in .env

# 2. Start the full stack
docker compose up --build

# 3. Generate a test token
TOKEN=$(JWT_SECRET=your-secret go run cmd/tokengen/main.go --subject client-a)

# 4. Send a request
curl -H "Authorization: Bearer $TOKEN" http://localhost/ping

# 5. Test rate limiting
for i in {1..12}; do
  curl -s -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    http://localhost/ping
done
# Expected: 10× 200, 2× 429

# 6. Switch algorithms
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
| `PORT` | `8080` | Application port |
| `METRICS_PORT` | `9091` | Prometheus metrics port |
| `REDIS_ADDR` | `localhost:6379` | Redis address |
| `JWT_SECRET` | **required** | HMAC secret for JWT verification |
| `RATE_LIMIT` | `10` | Max requests per window |
| `WINDOW` | `60s` | Window duration (e.g. `60s`, `1m`, `2m30s`) |
| `ALGORITHM` | `sliding_window` | `sliding_window` or `token_bucket` |

---

## Observability

Prometheus scrapes all 3 RL nodes every 15 seconds on `:9091/metrics`.

**Custom metrics:**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `ratelimiter_requests_total` | Counter | `client_id`, `result` | Requests labeled `allowed` / `denied` |
| `ratelimiter_request_duration_seconds` | Histogram | `client_id` | Lua script execution latency |

**Grafana queries:**

```promql
# Request rate per node
rate(ratelimiter_requests_total[1m])

# p99 latency per node
histogram_quantile(0.99, rate(ratelimiter_request_duration_seconds_bucket[1m]))
```

---

## Chaos Testing

### Local (Docker Compose)

**Test:** Killed `rl2` mid-traffic, 50 requests with 0.5s interval.

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

**Result:** 50/50 `200` — 0% error rate. Nginx rerouted within one request cycle.  
**Recovery:** `docker start ratelimiter-rl2-1` — rejoined automatically, subsequent 50/50 `200`.

### AWS (ECS Fargate)

**Test:** Stopped an ECS task mid-traffic via `aws ecs stop-task`.

```bash
# Terminal 1 — traffic (1s interval)
for i in {1..50}; do
  STATUS=$(curl -s -o /dev/null -w "%{http_code}" \
    -H "Authorization: Bearer $TOKEN" \
    http://<ALB-DNS>/ping)
  echo "Request $i: $STATUS"
  sleep 1
done

# Terminal 2 — stop task after 10s
TASK=$(aws ecs list-tasks --cluster ratelimiter-cluster \
  --query 'taskArns[0]' --output text)
sleep 10 && aws ecs stop-task --cluster ratelimiter-cluster --task $TASK
```

**Result:** 50/50 `200` — 0% error rate.  
**Recovery:** ECS automatically replaced the stopped task. No manual intervention. Service returned to `Running: 3` within 2 minutes.

**AWS vs local distinction:** Docker Compose requires a manual `docker start`. ECS automatically replaces failed tasks — a meaningful difference in operational behavior.

---

## What's Next

- [ ] Unit tests for middleware and Lua script logic
- [ ] AWS CDK — translate manual deployment into reproducible infrastructure as code
- [ ] Multi-region deployment with latency comparison (us-east-1 vs ap-south-1)
- [ ] Redis Sentinel or Cluster for high availability (current single-node Redis is a SPOF)
