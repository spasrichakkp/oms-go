package orders

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"oms/internal/auth"
	db "oms/internal/db"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

type stubHandlerService struct {
	createResult      CreateOrderResult
	createErr         error
	getResult         OrderDetails
	getErr            error
	listResult        []OrderDetails
	listErr           error
	cancelResult      OrderDetails
	cancelErr         error
	updateResult      OrderDetails
	updateErr         error
	lastCreateInput   CreateOrderInput
	lastGetOrderID    uuid.UUID
	lastGetCustomer   uuid.UUID
	lastListInput     ListOrdersInput
	lastCancelInput   CancelOrderInput
	lastUpdateOrder   uuid.UUID
	lastUpdateStatus  OrderStatus
	lastUpdateActor   string
	lastUpdateActorID *uuid.UUID
	lastUpdateIP      *string
	updateCalls       int
}

func (s *stubHandlerService) CreateOrder(_ context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	s.lastCreateInput = input
	return s.createResult, s.createErr
}

func (s *stubHandlerService) GetOrder(_ context.Context, orderID, customerID uuid.UUID) (OrderDetails, error) {
	s.lastGetOrderID = orderID
	s.lastGetCustomer = customerID
	return s.getResult, s.getErr
}

func (s *stubHandlerService) ListOrderDetails(_ context.Context, input ListOrdersInput) ([]OrderDetails, error) {
	s.lastListInput = input
	return s.listResult, s.listErr
}

func (s *stubHandlerService) CancelOrderDetails(_ context.Context, input CancelOrderInput) (OrderDetails, error) {
	s.lastCancelInput = input
	return s.cancelResult, s.cancelErr
}

func (s *stubHandlerService) UpdateOrderStatusByTarget(_ context.Context, orderID uuid.UUID, newStatus OrderStatus, actorType string, actorID *uuid.UUID, _ *string, ipAddress *string) (OrderDetails, error) {
	s.updateCalls++
	s.lastUpdateOrder = orderID
	s.lastUpdateStatus = newStatus
	s.lastUpdateActor = actorType
	s.lastUpdateActorID = actorID
	s.lastUpdateIP = ipAddress
	return s.updateResult, s.updateErr
}

func TestOrderRoutesRejectMissingIdentityWithJSON401(t *testing.T) {
	t.Parallel()

	router := newOrdersTestRouter(NewHandler(&stubHandlerService{}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/orders", nil)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusUnauthorized, "unauthorized")
}

func TestOrderRoutesRejectCustomerStatusUpdateWithJSON403(t *testing.T) {
	t.Parallel()

	router := newOrdersTestRouter(NewHandler(&stubHandlerService{}))
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/orders/"+uuid.New().String()+"/status", strings.NewReader(`{"status":"PROCESSING"}`))
	req.Header.Set("X-OMS-Role", "CUSTOMER")
	req.Header.Set("X-OMS-Customer-ID", uuid.New().String())
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusForbidden, "forbidden")
}

func TestCreateOrderRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	router := newOrdersTestRouter(NewHandler(&stubHandlerService{}))
	req := authenticatedRequest(http.MethodPost, "/api/v1/orders", `{"idempotency_key":"idem-1","currency":"USD","customer_id":"`+uuid.New().String()+`","items":[{"product_id":"`+uuid.New().String()+`","sku":"sku-1","quantity":1,"unit_price_cents":100}]}`)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestCreateOrderRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	router := newOrdersTestRouter(NewHandler(&stubHandlerService{}))
	req := authenticatedRequest(http.MethodPost, "/api/v1/orders", strings.Repeat("x", int(DefaultRequestBodyLimit)+1))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusBadRequest, "invalid_request")
}

func TestCreateOrderUsesCustomerIDFromContextAndReturns201(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{
		createResult: CreateOrderResult{
			Order: db.Order{
				ID:         pgUUID(orderID),
				CustomerID: pgUUID(customerID),
				Status:     string(StatusPending),
				Currency:   "USD",
				TotalCents: 100,
				CreatedAt:  pgTimestamptz(time.Unix(1, 0).UTC()),
				UpdatedAt:  pgTimestamptz(time.Unix(2, 0).UTC()),
			},
			Items: []db.OrderItem{{
				OrderID:        pgUUID(orderID),
				ProductID:      pgUUID(uuid.New()),
				Sku:            "sku-1",
				Quantity:       1,
				UnitPriceCents: 100,
			}},
		},
	}

	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodPost, "/api/v1/orders", `{"idempotency_key":"idem-1","currency":"USD","items":[{"product_id":"`+uuid.New().String()+`","sku":"sku-1","quantity":1,"unit_price_cents":100}]}`, customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}

	if svc.lastCreateInput.CustomerID != customerID {
		t.Fatalf("expected customer id from auth context")
	}

	if svc.lastCreateInput.ActorID == nil || *svc.lastCreateInput.ActorID != customerID {
		t.Fatalf("expected create actor id to match auth customer")
	}

	if svc.lastCreateInput.IPAddress == nil || *svc.lastCreateInput.IPAddress != "192.0.2.1" {
		t.Fatalf("expected create request IP to be captured, got %#v", svc.lastCreateInput.IPAddress)
	}
}

