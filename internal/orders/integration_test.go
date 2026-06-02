package orders

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"oms/internal/auth"
	db "oms/internal/db"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationOptInEnv = "OMS_RUN_INTEGRATION"

type integrationEnv struct {
	adminConn *pgx.Conn
	pool      *pgxpool.Pool
	repo      *Repository
	service   *Service
	router    http.Handler
	database  string
}

func TestIntegrationPostgres(t *testing.T) {
	t.Parallel()

	env := newIntegrationEnv(t)

	t.Run("applies migrations to a fresh database", func(t *testing.T) {
		assertTablesExist(t, env.pool)
	})

	t.Run("create order persists items and created audit event", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result, err := env.service.CreateOrder(context.Background(), CreateOrderInput{
			CustomerID:     customerID,
			IdempotencyKey: "create-order-audit",
			Currency:       "USD",
			Items: []CreateOrderItemRequest{
				{ProductID: uuid.New().String(), SKU: "sku-1", Quantity: 2, UnitPriceCents: 150},
				{ProductID: uuid.New().String(), SKU: "sku-2", Quantity: 1, UnitPriceCents: 250},
			},
		})
		if err != nil {
			t.Fatalf("create order: %v", err)
		}

		orderID := uuid.UUID(result.Order.ID.Bytes)

		details, err := env.service.GetOrder(context.Background(), orderID, customerID)
		if err != nil {
			t.Fatalf("get order: %v", err)
		}

		if details.Order.Status != string(StatusPending) {
			t.Fatalf("expected PENDING order, got %s", details.Order.Status)
		}

		if details.Order.TotalCents != 550 {
			t.Fatalf("expected total cents 550, got %d", details.Order.TotalCents)
		}

		if len(details.Items) != 2 {
			t.Fatalf("expected 2 order items, got %d", len(details.Items))
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get order events: %v", err)
		}

		if len(events) != 1 {
			t.Fatalf("expected 1 audit event, got %d", len(events))
		}

		event := events[0]
		if event.Action != actionCreated || event.ActorType != ActorTypeCustomer {
			t.Fatalf("expected CREATED event from CUSTOMER, got action=%s actor=%s", event.Action, event.ActorType)
		}

		if !event.ToStatus.Valid || event.ToStatus.String != string(StatusPending) {
			t.Fatalf("expected to_status PENDING, got %#v", event.ToStatus)
		}
	})

	t.Run("http create audit stores customer actor_id and request ip", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		body := `{"idempotency_key":"http-create-audit","currency":"USD","items":[{"product_id":"` + uuid.New().String() + `","sku":"sku-1","quantity":1,"unit_price_cents":100}]}`
		request := authenticatedIntegrationRequestWithRemoteAddr(http.MethodPost, "/api/v1/orders", body, customerID, "198.51.100.10:3210")
		recorder := httptest.NewRecorder()

		env.router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusCreated {
			t.Fatalf("expected create to return 201, got %d: %s", recorder.Code, recorder.Body.String())
		}

		var response orderResponse
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode create response: %v", err)
		}

		orderID, err := uuid.Parse(response.ID)
		if err != nil {
			t.Fatalf("parse created order id: %v", err)
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get order events: %v", err)
		}

		event := findEventByAction(events, actionCreated)
		if event == nil {
			t.Fatal("expected CREATED audit event")
		}

		if !event.ActorID.Valid || uuid.UUID(event.ActorID.Bytes) != customerID {
			t.Fatalf("expected create actor_id %s, got %#v", customerID, event.ActorID)
		}

		if event.IpAddress == nil || event.IpAddress.String() != "198.51.100.10" {
			t.Fatalf("expected create ip_address 198.51.100.10, got %#v", event.IpAddress)
		}
	})

	t.Run("create order idempotency is enforced per customer and key", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		input := CreateOrderInput{
			CustomerID:     customerID,
			IdempotencyKey: "idem-same-customer",
			Currency:       "USD",
			Items: []CreateOrderItemRequest{
				{ProductID: uuid.New().String(), SKU: "sku-1", Quantity: 1, UnitPriceCents: 100},
			},
		}

		if _, err := env.service.CreateOrder(context.Background(), input); err != nil {
			t.Fatalf("first create order: %v", err)
		}

		if _, err := env.service.CreateOrder(context.Background(), input); !errors.Is(err, ErrIdempotencyConflict) {
			t.Fatalf("expected idempotency conflict, got %v", err)
		}

		var count int
		if err := env.pool.QueryRow(context.Background(), `
			SELECT count(*)
			FROM orders
			WHERE customer_id = $1
			  AND idempotency_key = $2
		`, customerID, input.IdempotencyKey).Scan(&count); err != nil {
			t.Fatalf("count orders: %v", err)
		}

		if count != 1 {
			t.Fatalf("expected exactly 1 order for customer+idempotency key, got %d", count)
		}
	})

	t.Run("duplicate create http response is sanitized", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		body := `{"idempotency_key":"http-idem-duplicate","currency":"USD","items":[{"product_id":"` + uuid.New().String() + `","sku":"sku-1","quantity":1,"unit_price_cents":100}]}`

		firstRequest := authenticatedIntegrationRequest(http.MethodPost, "/api/v1/orders", body, customerID)
		firstRecorder := httptest.NewRecorder()
		env.router.ServeHTTP(firstRecorder, firstRequest)
		if firstRecorder.Code != http.StatusCreated {
			t.Fatalf("expected first create to return 201, got %d: %s", firstRecorder.Code, firstRecorder.Body.String())
		}

		secondRequest := authenticatedIntegrationRequest(http.MethodPost, "/api/v1/orders", body, customerID)
		secondRecorder := httptest.NewRecorder()
		env.router.ServeHTTP(secondRecorder, secondRequest)

		if secondRecorder.Code != http.StatusConflict {
			t.Fatalf("expected duplicate create to return 409, got %d: %s", secondRecorder.Code, secondRecorder.Body.String())
		}

		var response errorResponse
		if err := json.Unmarshal(secondRecorder.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode duplicate conflict response: %v", err)
		}

		if response.Error.Code != "conflict" || response.Error.Message != "idempotency conflict" {
			t.Fatalf("expected sanitized duplicate conflict response, got %#v", response.Error)
		}

		for _, forbidden := range []string{
			"SQLSTATE",
			idempotencyConstraintName,
			"duplicate key",
			"pgconn",
			"PostgreSQL",
			"orders",
		} {
			if strings.Contains(secondRecorder.Body.String(), forbidden) {
				t.Fatalf("expected duplicate conflict response to hide %q, got %s", forbidden, secondRecorder.Body.String())
			}
		}
	})

	t.Run("get order enforces customer ownership", func(t *testing.T) {
		env.resetData(t)

		ownerID := uuid.New()
		otherCustomerID := uuid.New()
		result := createIntegrationOrder(t, env.service, ownerID, "owned-order", 1, 125)

		_, err := env.service.GetOrder(context.Background(), uuid.UUID(result.Order.ID.Bytes), otherCustomerID)
		if !errors.Is(err, ErrOrderNotFound) {
			t.Fatalf("expected owned order lookup to fail for other customer, got %v", err)
		}
	})

	t.Run("list orders enforces ownership and status filter", func(t *testing.T) {
		env.resetData(t)

		customerA := uuid.New()
		customerB := uuid.New()

		pendingA := createIntegrationOrder(t, env.service, customerA, "list-a-pending", 1, 100)
		processingA := createIntegrationOrder(t, env.service, customerA, "list-a-processing", 1, 200)
		createIntegrationOrder(t, env.service, customerB, "list-b-pending", 1, 300)

		_, err := env.service.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
			OrderID:        uuid.UUID(processingA.Order.ID.Bytes),
			ExpectedStatus: StatusPending,
			NewStatus:      StatusProcessing,
			ActorType:      ActorTypeAdmin,
		})
		if err != nil {
			t.Fatalf("advance processing order: %v", err)
		}

		ordersList, err := env.service.ListOrders(context.Background(), ListOrdersInput{
			CustomerID: customerA,
			Limit:      10,
		})
		if err != nil {
			t.Fatalf("list orders: %v", err)
		}

		if len(ordersList) != 2 {
			t.Fatalf("expected 2 orders for customer A, got %d", len(ordersList))
		}

		for _, order := range ordersList {
			if uuid.UUID(order.CustomerID.Bytes) != customerA {
				t.Fatalf("expected only customer A orders, got customer %s", uuid.UUID(order.CustomerID.Bytes))
			}
		}

		status := StatusPending
		filtered, err := env.service.ListOrders(context.Background(), ListOrdersInput{
			CustomerID: customerA,
			Limit:      10,
			Status:     &status,
		})
		if err != nil {
			t.Fatalf("list filtered orders: %v", err)
		}

		if len(filtered) != 1 || uuid.UUID(filtered[0].ID.Bytes) != uuid.UUID(pendingA.Order.ID.Bytes) {
			t.Fatalf("expected only the pending order for customer A")
		}
	})

	t.Run("cancel succeeds only for pending", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "cancel-pending", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		cancelled, err := env.service.CancelOrder(context.Background(), CancelOrderInput{
			OrderID:    orderID,
			CustomerID: customerID,
		})
		if err != nil {
			t.Fatalf("cancel order: %v", err)
		}

		if cancelled.Status != string(StatusCancelled) {
			t.Fatalf("expected CANCELLED, got %s", cancelled.Status)
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get cancel events: %v", err)
		}

		cancelEvent := findEventByAction(events, actionCancelled)
		if cancelEvent == nil {
			t.Fatal("expected CANCELLED audit event")
		}
	})

	t.Run("http cancel audit stores customer actor_id and request ip", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "http-cancel-audit", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		request := authenticatedIntegrationRequestWithRemoteAddr(http.MethodPost, "/api/v1/orders/"+orderID.String()+"/cancel", "", customerID, "198.51.100.11:4210")
		recorder := httptest.NewRecorder()
		env.router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected cancel to return 200, got %d: %s", recorder.Code, recorder.Body.String())
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get cancel events: %v", err)
		}

		cancelEvent := findEventByAction(events, actionCancelled)
		if cancelEvent == nil {
			t.Fatal("expected CANCELLED audit event")
		}

		if !cancelEvent.ActorID.Valid || uuid.UUID(cancelEvent.ActorID.Bytes) != customerID {
			t.Fatalf("expected cancel actor_id %s, got %#v", customerID, cancelEvent.ActorID)
		}

		if cancelEvent.IpAddress == nil || cancelEvent.IpAddress.String() != "198.51.100.11" {
			t.Fatalf("expected cancel ip_address 198.51.100.11, got %#v", cancelEvent.IpAddress)
		}
	})

	t.Run("cancel fails for processing shipped delivered and cancelled", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		cases := []struct {
			name        string
			targetState OrderStatus
		}{
			{name: "processing", targetState: StatusProcessing},
			{name: "shipped", targetState: StatusShipped},
			{name: "delivered", targetState: StatusDelivered},
			{name: "cancelled", targetState: StatusCancelled},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				result := createIntegrationOrder(t, env.service, customerID, "cancel-"+tc.name+"-"+uuid.NewString(), 1, 100)
				orderID := uuid.UUID(result.Order.ID.Bytes)
				if tc.targetState == StatusCancelled {
					if _, err := env.service.CancelOrder(context.Background(), CancelOrderInput{
						OrderID:    orderID,
						CustomerID: customerID,
					}); err != nil {
						t.Fatalf("seed cancelled order: %v", err)
					}
				} else {
					moveOrderToStatus(t, env.service, orderID, tc.targetState)
				}

				_, err := env.service.CancelOrder(context.Background(), CancelOrderInput{
					OrderID:    orderID,
					CustomerID: customerID,
				})
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("expected invalid transition for %s, got %v", tc.targetState, err)
				}

				details, err := env.service.GetOrder(context.Background(), orderID, customerID)
				if err != nil {
					t.Fatalf("reload order: %v", err)
				}

				if details.Order.Status != string(tc.targetState) {
					t.Fatalf("expected status %s to remain unchanged, got %s", tc.targetState, details.Order.Status)
				}
			})
		}
	})

	t.Run("admin and system status updates follow valid transitions", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "admin-system-status", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		processing, err := env.service.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
			OrderID:        orderID,
			ExpectedStatus: StatusPending,
			NewStatus:      StatusProcessing,
			ActorType:      ActorTypeAdmin,
		})
		if err != nil {
			t.Fatalf("admin status update: %v", err)
		}

		if processing.Status != string(StatusProcessing) {
			t.Fatalf("expected PROCESSING, got %s", processing.Status)
		}

		shipped, err := env.service.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
			OrderID:        orderID,
			ExpectedStatus: StatusProcessing,
			NewStatus:      StatusShipped,
			ActorType:      ActorTypeSystem,
		})
		if err != nil {
			t.Fatalf("system status update: %v", err)
		}

		if shipped.Status != string(StatusShipped) {
			t.Fatalf("expected SHIPPED, got %s", shipped.Status)
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get status update events: %v", err)
		}

		if !hasEvent(events, actionStatusUpdated, ActorTypeAdmin, string(StatusPending), string(StatusProcessing)) {
			t.Fatal("expected admin status update audit event")
		}

		if !hasEvent(events, actionStatusUpdated, ActorTypeSystem, string(StatusProcessing), string(StatusShipped)) {
			t.Fatal("expected system status update audit event")
		}
	})

	t.Run("http admin status update keeps nil actor_id and stores request ip", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "http-admin-status-audit", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		request := privilegedIntegrationRequestWithRemoteAddr(http.MethodPatch, "/api/v1/orders/"+orderID.String()+"/status", `{"status":"PROCESSING"}`, auth.RoleAdmin, "198.51.100.12:5210")
		recorder := httptest.NewRecorder()
		env.router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected admin update to return 200, got %d: %s", recorder.Code, recorder.Body.String())
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get admin status events: %v", err)
		}

		statusEvent := findEventByAction(events, actionStatusUpdated)
		if statusEvent == nil {
			t.Fatal("expected STATUS_UPDATED audit event")
		}

		if statusEvent.ActorType != ActorTypeAdmin {
			t.Fatalf("expected admin actor_type, got %s", statusEvent.ActorType)
		}

		if statusEvent.ActorID.Valid {
			t.Fatalf("expected admin actor_id to remain nil without a real subject mapping, got %#v", statusEvent.ActorID)
		}

		if statusEvent.IpAddress == nil || statusEvent.IpAddress.String() != "198.51.100.12" {
			t.Fatalf("expected admin ip_address 198.51.100.12, got %#v", statusEvent.IpAddress)
		}
	})

	t.Run("http admin status update stores verified jwt subject actor_id and request ip", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		subjectID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "http-admin-jwt-status-audit", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		router := newIntegrationIdentityRouter(NewHandler(env.service), auth.Identity{
			Role:         auth.RoleAdmin,
			Subject:      subjectID.String(),
			HasSubject:   true,
			SubjectID:    subjectID,
			HasSubjectID: true,
		})
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/orders/"+orderID.String()+"/status", strings.NewReader(`{"status":"PROCESSING"}`))
		request.RemoteAddr = "198.51.100.13:6210"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("expected admin update to return 200, got %d: %s", recorder.Code, recorder.Body.String())
		}

		events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
		if err != nil {
			t.Fatalf("get admin status events: %v", err)
		}

		statusEvent := findEventByAction(events, actionStatusUpdated)
		if statusEvent == nil {
			t.Fatal("expected STATUS_UPDATED audit event")
		}

		if statusEvent.ActorType != ActorTypeAdmin {
			t.Fatalf("expected admin actor_type, got %s", statusEvent.ActorType)
		}

		if !statusEvent.ActorID.Valid || uuid.UUID(statusEvent.ActorID.Bytes) != subjectID {
			t.Fatalf("expected admin actor_id %s, got %#v", subjectID, statusEvent.ActorID)
		}

		if statusEvent.IpAddress == nil || statusEvent.IpAddress.String() != "198.51.100.13" {
			t.Fatalf("expected admin ip_address 198.51.100.13, got %#v", statusEvent.IpAddress)
		}
	})

	t.Run("invalid status transition fails", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		result := createIntegrationOrder(t, env.service, customerID, "invalid-status-transition", 1, 100)
		orderID := uuid.UUID(result.Order.ID.Bytes)

		_, err := env.service.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
			OrderID:        orderID,
			ExpectedStatus: StatusPending,
			NewStatus:      StatusDelivered,
			ActorType:      ActorTypeAdmin,
		})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("expected invalid transition, got %v", err)
		}
	})

	t.Run("worker moves pending orders to processing and writes system events", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()
		pendingOne := createIntegrationOrder(t, env.service, customerID, "worker-pending-1", 1, 100)
		pendingTwo := createIntegrationOrder(t, env.service, customerID, "worker-pending-2", 1, 100)
		pendingThree := createIntegrationOrder(t, env.service, customerID, "worker-pending-3", 1, 100)
		shipped := createIntegrationOrder(t, env.service, customerID, "worker-shipped", 1, 100)
		moveOrderToStatus(t, env.service, uuid.UUID(shipped.Order.ID.Bytes), StatusShipped)

		processed, err := env.service.ProcessPendingOrdersBatch(context.Background(), ProcessPendingOrdersBatchInput{
			Limit: 2,
		})
		if err != nil {
			t.Fatalf("process pending batch: %v", err)
		}

		if len(processed) != 2 {
			t.Fatalf("expected 2 processed orders, got %d", len(processed))
		}

		processedIDs := make(map[string]struct{}, len(processed))
		for _, order := range processed {
			if order.Status != string(StatusProcessing) {
				t.Fatalf("expected processed order status PROCESSING, got %s", order.Status)
			}

			orderID := uuid.UUID(order.ID.Bytes)
			processedIDs[orderID.String()] = struct{}{}
			events, err := env.repo.GetOrderEventsByOrderID(context.Background(), orderID)
			if err != nil {
				t.Fatalf("get worker events: %v", err)
			}

			if !hasEvent(events, actionWorkerMovedToProcessing, ActorTypeSystem, string(StatusPending), string(StatusProcessing)) {
				t.Fatalf("expected worker system audit event for order %s", orderID)
			}
		}

		for _, candidate := range []uuid.UUID{
			uuid.UUID(pendingOne.Order.ID.Bytes),
			uuid.UUID(pendingTwo.Order.ID.Bytes),
			uuid.UUID(pendingThree.Order.ID.Bytes),
		} {
			details, err := env.service.GetOrder(context.Background(), candidate, customerID)
			if err != nil {
				t.Fatalf("reload worker candidate: %v", err)
			}

			_, wasProcessed := processedIDs[candidate.String()]
			expected := string(StatusPending)
			if wasProcessed {
				expected = string(StatusProcessing)
			}

			if details.Order.Status != expected {
				t.Fatalf("expected status %s for order %s, got %s", expected, candidate, details.Order.Status)
			}
		}

		shippedDetails, err := env.service.GetOrder(context.Background(), uuid.UUID(shipped.Order.ID.Bytes), customerID)
		if err != nil {
			t.Fatalf("reload shipped order: %v", err)
		}

		if shippedDetails.Order.Status != string(StatusShipped) {
			t.Fatalf("expected shipped order to remain SHIPPED, got %s", shippedDetails.Order.Status)
		}
	})

	t.Run("http rejects unknown json fields and oversized bodies", func(t *testing.T) {
		env.resetData(t)

		customerID := uuid.New()

		unknownFieldRequest := authenticatedIntegrationRequest(http.MethodPost, "/api/v1/orders", `{"idempotency_key":"http-unknown","currency":"USD","items":[{"product_id":"`+uuid.New().String()+`","sku":"sku-1","quantity":1,"unit_price_cents":100}],"unexpected":true}`, customerID)
		unknownFieldRecorder := httptest.NewRecorder()
		env.router.ServeHTTP(unknownFieldRecorder, unknownFieldRequest)
		assertErrorResponse(t, unknownFieldRecorder, http.StatusBadRequest, "invalid_request")

		oversizedRequest := authenticatedIntegrationRequest(http.MethodPost, "/api/v1/orders", strings.Repeat("x", int(DefaultRequestBodyLimit)+1), customerID)
		oversizedRecorder := httptest.NewRecorder()
		env.router.ServeHTTP(oversizedRecorder, oversizedRequest)
		assertErrorResponse(t, oversizedRecorder, http.StatusBadRequest, "invalid_request")
	})
}

