package orders

import (
	"context"
	"errors"
	"strings"
	"testing"

	db "oms/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

type stubRepository struct {
	createOrderResult         CreateOrderResult
	createOrderErr            error
	getOrderResult            db.Order
	getOrderErr               error
	getOrderItemsResult       []db.OrderItem
	getOrderItemsErr          error
	getOrderEventsResult      []db.OrderEvent
	getOrderEventsErr         error
	listOrdersResult          []db.Order
	listOrdersErr             error
	cancelOrderResult         db.Order
	cancelOrderErr            error
	updateOrderStatusResult   db.Order
	updateOrderStatusErr      error
	processPendingBatchResult []db.Order
	processPendingBatchErr    error

	lastCreateOrderCommand       CreateOrderCommand
	lastListOrdersCommand        ListOrdersCommand
	lastCancelOrderCommand       CancelOrderCommand
	lastUpdateOrderStatusCommand UpdateOrderStatusCommand
	lastProcessBatchCommand      ProcessPendingOrdersBatchCommand
}

func (s *stubRepository) CreateOrderWithItemsAndAudit(_ context.Context, cmd CreateOrderCommand) (CreateOrderResult, error) {
	s.lastCreateOrderCommand = cmd
	return s.createOrderResult, s.createOrderErr
}

func (s *stubRepository) GetOrderByIDAndCustomerID(_ context.Context, _, _ uuid.UUID) (db.Order, error) {
	return s.getOrderResult, s.getOrderErr
}

func (s *stubRepository) GetOrderItemsByOrderID(_ context.Context, _ uuid.UUID) ([]db.OrderItem, error) {
	return s.getOrderItemsResult, s.getOrderItemsErr
}

func (s *stubRepository) GetOrderEventsByOrderID(_ context.Context, _ uuid.UUID) ([]db.OrderEvent, error) {
	return s.getOrderEventsResult, s.getOrderEventsErr
}

func (s *stubRepository) ListOrders(_ context.Context, cmd ListOrdersCommand) ([]db.Order, error) {
	s.lastListOrdersCommand = cmd
	return s.listOrdersResult, s.listOrdersErr
}

func (s *stubRepository) CancelOrderWithAudit(_ context.Context, cmd CancelOrderCommand) (db.Order, error) {
	s.lastCancelOrderCommand = cmd
	return s.cancelOrderResult, s.cancelOrderErr
}

func (s *stubRepository) UpdateOrderStatusWithAudit(_ context.Context, cmd UpdateOrderStatusCommand) (db.Order, error) {
	s.lastUpdateOrderStatusCommand = cmd
	return s.updateOrderStatusResult, s.updateOrderStatusErr
}

func (s *stubRepository) ProcessPendingOrdersBatchWithAudit(_ context.Context, cmd ProcessPendingOrdersBatchCommand) ([]db.Order, error) {
	s.lastProcessBatchCommand = cmd
	return s.processPendingBatchResult, s.processPendingBatchErr
}

func TestServiceCreateOrderComputesTotalCents(t *testing.T) {
	t.Parallel()

	customerID := uuid.New()
	productID1 := uuid.New()
	productID2 := uuid.New()
	repo := &stubRepository{}

	svc := NewService(repo)
	svc.newUUID = func() uuid.UUID { return uuid.Nil }

	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		CustomerID:     customerID,
		IdempotencyKey: "idem-123",
		Currency:       "USD",
		Items: []CreateOrderItemRequest{
			{ProductID: productID1.String(), SKU: "sku-1", Quantity: 2, UnitPriceCents: 150},
			{ProductID: productID2.String(), SKU: "sku-2", Quantity: 3, UnitPriceCents: 200},
		},
	})
	if err != nil {
		t.Fatalf("expected create order to succeed, got %v", err)
	}

	if repo.lastCreateOrderCommand.CustomerID != customerID {
		t.Fatalf("expected customer id from service input")
	}

	if repo.lastCreateOrderCommand.TotalCents != 900 {
		t.Fatalf("expected total cents 900, got %d", repo.lastCreateOrderCommand.TotalCents)
	}
}

func TestServiceCreateOrderRejectsOverflow(t *testing.T) {
	t.Parallel()

	svc := NewService(&stubRepository{})
	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		CustomerID:     uuid.New(),
		IdempotencyKey: "idem-123",
		Currency:       "USD",
		Items: []CreateOrderItemRequest{
			{ProductID: uuid.New().String(), SKU: "sku-1", Quantity: 2, UnitPriceCents: 1 << 62},
		},
	})
	if !errors.Is(err, ErrTotalCentsOverflow) {
		t.Fatalf("expected overflow error, got %v", err)
	}
}

