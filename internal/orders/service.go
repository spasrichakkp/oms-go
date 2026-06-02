package orders

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	db "oms/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5"
)

const (
	ActorTypeCustomer = "CUSTOMER"
	ActorTypeAdmin    = "ADMIN"
	ActorTypeSystem   = "SYSTEM"

	idempotencyConstraintName = "orders_customer_id_idempotency_key_key"
	uniqueViolationCode       = "23505"
)

type serviceRepository interface {
	CreateOrderWithItemsAndAudit(ctx context.Context, cmd CreateOrderCommand) (CreateOrderResult, error)
	GetOrderByIDAndCustomerID(ctx context.Context, orderID, customerID uuid.UUID) (db.Order, error)
	GetOrderItemsByOrderID(ctx context.Context, orderID uuid.UUID) ([]db.OrderItem, error)
	GetOrderEventsByOrderID(ctx context.Context, orderID uuid.UUID) ([]db.OrderEvent, error)
	ListOrders(ctx context.Context, cmd ListOrdersCommand) ([]db.Order, error)
	CancelOrderWithAudit(ctx context.Context, cmd CancelOrderCommand) (db.Order, error)
	UpdateOrderStatusWithAudit(ctx context.Context, cmd UpdateOrderStatusCommand) (db.Order, error)
	ProcessPendingOrdersBatchWithAudit(ctx context.Context, cmd ProcessPendingOrdersBatchCommand) ([]db.Order, error)
}

type Service struct {
	repo    serviceRepository
	newUUID func() uuid.UUID
}

type OrderDetails struct {
	Order db.Order
	Items []db.OrderItem
}

type CreateOrderInput struct {
	CustomerID     uuid.UUID
	IdempotencyKey string
	Currency       string
	Items          []CreateOrderItemRequest
	ActorID        *uuid.UUID
	Reason         *string
	IPAddress      *string
}

type ListOrdersInput struct {
	CustomerID      uuid.UUID
	Limit           int32
	Status          *OrderStatus
	CursorCreatedAt sql.NullTime
	CursorID        uuid.NullUUID
}

type CancelOrderInput struct {
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	ActorID    *uuid.UUID
	Reason     *string
	IPAddress  *string
}

type UpdateOrderStatusInput struct {
	OrderID        uuid.UUID
	ExpectedStatus OrderStatus
	NewStatus      OrderStatus
	ActorType      string
	ActorID        *uuid.UUID
	Reason         *string
	IPAddress      *string
}

type ProcessPendingOrdersBatchInput struct {
	Limit     int32
	ActorID   *uuid.UUID
	Reason    *string
	IPAddress *string
}

func NewService(repo serviceRepository) *Service {
	return &Service{
		repo:    repo,
		newUUID: uuid.New,
	}
}

func (s *Service) CreateOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error) {
	if err := ValidateCreateOrderRequest(CreateOrderRequest{
		IdempotencyKey: input.IdempotencyKey,
		Currency:       input.Currency,
		Items:          input.Items,
	}); err != nil {
		return CreateOrderResult{}, err
	}

	totalCents, err := calculateTotalCents(input.Items)
	if err != nil {
		return CreateOrderResult{}, err
	}

	cmdItems := make([]CreateOrderItemInput, 0, len(input.Items))
	for index, item := range input.Items {
		productID, err := uuid.Parse(item.ProductID)
		if err != nil {
			return CreateOrderResult{}, ValidationErrors{{
				Field:   fmt.Sprintf("items[%d].product_id", index),
				Message: "must be a valid UUID",
			}}
		}

		cmdItems = append(cmdItems, CreateOrderItemInput{
			ID:             s.newUUID(),
			ProductID:      productID,
			SKU:            item.SKU,
			Quantity:       item.Quantity,
			UnitPriceCents: item.UnitPriceCents,
		})
	}

	result, err := s.repo.CreateOrderWithItemsAndAudit(ctx, CreateOrderCommand{
		OrderID:        s.newUUID(),
		CustomerID:     input.CustomerID,
		IdempotencyKey: input.IdempotencyKey,
		TotalCents:     totalCents,
		Currency:       input.Currency,
		Items:          cmdItems,
		ActorID:        input.ActorID,
		Reason:         input.Reason,
		IPAddress:      input.IPAddress,
	})
	if err != nil {
		if isIdempotencyConflictError(err) {
			return CreateOrderResult{}, ErrIdempotencyConflict
		}

		return CreateOrderResult{}, err
	}

	return result, nil
}