func newIntegrationEnv(t *testing.T) *integrationEnv {
	t.Helper()

	if os.Getenv(integrationOptInEnv) != "1" {
		t.Skipf("%s=1 and DATABASE_URL are required for PostgreSQL integration tests", integrationOptInEnv)
	}

	baseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if baseURL == "" {
		t.Skip("DATABASE_URL is required for PostgreSQL integration tests")
	}

	ctx := context.Background()

	adminConn, err := pgx.Connect(ctx, baseURL)
	if err != nil {
		t.Fatalf("connect admin database: %v", err)
	}

	databaseName := "oms_integration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := adminConn.Exec(ctx, fmt.Sprintf("CREATE DATABASE %s", databaseName)); err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("create integration database: %v", err)
	}

	tempDatabaseURL, err := databaseURLWithDatabase(baseURL, databaseName)
	if err != nil {
		_ = adminConn.Close(ctx)
		t.Fatalf("build integration database url: %v", err)
	}

	if err := applyMigration(ctx, tempDatabaseURL, readMigrationSQL(t)); err != nil {
		_, _ = adminConn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", databaseName))
		_ = adminConn.Close(ctx)
		t.Fatalf("apply migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, tempDatabaseURL)
	if err != nil {
		_, _ = adminConn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", databaseName))
		_ = adminConn.Close(ctx)
		t.Fatalf("create integration pool: %v", err)
	}

	env := &integrationEnv{
		adminConn: adminConn,
		pool:      pool,
		repo:      NewRepository(pool),
		database:  databaseName,
	}
	env.service = NewService(env.repo)
	env.router = newIntegrationRouter(NewHandler(env.service))

	t.Cleanup(func() {
		env.pool.Close()
		_, _ = env.adminConn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()", env.database)
		_, _ = env.adminConn.Exec(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", env.database))
		_ = env.adminConn.Close(ctx)
	})

	return env
}

func (e *integrationEnv) resetData(t *testing.T) {
	t.Helper()

	if _, err := e.pool.Exec(context.Background(), "TRUNCATE order_events, order_items, orders"); err != nil {
		t.Fatalf("reset integration data: %v", err)
	}
}

func createIntegrationOrder(t *testing.T, service *Service, customerID uuid.UUID, idempotencyKey string, quantity int32, unitPriceCents int64) CreateOrderResult {
	t.Helper()

	result, err := service.CreateOrder(context.Background(), CreateOrderInput{
		CustomerID:     customerID,
		IdempotencyKey: idempotencyKey,
		Currency:       "USD",
		Items: []CreateOrderItemRequest{
			{
				ProductID:      uuid.New().String(),
				SKU:            "sku-" + idempotencyKey,
				Quantity:       quantity,
				UnitPriceCents: unitPriceCents,
			},
		},
	})
	if err != nil {
		t.Fatalf("create integration order: %v", err)
	}

	return result
}

func moveOrderToStatus(t *testing.T, service *Service, orderID uuid.UUID, target OrderStatus) {
	t.Helper()

	transitions := []struct {
		expected OrderStatus
		next     OrderStatus
		actor    string
	}{
		{expected: StatusPending, next: StatusProcessing, actor: ActorTypeAdmin},
		{expected: StatusProcessing, next: StatusShipped, actor: ActorTypeSystem},
		{expected: StatusShipped, next: StatusDelivered, actor: ActorTypeAdmin},
	}

	switch target {
	case StatusPending:
		return
	case StatusProcessing:
		transitions = transitions[:1]
	case StatusShipped:
		transitions = transitions[:2]
	case StatusDelivered:
	default:
		t.Fatalf("unsupported target status %s", target)
	}

	for _, step := range transitions {
		if _, err := service.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
			OrderID:        orderID,
			ExpectedStatus: step.expected,
			NewStatus:      step.next,
			ActorType:      step.actor,
		}); err != nil {
			t.Fatalf("move order to %s: %v", step.next, err)
		}
	}
}