func TestServiceCreateOrderMapsIdempotencyConflict(t *testing.T) {
	t.Parallel()

	repo := &stubRepository{
		createOrderErr: &pgconn.PgError{
			Code:           uniqueViolationCode,
			ConstraintName: idempotencyConstraintName,
			Message:        `duplicate key value violates unique constraint "orders_customer_id_idempotency_key_key"`,
		},
	}
	svc := NewService(repo)
	svc.newUUID = func() uuid.UUID { return uuid.Nil }

	_, err := svc.CreateOrder(context.Background(), CreateOrderInput{
		CustomerID:     uuid.New(),
		IdempotencyKey: "idem-123",
		Currency:       "USD",
		Items: []CreateOrderItemRequest{
			{ProductID: uuid.New().String(), SKU: "sku-1", Quantity: 1, UnitPriceCents: 100},
		},
	})
	if !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("expected idempotency conflict, got %v", err)
	}

	for _, forbidden := range []string{
		"SQLSTATE",
		idempotencyConstraintName,
		"duplicate key",
		"pgconn",
		"PostgreSQL",
	} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("expected sanitized idempotency conflict error, got %q", err.Error())
		}
	}
}

func TestServiceGetOrderReturnsItems(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	customerID := uuid.New()
	repo := &stubRepository{
		getOrderResult:      db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID)},
		getOrderItemsResult: []db.OrderItem{{OrderID: serviceTestPGUUID(orderID)}},
	}

	svc := NewService(repo)
	details, err := svc.GetOrder(context.Background(), orderID, customerID)
	if err != nil {
		t.Fatalf("expected get order success, got %v", err)
	}

	if details.Order.ID.String() != orderID.String() || len(details.Items) != 1 {
		t.Fatalf("expected order details with one item")
	}
}

func TestServiceGetOrderMapsNotFound(t *testing.T) {
	t.Parallel()

	svc := NewService(&stubRepository{getOrderErr: pgx.ErrNoRows})
	_, err := svc.GetOrder(context.Background(), uuid.New(), uuid.New())
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("expected order not found, got %v", err)
	}
}

func TestServiceCancelOrderRejectsInvalidTransition(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	customerID := uuid.New()
	repo := &stubRepository{
		getOrderResult: db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID), Status: string(StatusProcessing)},
	}

	svc := NewService(repo)
	_, err := svc.CancelOrder(context.Background(), CancelOrderInput{
		OrderID:    orderID,
		CustomerID: customerID,
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}
}

func TestServiceCancelOrderCallsRepositoryForPending(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	customerID := uuid.New()
	repo := &stubRepository{
		getOrderResult:    db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID), Status: string(StatusPending)},
		cancelOrderResult: db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID), Status: string(StatusCancelled)},
	}

	svc := NewService(repo)
	order, err := svc.CancelOrder(context.Background(), CancelOrderInput{
		OrderID:    orderID,
		CustomerID: customerID,
	})
	if err != nil {
		t.Fatalf("expected cancel success, got %v", err)
	}

	if order.Status != string(StatusCancelled) {
		t.Fatalf("expected cancelled status")
	}
}

func TestServiceUpdateOrderStatusValidatesActorAndTransition(t *testing.T) {
	t.Parallel()

	svc := NewService(&stubRepository{})

	_, err := svc.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
		OrderID:        uuid.New(),
		ExpectedStatus: StatusDelivered,
		NewStatus:      StatusPending,
		ActorType:      ActorTypeAdmin,
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("expected invalid transition, got %v", err)
	}

	_, err = svc.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
		OrderID:        uuid.New(),
		ExpectedStatus: StatusPending,
		NewStatus:      StatusProcessing,
		ActorType:      ActorTypeCustomer,
	})
	if !errors.Is(err, ErrInvalidActorType) {
		t.Fatalf("expected invalid actor type, got %v", err)
	}
}

