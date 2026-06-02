# Order Management System (OMS) - V1

A development-stage Order Management System backend built in Go. It provides
customer-owned order APIs, deterministic status transitions, PostgreSQL-backed
audit events, idempotent order creation, and an in-process database worker.

> **Security status:** OMS V1 is not production-ready. Trusted `X-OMS-*`
> identity headers are available only when explicit local development mode is
> enabled. Use verified Bearer JWT authentication outside local development.

## What Is Included

| Capability | Behavior |
| --- | --- |
| Create order | A customer can create an order with one or more items. The server calculates `total_cents`. |
| Retrieve order | A customer can retrieve an order by ID only when the order belongs to that customer. |
| List orders | A customer can list owned orders with optional status filtering and keyset pagination. |
| Cancel order | A customer can cancel an owned order only while it is `PENDING`. |
| Update status | An authenticated `ADMIN` or `SYSTEM` actor can apply a valid status transition. |
| Background processing | An in-process worker moves `PENDING` orders to `PROCESSING` every 5 minutes. |
| Audit trail | Every successful state change writes an `order_events` row with actor and request-IP attribution where available. |
| Idempotency | `customer_id + idempotency_key` prevents duplicate order creation. |

## Architecture

### Layered Request Lifecycle

```mermaid
graph TD
    Client["Client"] -->|"HTTP request"| API["API routing / chi router"]

    subgraph AuthBoundary["Authentication boundary"]
        API -->|"Authenticate and verify"| Auth["Auth middleware"]
        Auth -->|"Local only: trusted X-OMS headers"| DevMode["Dev identity context"]
        Auth -->|"Bearer JWT"| JWTMode["JWT identity context"]
    end

    JWTMode --> Handler["Orders HTTP handler"]
    DevMode --> Handler

    subgraph CoreDomain["Core domain"]
        Handler -->|"Strict JSON and payload validation"| Validation["Validation"]
        Validation -->|"Business rules and total_cents"| Service["Orders service"]
        Service -->|"Validate state transition"| StateMachine["Centralized state machine"]
    end

    subgraph DataAccess["Data access"]
        Service -->|"Transactional operation"| Repository["Repository"]
        Repository -->|"Generated sqlc queries"| Database[("PostgreSQL")]
        Repository -->|"Write audit event"| Audit[("order_events")]
    end
```

### State Machine

```mermaid
stateDiagram-v2
    [*] --> PENDING : Customer creates order
    PENDING --> CANCELLED : Customer cancels order
    PENDING --> PROCESSING : Worker or privileged actor
    PROCESSING --> SHIPPED : Privileged actor
    SHIPPED --> DELIVERED : Privileged actor
    CANCELLED --> [*]
    DELIVERED --> [*]
```

### Background Worker

```mermaid
graph LR
    Ticker["5-minute ticker"] --> Guard["In-process overlap guard"]
    Guard --> Query["Select PENDING batch"]
    Query -->|"FOR UPDATE SKIP LOCKED"| Update["Update to PROCESSING"]
    Update --> Event["Write SYSTEM audit event"]
    Event --> Done["Commit transaction"]
```

## Local Onboarding

### Prerequisites

- Docker with Docker Compose
- Go `1.26.3` for local tests and direct execution
- `curl`

### 1. Start PostgreSQL and the API

```bash
docker compose -f deploy/docker-compose.yml up --build -d
```

The local stack contains exactly two services:

- `postgres`: PostgreSQL 16 published at `127.0.0.1:5432`
- `api`: OMS API published at `127.0.0.1:8080`

Compose explicitly enables `OMS_AUTH_MODE=dev`. The trusted headers and fixed
PostgreSQL credentials are local-development conveniences, not LAN or
production defaults.

### 2. Apply the Database Migration

Migrations are intentionally manual. Apply the initial schema before using the
order endpoints:

```bash
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U postgres -d oms < db/migrations/000001_init_oms_schema.up.sql
```

### 3. Verify the API

```bash
curl -i http://127.0.0.1:8080/healthz
```

Expected response:

```text
HTTP/1.1 200 OK

ok
```

### 4. Set Shell Variables for the Examples