func hasEvent(events []db.OrderEvent, action, actorType, fromStatus, toStatus string) bool {
	for _, event := range events {
		if event.Action != action || event.ActorType != actorType {
			continue
		}

		if fromStatus != "" {
			if !event.FromStatus.Valid || event.FromStatus.String != fromStatus {
				continue
			}
		}

		if toStatus != "" {
			if !event.ToStatus.Valid || event.ToStatus.String != toStatus {
				continue
			}
		}

		return true
	}

	return false
}

func findEventByAction(events []db.OrderEvent, action string) *db.OrderEvent {
	for index := range events {
		if events[index].Action == action {
			return &events[index]
		}
	}

	return nil
}

func newIntegrationRouter(handler *Handler) http.Handler {
	router := chi.NewRouter()
	router.Route("/api/v1/orders", func(r chi.Router) {
		r.Use(auth.IdentityMiddleware(auth.ModeDev))
		RegisterRoutes(r, handler)
	})
	return router
}

func newIntegrationIdentityRouter(handler *Handler, identity auth.Identity) http.Handler {
	router := chi.NewRouter()
	router.Route("/api/v1/orders", func(r chi.Router) {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), identity)))
			})
		})
		RegisterRoutes(r, handler)
	})
	return router
}

func authenticatedIntegrationRequest(method, path, body string, customerID uuid.UUID) *http.Request {
	return authenticatedIntegrationRequestWithRemoteAddr(method, path, body, customerID, "192.0.2.1:1234")
}