func TestGetOrderUsesCustomerIDFromContext(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{
		getResult: OrderDetails{
			Order: db.Order{ID: pgUUID(orderID), CustomerID: pgUUID(customerID), Status: string(StatusPending), Currency: "USD"},
			Items: []db.OrderItem{{OrderID: pgUUID(orderID), ProductID: pgUUID(uuid.New()), Sku: "sku-1", Quantity: 1, UnitPriceCents: 100}},
		},
	}

	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodGet, "/api/v1/orders/"+orderID.String(), "", customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if svc.lastGetCustomer != customerID || svc.lastGetOrderID != orderID {
		t.Fatalf("expected handler to use auth customer and path order id")
	}
}

func TestListOrdersParsesCursorAndReturnsNextCursor(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{
		listResult: []OrderDetails{{
			Order: db.Order{
				ID:         pgUUID(orderID),
				CustomerID: pgUUID(customerID),
				Status:     string(StatusPending),
				Currency:   "USD",
				CreatedAt:  pgTimestamptz(time.Unix(10, 0).UTC()),
				UpdatedAt:  pgTimestamptz(time.Unix(11, 0).UTC()),
			},
			Items: []db.OrderItem{{OrderID: pgUUID(orderID), ProductID: pgUUID(uuid.New()), Sku: "sku-1", Quantity: 1, UnitPriceCents: 100}},
		}},
	}

	cursor := encodeCursor(time.Unix(5, 0).UTC(), uuid.New())
	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodGet, "/api/v1/orders?status=PENDING&limit=1&cursor="+cursor, "", customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if svc.lastListInput.CustomerID != customerID || svc.lastListInput.Limit != 1 || svc.lastListInput.Status == nil || *svc.lastListInput.Status != StatusPending {
		t.Fatalf("expected list input to be populated from auth and query params")
	}

	var body orderListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if body.NextCursor == nil || *body.NextCursor == "" {
		t.Fatalf("expected next_cursor to be set")
	}
}

func TestCancelOrderMapsInvalidTransitionTo409(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{cancelErr: ErrInvalidTransition}
	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodPost, "/api/v1/orders/"+orderID.String()+"/cancel", "", customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusConflict, "conflict")
}

func TestCancelOrderUsesCustomerIDAndRequestIP(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{
		cancelResult: OrderDetails{
			Order: db.Order{ID: pgUUID(orderID), CustomerID: pgUUID(customerID), Status: string(StatusCancelled), Currency: "USD"},
			Items: []db.OrderItem{{OrderID: pgUUID(orderID), ProductID: pgUUID(uuid.New()), Sku: "sku-1", Quantity: 1, UnitPriceCents: 100}},
		},
	}
	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodPost, "/api/v1/orders/"+orderID.String()+"/cancel", "", customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if svc.lastCancelInput.ActorID == nil || *svc.lastCancelInput.ActorID != customerID {
		t.Fatalf("expected cancel actor id to match auth customer")
	}

	if svc.lastCancelInput.IPAddress == nil || *svc.lastCancelInput.IPAddress != "192.0.2.1" {
		t.Fatalf("expected cancel request IP to be captured, got %#v", svc.lastCancelInput.IPAddress)
	}
}

func TestGetOrderMapsNotFoundTo404(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	orderID := uuid.New()
	svc := &stubHandlerService{getErr: ErrOrderNotFound}
	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequestWithCustomer(http.MethodGet, "/api/v1/orders/"+orderID.String(), "", customerID)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusNotFound, "not_found")
}

func TestCreateOrderMapsIdempotencyConflictTo409(t *testing.T) {
	t.Parallel()

	svc := &stubHandlerService{createErr: ErrIdempotencyConflict}
	router := newOrdersTestRouter(NewHandler(svc))
	req := authenticatedRequest(http.MethodPost, "/api/v1/orders", `{"idempotency_key":"idem-1","currency":"USD","items":[{"product_id":"`+uuid.New().String()+`","sku":"sku-1","quantity":1,"unit_price_cents":100}]}`)
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assertErrorResponse(t, rec, http.StatusConflict, "conflict")

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}

	if body.Error.Message != "idempotency conflict" {
		t.Fatalf("expected sanitized idempotency conflict message, got %q", body.Error.Message)
	}

	for _, forbidden := range []string{
		"SQLSTATE",
		idempotencyConstraintName,
		"duplicate key",
		"pgconn",
		"PostgreSQL",
	} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Fatalf("expected response body to hide %q, got %s", forbidden, rec.Body.String())
		}
	}
}