```bash
export OMS_BASE_URL='http://127.0.0.1:8080'
export CUSTOMER_ID='11111111-1111-1111-1111-111111111111'
export OTHER_CUSTOMER_ID='99999999-9999-9999-9999-999999999999'
```

After creating an order, set its returned ID:

```bash
export ORDER_ID='<returned-order-id>'
```

### 5. Stop the Local Stack

```bash
docker compose -f deploy/docker-compose.yml down
```

This preserves the PostgreSQL volume. To remove local database data
intentionally, run `docker compose -f deploy/docker-compose.yml down -v`.

## API Reference

All order endpoints require authentication. The health endpoint is public.

| Method | Path | Caller | Purpose |
| --- | --- | --- | --- |
| `GET` | `/healthz` | Public | Check that the API process is running. |
| `POST` | `/api/v1/orders` | `CUSTOMER` | Create an order with multiple items. |
| `GET` | `/api/v1/orders/{order_id}` | `CUSTOMER` | Retrieve an owned order. |
| `GET` | `/api/v1/orders` | `CUSTOMER` | List owned orders with optional filtering. |
| `POST` | `/api/v1/orders/{order_id}/cancel` | `CUSTOMER` | Cancel an owned `PENDING` order. |
| `PATCH` | `/api/v1/orders/{order_id}/status` | `ADMIN` or `SYSTEM` | Apply a valid status transition. |

### Local Development Authentication

In local Compose, send trusted development headers:

| Header | Required | Description |
| --- | --- | --- |
| `X-OMS-Role` | Yes | `CUSTOMER`, `ADMIN`, or `SYSTEM`. |
| `X-OMS-Customer-ID` | For `CUSTOMER` | Customer UUID used for ownership checks. |

Do not expose `OMS_AUTH_MODE=dev` outside local development. When auth mode is
empty, protected endpoints fail closed with `401`. In JWT mode, trusted
`X-OMS-*` headers are ignored.

### Error Envelope

Failures use a consistent JSON envelope:

```json
{
  "error": {
    "code": "conflict",
    "message": "idempotency conflict"
  }
}
```

Common status codes:

| Status | Meaning |
| --- | --- |
| `400` | Invalid JSON, unknown field, oversized body, invalid UUID, or invalid query parameter. |
| `401` | Missing or invalid authentication. |
| `403` | Authenticated caller does not have the required role. |
| `404` | Order does not exist or is not owned by the authenticated customer. |
| `409` | Invalid transition, invalid cancellation state, or idempotency conflict. |
| `500` | Unexpected internal error. |

## Customer Scenarios

### Create an Order with Multiple Items

`POST /api/v1/orders`

The customer identity comes from auth context. Do not send `customer_id` or
`total_cents` in the JSON body. The server calculates the total using integer
arithmetic and records a `CREATED` audit event.

```bash
curl -i -X POST "$OMS_BASE_URL/api/v1/orders" \
  -H 'Content-Type: application/json' \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID" \
  -d '{
    "idempotency_key": "checkout-2026-0001",
    "currency": "USD",
    "items": [
      {
        "product_id": "22222222-2222-2222-2222-222222222222",
        "sku": "SKU-COFFEE-001",
        "quantity": 2,
        "unit_price_cents": 1250
      },
      {
        "product_id": "33333333-3333-3333-3333-333333333333",
        "sku": "SKU-MUG-001",
        "quantity": 1,
        "unit_price_cents": 900
      }
    ]
  }'
```

Expected result: `201 Created`. The returned order starts in `PENDING` with
`total_cents: 3400`.

Important create scenarios:

| Scenario | Result |
| --- | --- |
| One or more valid items | `201 Created` |
| Same customer and same `idempotency_key` submitted again | `409 Conflict` with sanitized `idempotency conflict` message |
| Missing items, non-positive quantity, negative price, or invalid currency | `400 Bad Request` |
| Client supplies `customer_id`, `total_cents`, or another unknown field | `400 Bad Request` |

### Retrieve Owned Order Details

`GET /api/v1/orders/{order_id}`

```bash
curl -i "$OMS_BASE_URL/api/v1/orders/$ORDER_ID" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID"
```

Expected result: `200 OK` with order status, currency, server-computed total,
timestamps, and all order items.

Ownership behavior:

```bash
curl -i "$OMS_BASE_URL/api/v1/orders/$ORDER_ID" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $OTHER_CUSTOMER_ID"
```