func TestServiceUpdateOrderStatusCallsRepository(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	repo := &stubRepository{
		updateOrderStatusResult: db.Order{ID: serviceTestPGUUID(orderID), Status: string(StatusShipped)},
	}

	svc := NewService(repo)
	order, err := svc.UpdateOrderStatus(context.Background(), UpdateOrderStatusInput{
		OrderID:        orderID,
		ExpectedStatus: StatusProcessing,
		NewStatus:      StatusShipped,
		ActorType:      ActorTypeAdmin,
	})
	if err != nil {
		t.Fatalf("expected status update success, got %v", err)
	}

	if order.Status != string(StatusShipped) || repo.lastUpdateOrderStatusCommand.ActorType != ActorTypeAdmin {
		t.Fatalf("expected repository update command")
	}
}

func TestServiceListOrderDetailsReturnsItems(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	customerID := uuid.New()
	repo := &stubRepository{
		listOrdersResult:    []db.Order{{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID)}},
		getOrderItemsResult: []db.OrderItem{{OrderID: serviceTestPGUUID(orderID)}},
	}

	svc := NewService(repo)
	details, err := svc.ListOrderDetails(context.Background(), ListOrdersInput{
		CustomerID: customerID,
		Limit:      10,
	})
	if err != nil {
		t.Fatalf("expected list details success, got %v", err)
	}

	if len(details) != 1 || len(details[0].Items) != 1 {
		t.Fatalf("expected hydrated order details")
	}
}

func TestServiceCancelOrderDetailsReturnsItems(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	customerID := uuid.New()
	repo := &stubRepository{
		getOrderResult:      db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID), Status: string(StatusPending)},
		cancelOrderResult:   db.Order{ID: serviceTestPGUUID(orderID), CustomerID: serviceTestPGUUID(customerID), Status: string(StatusCancelled)},
		getOrderItemsResult: []db.OrderItem{{OrderID: serviceTestPGUUID(orderID)}},
	}

	svc := NewService(repo)
	details, err := svc.CancelOrderDetails(context.Background(), CancelOrderInput{
		OrderID:    orderID,
		CustomerID: customerID,
	})
	if err != nil {
		t.Fatalf("expected cancel details success, got %v", err)
	}

	if details.Order.Status != string(StatusCancelled) || len(details.Items) != 1 {
		t.Fatalf("expected cancelled order details")
	}
}

func TestServiceUpdateOrderStatusDetailsMapsNoRowsToNotFound(t *testing.T) {
	t.Parallel()

	repo := &stubRepository{
		updateOrderStatusErr: errors.New("placeholder"),
	}
	repo.updateOrderStatusErr = pgx.ErrNoRows
	svc := NewService(repo)

	_, err := svc.UpdateOrderStatusDetails(context.Background(), UpdateOrderStatusInput{
		OrderID:        uuid.New(),
		ExpectedStatus: StatusPending,
		NewStatus:      StatusProcessing,
		ActorType:      ActorTypeAdmin,
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestServiceUpdateOrderStatusByTargetInfersExpectedStatus(t *testing.T) {
	t.Parallel()

	orderID := uuid.New()
	repo := &stubRepository{
		updateOrderStatusResult: db.Order{ID: serviceTestPGUUID(orderID), Status: string(StatusProcessing)},
		getOrderItemsResult:     []db.OrderItem{{OrderID: serviceTestPGUUID(orderID)}},
	}

	svc := NewService(repo)
	details, err := svc.UpdateOrderStatusByTarget(context.Background(), orderID, StatusProcessing, ActorTypeAdmin, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected status update by target success, got %v", err)
	}

	if repo.lastUpdateOrderStatusCommand.ExpectedStatus != StatusPending {
		t.Fatalf("expected inferred status %q, got %q", StatusPending, repo.lastUpdateOrderStatusCommand.ExpectedStatus)
	}

	if details.Order.Status != string(StatusProcessing) || len(details.Items) != 1 {
		t.Fatalf("expected updated order details")
	}
}

func TestServiceProcessPendingOrdersBatchUsesRepository(t *testing.T) {
	t.Parallel()

	repo := &stubRepository{
		processPendingBatchResult: []db.Order{{Status: string(StatusProcessing)}},
	}

	svc := NewService(repo)
	orders, err := svc.ProcessPendingOrdersBatch(context.Background(), ProcessPendingOrdersBatchInput{Limit: 10})
	if err != nil {
		t.Fatalf("expected worker batch success, got %v", err)
	}

	if len(orders) != 1 || repo.lastProcessBatchCommand.Limit != 10 {
		t.Fatalf("expected worker repository command")
	}
}

func serviceTestPGUUID(value uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: [16]byte(value),
		Valid: true,
	}
}