func TestUpdateOrderStatusAllowsAdminAndReturns200(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	svc := &stubHandlerService{
		updateResult: OrderDetails{
			Order: db.Order{ID: pgUUID(orderID), CustomerID: pgUUID(uuid.New()), Status: string(StatusProcessing), Currency: "USD"},
			Items: []db.OrderItem{{OrderID: pgUUID(orderID), ProductID: pgUUID(uuid.New()), Sku: "sku-1", Quantity: 1, UnitPriceCents: 100}},
		},
	}

	router := newOrdersTestRouter(NewHandler(svc))
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/orders/"+orderID.String()+"/status", strings.NewReader(`{"status":"PROCESSING"}`))
	req.Header.Set("X-OMS-Role", "ADMIN")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	if svc.lastUpdateOrder != orderID || svc.lastUpdateStatus != StatusProcessing || svc.lastUpdateActor != ActorTypeAdmin {
		t.Fatalf("expected admin status update call")
	}

	if svc.updateCalls != 1 {
		t.Fatalf("expected one privileged status update call, got %d", svc.updateCalls)
	}

	if svc.lastUpdateActorID != nil {
		t.Fatalf("expected privileged update actor id to remain nil, got %v", *svc.lastUpdateActorID)
	}

	if svc.lastUpdateIP == nil || *svc.lastUpdateIP != "192.0.2.1" {
		t.Fatalf("expected update request IP to be captured, got %#v", svc.lastUpdateIP)
	}
}

func TestUpdateOrderStatusUsesVerifiedPrivilegedSubjectID(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		role auth.Role
	}{
		{name: "admin", role: auth.RoleAdmin},
		{name: "system", role: auth.RoleSystem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			orderID := uuid.New()
			subjectID := uuid.New()
			svc := &stubHandlerService{
				updateResult: OrderDetails{
					Order: db.Order{ID: pgUUID(orderID), CustomerID: pgUUID(uuid.New()), Status: string(StatusProcessing), Currency: "USD"},
					Items: []db.OrderItem{{OrderID: pgUUID(orderID), ProductID: pgUUID(uuid.New()), Sku: "sku-1", Quantity: 1, UnitPriceCents: 100}},
				},
			}

			router := newOrdersIdentityTestRouter(NewHandler(svc), auth.Identity{
				Role:         tc.role,
				Subject:      subjectID.String(),
				HasSubject:   true,
				SubjectID:    subjectID,
				HasSubjectID: true,
			})
			req := httptest.NewRequest(http.MethodPatch, "/api/v1/orders/"+orderID.String()+"/status", strings.NewReader(`{"status":"PROCESSING"}`))
			rec := httptest.NewRecorder()

			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", rec.Code)
			}
			if svc.updateCalls != 1 {
				t.Fatalf("expected one privileged status update call, got %d", svc.updateCalls)
			}
			if svc.lastUpdateActorID == nil || *svc.lastUpdateActorID != subjectID {
				t.Fatalf("expected actor id %s, got %#v", subjectID, svc.lastUpdateActorID)
			}
			if svc.lastUpdateIP == nil || *svc.lastUpdateIP != "192.0.2.1" {
				t.Fatalf("expected update request IP to be captured, got %#v", svc.lastUpdateIP)
			}
		})
	}
}

func newOrdersTestRouter(handler *Handler) http.Handler {
	router := chi.NewRouter()
	router.Route("/api/v1/orders", func(r chi.Router) {
		r.Use(auth.IdentityMiddleware(auth.ModeDev))
		RegisterRoutes(r, handler)
	})
	return router
}

func newOrdersIdentityTestRouter(handler *Handler, identity auth.Identity) http.Handler {
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

func authenticatedRequest(method, path, body string) *http.Request {
	return authenticatedRequestWithCustomer(method, path, body, uuid.New())
}

func authenticatedRequestWithCustomer(method, path, body string, customerID uuid.UUID) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("X-OMS-Role", "CUSTOMER")
	req.Header.Set("X-OMS-Customer-ID", customerID.String())
	return req
}

func assertErrorResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) {
	t.Helper()

	if rec.Code != status {
		t.Fatalf("expected %d, got %d", status, rec.Code)
	}

	var body errorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error response: %v", err)
	}

	if body.Error.Code != code {
		t.Fatalf("expected code %q, got %q", code, body.Error.Code)
	}
}

func pgUUID(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: [16]byte(value),
		Valid: true,
	}
}

func pgTimestamptz(value time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{
		Time:  value,
		Valid: true,
	}
}