func (s *Service) GetOrder(ctx context.Context, orderID, customerID uuid.UUID) (OrderDetails, error) {
	order, err := s.repo.GetOrderByIDAndCustomerID(ctx, orderID, customerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return OrderDetails{}, ErrOrderNotFound
		}

		return OrderDetails{}, err
	}

	items, err := s.repo.GetOrderItemsByOrderID(ctx, orderID)
	if err != nil {
		return OrderDetails{}, err
	}

	return OrderDetails{Order: order, Items: items}, nil
}

func (s *Service) ListOrders(ctx context.Context, input ListOrdersInput) ([]db.Order, error) {
	return s.repo.ListOrders(ctx, ListOrdersCommand{
		CustomerID:      input.CustomerID,
		Limit:           input.Limit,
		Status:          input.Status,
		CursorCreatedAt: input.CursorCreatedAt,
		CursorID:        input.CursorID,
	})
}

func (s *Service) ListOrderDetails(ctx context.Context, input ListOrdersInput) ([]OrderDetails, error) {
	ordersList, err := s.ListOrders(ctx, input)
	if err != nil {
		return nil, err
	}

	return s.hydrateOrderDetails(ctx, ordersList)
}

func (s *Service) CancelOrder(ctx context.Context, input CancelOrderInput) (db.Order, error) {
	order, err := s.repo.GetOrderByIDAndCustomerID(ctx, input.OrderID, input.CustomerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Order{}, ErrOrderNotFound
		}

		return db.Order{}, err
	}

	if err := ValidateTransition(OrderStatus(order.Status), StatusCancelled); err != nil {
		return db.Order{}, err
	}

	return s.repo.CancelOrderWithAudit(ctx, CancelOrderCommand{
		OrderID:    input.OrderID,
		CustomerID: input.CustomerID,
		ActorID:    input.ActorID,
		Reason:     input.Reason,
		IPAddress:  input.IPAddress,
	})
}

func (s *Service) CancelOrderDetails(ctx context.Context, input CancelOrderInput) (OrderDetails, error) {
	order, err := s.CancelOrder(ctx, input)
	if err != nil {
		return OrderDetails{}, err
	}

	items, err := s.repo.GetOrderItemsByOrderID(ctx, uuid.UUID(order.ID.Bytes))
	if err != nil {
		return OrderDetails{}, err
	}

	return OrderDetails{
		Order: order,
		Items: items,
	}, nil
}

func (s *Service) UpdateOrderStatus(ctx context.Context, input UpdateOrderStatusInput) (db.Order, error) {
	if !isAllowedActorType(input.ActorType, ActorTypeAdmin, ActorTypeSystem) {
		return db.Order{}, ErrInvalidActorType
	}

	if err := ValidateTransition(input.ExpectedStatus, input.NewStatus); err != nil {
		return db.Order{}, err
	}

	order, err := s.repo.UpdateOrderStatusWithAudit(ctx, UpdateOrderStatusCommand{
		OrderID:        input.OrderID,
		ExpectedStatus: input.ExpectedStatus,
		NewStatus:      input.NewStatus,
		ActorType:      input.ActorType,
		ActorID:        input.ActorID,
		Reason:         input.Reason,
		IPAddress:      input.IPAddress,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.Order{}, s.resolveStatusUpdateNoRows(ctx, input.OrderID)
		}

		return db.Order{}, err
	}

	return order, nil
}

