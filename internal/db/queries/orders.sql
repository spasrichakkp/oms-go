-- name: CreateOrder :one
INSERT INTO orders (
    id,
    customer_id,
    status,
    idempotency_key,
    total_cents,
    currency
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6
)
RETURNING *;

-- name: CreateOrderItem :one
INSERT INTO order_items (
    id,
    order_id,
    product_id,
    sku,
    quantity,
    unit_price_cents
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6
)
RETURNING *;

-- name: GetOrderByIDAndCustomerID :one
SELECT *
FROM orders
WHERE id = $1
  AND customer_id = $2
LIMIT 1;

-- name: ListOrdersByCustomerID :many
SELECT *
FROM orders
WHERE customer_id = $1
  AND (
    sqlc.narg(cursor_created_at)::timestamptz IS NULL
    OR (
      created_at < sqlc.narg(cursor_created_at)::timestamptz
      OR (
        created_at = sqlc.narg(cursor_created_at)::timestamptz
        AND id < sqlc.narg(cursor_id)::uuid
      )
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT $2;

-- name: ListOrdersByCustomerIDAndStatus :many
SELECT *
FROM orders
WHERE customer_id = $1
  AND status = $2
  AND (
    sqlc.narg(cursor_created_at)::timestamptz IS NULL
    OR (
      created_at < sqlc.narg(cursor_created_at)::timestamptz
      OR (
        created_at = sqlc.narg(cursor_created_at)::timestamptz
        AND id < sqlc.narg(cursor_id)::uuid
      )
    )
  )
ORDER BY created_at DESC, id DESC
LIMIT $3;

-- name: CancelPendingOrder :one
UPDATE orders
SET status = 'CANCELLED',
    updated_at = now()
WHERE id = $1
  AND customer_id = $2
  AND status = 'PENDING'
RETURNING *;

-- name: UpdateOrderStatusIfCurrentStatusMatches :one
UPDATE orders
SET status = $3,
    updated_at = now()
WHERE id = $1
  AND status = $2
RETURNING *;

-- name: CreateOrderEvent :one
INSERT INTO order_events (
    id,
    order_id,
    actor_type,
    actor_id,
    from_status,
    to_status,
    action,
    reason,
    ip_address
) VALUES (
    $1,
    $2,
    $3,
    $4,
    $5,
    $6,
    $7,
    $8,
    $9
)
RETURNING *;

-- name: GetOrderItemsByOrderID :many
SELECT *
FROM order_items
WHERE order_id = $1
ORDER BY created_at ASC, id ASC;

-- name: GetOrderEventsByOrderID :many
SELECT *
FROM order_events
WHERE order_id = $1
ORDER BY created_at DESC, id DESC;

-- name: WorkerPickAndMovePendingOrdersToProcessing :many
WITH picked_orders AS (
    SELECT id
    FROM orders
    WHERE status = 'PENDING'
    ORDER BY created_at ASC, id ASC
    LIMIT $1
    FOR UPDATE SKIP LOCKED
)
UPDATE orders
SET status = 'PROCESSING',
    updated_at = now()
WHERE id IN (
    SELECT id
    FROM picked_orders
)
RETURNING *;