func authenticatedIntegrationRequestWithRemoteAddr(method, path, body string, customerID uuid.UUID, remoteAddr string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-OMS-Role", "CUSTOMER")
	req.Header.Set("X-OMS-Customer-ID", customerID.String())
	req.RemoteAddr = remoteAddr
	return req
}

func privilegedIntegrationRequestWithRemoteAddr(method, path, body string, role auth.Role, remoteAddr string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-OMS-Role", string(role))
	req.RemoteAddr = remoteAddr
	return req
}

func assertTablesExist(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()

	for _, table := range []string{"orders", "order_items", "order_events"} {
		var exists bool
		if err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1
				FROM information_schema.tables
				WHERE table_schema = 'public'
				  AND table_name = $1
			)
		`, table).Scan(&exists); err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}

		if !exists {
			t.Fatalf("expected table %s to exist", table)
		}
	}
}

func databaseURLWithDatabase(baseURL, databaseName string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}

	parsed.Path = "/" + databaseName
	return parsed.String(), nil
}

func readMigrationSQL(t *testing.T) string {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}

	migrationPath := filepath.Join(filepath.Dir(currentFile), "..", "..", "db", "migrations", "000001_init_oms_schema.up.sql")
	bytes, err := os.ReadFile(migrationPath)
	if err != nil {
		t.Fatalf("read migration sql: %v", err)
	}

	return string(bytes)
}

func applyMigration(ctx context.Context, databaseURL, migrationSQL string) error {
	connConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return err
	}

	connConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol

	conn, err := pgx.ConnectConfig(ctx, connConfig)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx, migrationSQL)
	return err
}