func (s *Service) UpdateOrderStatusDetails(ctx context.Context, input UpdateOrderStatusInput) (OrderDetails, error) {
	order, err := s.UpdateOrderStatus(ctx, input)
	if err != nil {
		return OrderDetails{}, err
	}

	items, err := s.repo.GetOrderItemsByOrderID(ctx, uuid.UUID(order.ID.Bytes))
	if err != nil {
		return OrderDetails{}, err
	}

	return OrderDetails{
		Order: order,
		Items: items,
	}, nil
}

func (s *Service) UpdateOrderStatusByTarget(ctx context.Context, orderID uuid.UUID, newStatus OrderStatus, actorType string, actorID *uuid.UUID, reason, ipAddress *string) (OrderDetails, error) {
	expectedStatus, err := expectedStatusForTarget(newStatus)
	if err != nil {
		return OrderDetails{}, err
	}

	return s.UpdateOrderStatusDetails(ctx, UpdateOrderStatusInput{
		OrderID:        orderID,
		ExpectedStatus: expectedStatus,
		NewStatus:      newStatus,
		ActorType:      actorType,
		ActorID:        actorID,
		Reason:         reason,
		IPAddress:      ipAddress,
	})
}

func (s *Service) ProcessPendingOrdersBatch(ctx context.Context, input ProcessPendingOrdersBatchInput) ([]db.Order, error) {
	if err := ValidateTransition(StatusPending, StatusProcessing); err != nil {
		return nil, err
	}

	return s.repo.ProcessPendingOrdersBatchWithAudit(ctx, ProcessPendingOrdersBatchCommand{
		Limit:     input.Limit,
		ActorID:   input.ActorID,
		Reason:    input.Reason,
		IPAddress: input.IPAddress,
	})
}

func calculateTotalCents(items []CreateOrderItemRequest) (int64, error) {
	var total int64

	for _, item := range items {
		lineTotal, err := multiplyCents(item.UnitPriceCents, int64(item.Quantity))
		if err != nil {
			return 0, err
		}

		nextTotal := total + lineTotal
		if (lineTotal > 0 && nextTotal < total) || (lineTotal < 0 && nextTotal > total) {
			return 0, ErrTotalCentsOverflow
		}

		total = nextTotal
	}

	return total, nil
}

func multiplyCents(price, quantity int64) (int64, error) {
	if price == 0 || quantity == 0 {
		return 0, nil
	}

	const maxInt64 = int64(^uint64(0) >> 1)
	if price > 0 && quantity > 0 && price > maxInt64/quantity {
		return 0, ErrTotalCentsOverflow
	}

	return price * quantity, nil
}

func isIdempotencyConflictError(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}

	return pgErr.Code == uniqueViolationCode && pgErr.ConstraintName == idempotencyConstraintName
}

func isAllowedActorType(actorType string, allowed ...string) bool {
	for _, candidate := range allowed {
		if actorType == candidate {
			return true
		}
	}

	return false
}

func (s *Service) hydrateOrderDetails(ctx context.Context, ordersList []db.Order) ([]OrderDetails, error) {
	details := make([]OrderDetails, 0, len(ordersList))
	for _, order := range ordersList {
		items, err := s.repo.GetOrderItemsByOrderID(ctx, uuid.UUID(order.ID.Bytes))
		if err != nil {
			return nil, err
		}

		details = append(details, OrderDetails{
			Order: order,
			Items: items,
		})
	}

	return details, nil
}

func (s *Service) resolveStatusUpdateNoRows(ctx context.Context, orderID uuid.UUID) error {
	events, err := s.repo.GetOrderEventsByOrderID(ctx, orderID)
	if err != nil {
		return err
	}

	if len(events) == 0 {
		return ErrOrderNotFound
	}

	return ErrInvalidTransition
}

func expectedStatusForTarget(target OrderStatus) (OrderStatus, error) {
	switch target {
	case StatusProcessing:
		return StatusPending, nil
	case StatusShipped:
		return StatusProcessing, nil
	case StatusDelivered:
		return StatusShipped, nil
	case StatusCancelled:
		return StatusPending, nil
	default:
		return "", ValidateTransition(StatusPending, target)
	}
}