Expected result: `404 Not Found`. Customer-facing reads do not reveal whether
another customer owns an order.

### List Owned Orders

`GET /api/v1/orders`

List the authenticated customer's orders:

```bash
curl -i "$OMS_BASE_URL/api/v1/orders?limit=50" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID"
```

Filter by status:

```bash
curl -i "$OMS_BASE_URL/api/v1/orders?status=PENDING&limit=50" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID"
```

Pagination uses an opaque keyset cursor. When a response contains a non-null
`next_cursor`, request the next page:

```bash
export NEXT_CURSOR='<next_cursor-from-response>'

curl -i "$OMS_BASE_URL/api/v1/orders?limit=50&cursor=$NEXT_CURSOR" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID"
```

List scenarios:

| Scenario | Result |
| --- | --- |
| No `status` filter | Returns owned orders across all statuses |
| Valid filter: `PENDING`, `PROCESSING`, `SHIPPED`, `DELIVERED`, or `CANCELLED` | Returns matching owned orders |
| Invalid status, malformed cursor, or `limit` outside `1..100` | `400 Bad Request` |

### Cancel a Pending Order

`POST /api/v1/orders/{order_id}/cancel`

```bash
curl -i -X POST "$OMS_BASE_URL/api/v1/orders/$ORDER_ID/cancel" \
  -H 'X-OMS-Role: CUSTOMER' \
  -H "X-OMS-Customer-ID: $CUSTOMER_ID"
```

Expected result for a `PENDING` order: `200 OK` with status `CANCELLED`. The
state change and customer attribution are written atomically with the audit
event.

Cancellation scenarios:

| Scenario | Result |
| --- | --- |
| Owned order is `PENDING` | `200 OK`, status becomes `CANCELLED` |
| Owned order is `PROCESSING`, `SHIPPED`, `DELIVERED`, or already `CANCELLED` | `409 Conflict` |
| Order belongs to another customer or does not exist | `404 Not Found` |

## Privileged Status Updates

`PATCH /api/v1/orders/{order_id}/status`

Only `ADMIN` or `SYSTEM` may call this route. Customers receive `403 Forbidden`.

Valid transitions:

| Current status | Allowed next status |
| --- | --- |
| `PENDING` | `PROCESSING` or `CANCELLED` |
| `PROCESSING` | `SHIPPED` |
| `SHIPPED` | `DELIVERED` |
| `DELIVERED` | None |
| `CANCELLED` | None |

Move an order from `PROCESSING` to `SHIPPED` in local development:

```bash
curl -i -X PATCH "$OMS_BASE_URL/api/v1/orders/$ORDER_ID/status" \
  -H 'Content-Type: application/json' \
  -H 'X-OMS-Role: ADMIN' \
  -d '{"status":"SHIPPED"}'
```

Expected result for a valid transition: `200 OK`.

Status-update scenarios:

| Scenario | Result |
| --- | --- |
| `ADMIN` or `SYSTEM` requests a valid transition | `200 OK` and a `STATUS_UPDATED` audit event |
| `CUSTOMER` requests any privileged status update | `403 Forbidden` |
| Privileged actor requests an invalid transition | `409 Conflict` |
| Unknown status value or unknown JSON field | `400 Bad Request` |

## Automatic Pending-to-Processing Worker

The API process runs one in-process database-backed worker:

- It starts with the API process.
- It waits for the first 5-minute tick; it does not process immediately at startup.
- It moves a configurable batch of `PENDING` orders to `PROCESSING`.
- `WORKER_BATCH_SIZE` defaults to `500`.
- It prevents overlapping runs in the same process.
- It uses `FOR UPDATE SKIP LOCKED` for safe concurrent instances.
- It writes a `WORKER_MOVED_TO_PROCESSING` audit event with actor type `SYSTEM`.
- It stops cleanly when the API receives a shutdown signal.

Worker scenario:

1. Create an order and observe the returned `PENDING` status.
2. Leave the API running.
3. Wait for the next 5-minute worker tick.
4. Retrieve the order again.
5. Observe that the order is now `PROCESSING`.
6. Attempt cancellation and observe `409 Conflict`, because cancellation is
   allowed only while the order remains `PENDING`.

