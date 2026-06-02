# Order Management System (OMS) - V1

A development-stage Order Management System (OMS) backend built in Go. It uses a layered architecture, a deterministic order state machine, a transactional repository, and an in-process database-backed worker.

> **Security status:** OMS V1 is not production-ready. Remaining security, release-readiness, and local-environment hardening tasks are tracked in [`docs/BLOCKERS.md`](docs/BLOCKERS.md).

---

## 🎨 Visual System Architecture

### 1. Layered Request Lifecycle
```mermaid
graph TD
    Client[Client / Frontend] -->|HTTP Request| API[API Routing / Chi Router]
    
    subgraph Auth Boundary
        API -->|1. Authenticate & Verify| Auth[Auth Middleware]
        Auth -->|Dev: Trusted X-OMS Headers| DevMode[Dev Identity Context]
        Auth -->|Prod: Asymmetric Bearer JWT| JWTMode[JWT Identity Context]
    end
    
    JWTMode -->|2. Route Handler| Handler[Orders HTTP Handler]
    DevMode -->|2. Route Handler| Handler
    
    subgraph Core Domain
        Handler -->|3. Validate Payload| Val[Strict Payload Validation]
        Val -->|4. Business Rules & integer total_cents| Service[Orders Service]
        Service -->|5. Validate State Transition| SM[Centralized State Machine]
    end
    
    subgraph Data Access Layer
        Service -->|6. Transactional Write withTx| Repo[Transactional Repository]
        Repo -->|7. SQL Queries sqlc| DB[(PostgreSQL Database)]
    end
```

### 2. State Machine Transitions
```mermaid
stateDiagram-v2
    [*] --> PENDING : Create Order (1. CUSTOMER)
    
    PENDING --> CANCELLED : Cancel Order (2. CUSTOMER)
    PENDING --> PROCESSING : Batch Process (3. Worker / ADMIN)
    
    PROCESSING --> SHIPPED : Update Status (4. ADMIN / SYSTEM)
    SHIPPED --> DELIVERED : Update Status (5. ADMIN)
    
    CANCELLED --> [*]
    DELIVERED --> [*]
```

### 3. Background DB-Backed Worker Flow
```mermaid
graph LR
    Ticker[5-Min Ticker] -->|Trigger Run| Lock[Acquire Batch Lock]
    Lock -->|FOR UPDATE SKIP LOCKED| Query[Fetch PENDING Orders]
    Query -->|Bulk Process| Update[Transition Status to PROCESSING]
    Update -->|Audit Event| Event[Log SYSTEM Event]
    Event -->|Release| Done[Batch Complete]
```

---

## 🚀 Quick Start & Onboarding

Run the entire system in seconds using Docker. Follow these three simple steps to go from onboarding to a live, functional order API.

### Step 1: Start PostgreSQL and the OMS API
Run the multi-stage build and spin up the database container:
```bash
docker compose -f deploy/docker-compose.yml up --build -d
```

The local Compose stack publishes the API and PostgreSQL only on `127.0.0.1`. Its trusted dev-header auth mode and fixed PostgreSQL credentials are local-development conveniences, not LAN or production defaults.

### Step 2: Apply the Database Schema Migrations
Apply the initial table schemas, unique constraints, and optimized indexes to the running container:
```bash
docker compose -f deploy/docker-compose.yml exec -T postgres \
  psql -U postgres -d oms < db/migrations/000001_init_oms_schema.up.sql
```

### Step 3: Verify the Healthcheck
Verify that the service is running and listening on port `8080`:
```bash
curl -i http://localhost:8080/healthz
```

---

## 🛠️ Verification & Test Suite

Verify compliance, unit test suite, and SQL drift verification locally:

### Run Unit Tests
```bash
go test ./...
go vet ./...
```

### Run Postgres Integration Tests
```bash
OMS_RUN_INTEGRATION=1 \
DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/oms?sslmode=disable' \
go test -v ./internal/orders -run Integration
```

---

## 🔒 Configuration & Authentication Reference

The API configures itself through the following environment variables:

| Environment Variable | Default Value | Description |
| :--- | :--- | :--- |
| `DATABASE_URL` | *Required* | PostgreSQL connection string. |
| `OMS_AUTH_MODE` | `""` (Locked) | Authentication mode: `""` (Fail-closed), `dev` (Local headers), `jwt` (Bearer tokens). |
| `OMS_JWT_HTTP_TIMEOUT` | `5s` | JWT-mode timeout for OIDC discovery and JWKS fetches, including key refresh. Invalid, zero, or negative values fail startup. |
| `WORKER_BATCH_SIZE` | `500` | Max number of PENDING orders the background worker processes per iteration. |
| `HTTP_READ_TIMEOUT` | `5s` | Read deadline for slow-client protection. |
| `HTTP_WRITE_TIMEOUT` | `10s` | Write deadline for server response limits. |
| `HTTP_IDLE_TIMEOUT` | `60s` | Idle keep-alive connection lifespan. |

### Development Mode (`OMS_AUTH_MODE=dev`)
Protected endpoints accept trusted identity variables passed as headers:
*   `X-OMS-Role`: Role value (`CUSTOMER`, `ADMIN`, `SYSTEM`).
*   `X-OMS-Customer-ID`: UUID associated with the customer (required for `CUSTOMER`).

*Example Create Order request:*
```bash
curl -i -X POST \
  -H "Content-Type: application/json" \
  -H "X-OMS-Role: CUSTOMER" \
  -H "X-OMS-Customer-ID: 11111111-1111-1111-1111-111111111111" \
  -d '{
    "idempotency_key": "order-idem-1",
    "currency": "USD",
    "items": [
      {"product_id": "22222222-2222-2222-2222-222222222222", "sku": "SKU-PROD-A", "quantity": 1, "unit_price_cents": 1000}
    ]
  }' \
  http://localhost:8080/api/v1/orders
```

---

## 🚫 Project Limitations & Out of Scope

This repository represents a highly performant **V1 Order Management System only**. The features and architectures below are explicitly left out to preserve simplicity and prevent scope-creep.

### Key Limitations (What Cannot Be Done Yet)
1.  **Automatic Migration at Startup**: Migrations are applied manually against the Postgres instance to prevent race conditions during startup or multiple instances starting.
2.  **External OIDC Provider Config**: To verify JWT tokens, `OMS_AUTH_MODE=jwt` expects exact issuer, audience, and claim configuration matching the client payload.
3.  **Payment Processing**: The system performs no payment authorization, payment captured flows, or checkouts.
4.  **Inventory & Stock Checks**: No dynamic warehouse inventory reduction or shipment scheduling is integrated. Order item prices and product details are validated statically server-side.

### Strict Architectural Scope Limits
The following third-party integrations and abstractions are **not included** by design:
*   No Redis, Kafka, or RabbitMQ message queues (all transitions and worker processes are db-backed).
*   No GraphQL API layers or dynamic rule engines.
*   No Object-Relational Mappers (ORMs) or complex plug-in patterns (database interaction is raw Go through `sqlc`).