## JWT Authentication Configuration

Use `OMS_AUTH_MODE=jwt` outside local development. JWT mode accepts only
`Authorization: Bearer <token>` and ignores trusted development headers.

| Environment Variable | Required | Description |
| --- | --- | --- |
| `OMS_AUTH_MODE` | Yes | Set to `jwt`. Empty mode fails closed; `dev` is local-only. |
| `OMS_JWT_ISSUER` | Yes | Expected HTTPS token issuer. |
| `OMS_JWT_AUDIENCE` | Yes | Expected token audience. |
| `OMS_JWT_ALLOWED_ALGS` | Yes | Comma-separated asymmetric signing algorithms, such as `RS256,ES256`. `none` and `HS*` are rejected. |
| `OMS_JWT_JWKS_URL` | Exactly one source | HTTPS JWKS endpoint. Set this or `OMS_OIDC_DISCOVERY_URL`, not both. |
| `OMS_OIDC_DISCOVERY_URL` | Exactly one source | HTTPS OIDC discovery endpoint. Set this or `OMS_JWT_JWKS_URL`, not both. |
| `OMS_JWT_ROLE_CLAIM` | Yes | Claim containing one OMS role value. |
| `OMS_JWT_ROLE_CUSTOMER_VALUE` | Yes | Provider claim value mapped to `CUSTOMER`. |
| `OMS_JWT_ROLE_ADMIN_VALUE` | Yes | Provider claim value mapped to `ADMIN`. |
| `OMS_JWT_ROLE_SYSTEM_VALUE` | Yes | Provider claim value mapped to `SYSTEM`. |
| `OMS_JWT_CUSTOMER_ID_CLAIM` | Yes | UUID-valued customer claim used for ownership checks. |
| `OMS_JWT_SUBJECT_CLAIM` | No | Stable subject claim. Defaults to `sub`. Privileged subjects must be UUID strings for audit attribution. |
| `OMS_JWT_HTTP_TIMEOUT` | No | Discovery and JWKS HTTP timeout. Defaults to `5s`. |

JWT validation verifies signature, issuer, audience, expiry, allowed algorithm,
role mapping, customer UUID, and privileged subject UUID. Discovery and JWKS
responses are bounded, and invalid authentication returns a generic `401`.

## Runtime Configuration

| Environment Variable | Default | Description |
| --- | --- | --- |
| `DATABASE_URL` | Required | PostgreSQL connection string. Startup fails before serving when PostgreSQL is unreachable. |
| `HTTP_ADDR` | `:8080` | HTTP listen address. |
| `WORKER_BATCH_SIZE` | `500` | Maximum orders processed by one worker batch. |
| `SHUTDOWN_TIMEOUT` | `10s` | Graceful shutdown timeout. |
| `HTTP_READ_TIMEOUT` | `5s` | Request read timeout. |
| `HTTP_WRITE_TIMEOUT` | `10s` | Response write timeout. |
| `HTTP_IDLE_TIMEOUT` | `60s` | Idle keep-alive timeout. |

Run the API directly against an existing PostgreSQL instance:

```bash
export DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/oms?sslmode=disable'
export OMS_AUTH_MODE='dev'

go run ./cmd/api
```

## Development Commands

Run unit tests and static checks:

```bash
go test ./...
go vet ./...
go mod verify
```

Run PostgreSQL integration tests:

```bash
OMS_RUN_INTEGRATION=1 \
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/oms?sslmode=disable' \
go test ./internal/orders -run Integration -count=1
```

Regenerate sqlc output with the pinned image:

```bash
docker run --rm -v "$PWD:/src" -w /src \
  sqlc/sqlc:1.31.1@sha256:70f53171d27b2424e9358869975455a6e955a5aa8e58a998a270a6e34e525537 \
  generate
```

Run the vulnerability scan:

```bash
go run golang.org/x/vuln/cmd/govulncheck@v1.3.0 ./...
```

## Scope Boundaries

This repository is intentionally limited to OMS V1. It does not include Redis,
asynq, Kafka, RabbitMQ, GraphQL, payments, inventory reservation, shipment
integration, email workflows, a product catalog, carts, a frontend, analytics,
microservices, an ORM, plugins, or dynamic rule engines.
