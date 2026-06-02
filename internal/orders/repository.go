package orders

import (
	"context"
	"database/sql"
	"net/netip"

	db "oms/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	actorTypeCustomer = "CUSTOMER"
	actorTypeSystem   = "SYSTEM"

	actionCreated                 = "CREATED"
	actionCancelled               = "CANCELLED"
	actionStatusUpdated           = "STATUS_UPDATED"
	actionWorkerMovedToProcessing = "WORKER_MOVED_TO_PROCESSING"
)

type Repository struct {
	db      *pgxpool.Pool
	queries *db.Queries
}

type CreateOrderItemInput struct {
	ID             uuid.UUID
	ProductID      uuid.UUID
	SKU            string
	Quantity       int32
	UnitPriceCents int64
}

type CreateOrderCommand struct {
	OrderID        uuid.UUID
	CustomerID     uuid.UUID
	IdempotencyKey string
	TotalCents     int64
	Currency       string
	Items          []CreateOrderItemInput
	ActorID        *uuid.UUID
	Reason         *string
	IPAddress      *string
}

type CreateOrderResult struct {
	Order db.Order
	Items []db.OrderItem
}

type CancelOrderCommand struct {
	OrderID    uuid.UUID
	CustomerID uuid.UUID
	ActorID    *uuid.UUID
	Reason     *string
	IPAddress  *string
}

type UpdateOrderStatusCommand struct {
	OrderID        uuid.UUID
	ExpectedStatus OrderStatus
	NewStatus      OrderStatus
	ActorType      string
	ActorID        *uuid.UUID
	Reason         *string
	IPAddress      *string
}

type ProcessPendingOrdersBatchCommand struct {
	Limit     int32
	ActorID   *uuid.UUID
	Reason    *string
	IPAddress *string
}

type ListOrdersCommand struct {
	CustomerID      uuid.UUID
	Limit           int32
	Status          *OrderStatus
	CursorCreatedAt sql.NullTime
	CursorID        uuid.NullUUID
}

func NewRepository(database *pgxpool.Pool) *Repository {
	return &Repository{
		db:      database,
		queries: db.New(database),
	}
}

func (r *Repository) GetOrderByIDAndCustomerID(ctx context.Context, orderID, customerID uuid.UUID) (db.Order, error) {
	return r.queries.GetOrderByIDAndCustomerID(ctx, db.GetOrderByIDAndCustomerIDParams{
		ID:         toPgUUID(orderID),
		CustomerID: toPgUUID(customerID),
	})
}

func (r *Repository) ListOrders(ctx context.Context, cmd ListOrdersCommand) ([]db.Order, error) {
	if cmd.Status == nil || *cmd.Status == "" {
		return r.queries.ListOrdersByCustomerID(ctx, db.ListOrdersByCustomerIDParams{
			CustomerID:      toPgUUID(cmd.CustomerID),
			Limit:           cmd.Limit,
			CursorCreatedAt: toPgTimestamptz(cmd.CursorCreatedAt),
			CursorID:        toPgNullUUID(cmd.CursorID),
		})
	}

	return r.queries.ListOrdersByCustomerIDAndStatus(ctx, db.ListOrdersByCustomerIDAndStatusParams{
		CustomerID:      toPgUUID(cmd.CustomerID),
		Status:          string(*cmd.Status),
		Limit:           cmd.Limit,
		CursorCreatedAt: toPgTimestamptz(cmd.CursorCreatedAt),
		CursorID:        toPgNullUUID(cmd.CursorID),
	})
}

func (r *Repository) GetOrderItemsByOrderID(ctx context.Context, orderID uuid.UUID) ([]db.OrderItem, error) {
	return r.queries.GetOrderItemsByOrderID(ctx, toPgUUID(orderID))
}

func (r *Repository) GetOrderEventsByOrderID(ctx context.Context, orderID uuid.UUID) ([]db.OrderEvent, error) {
	return r.queries.GetOrderEventsByOrderID(ctx, toPgUUID(orderID))
}

func (r *Repository) CreateOrderWithItemsAndAudit(ctx context.Context, cmd CreateOrderCommand) (CreateOrderResult, error) {
	var result CreateOrderResult

	err := r.withTx(ctx, func(queries *db.Queries) error {
		createdOrder, err := queries.CreateOrder(ctx, db.CreateOrderParams{
			ID:             toPgUUID(cmd.OrderID),
			CustomerID:     toPgUUID(cmd.CustomerID),
			Status:         string(StatusPending),
			IdempotencyKey: cmd.IdempotencyKey,
			TotalCents:     cmd.TotalCents,
			Currency:       cmd.Currency,
		})
		if err != nil {
			return err
		}

		createdItems := make([]db.OrderItem, 0, len(cmd.Items))
		for _, item := range cmd.Items {
			createdItem, err := queries.CreateOrderItem(ctx, db.CreateOrderItemParams{
				ID:             toPgUUID(item.ID),
				OrderID:        createdOrder.ID,
				ProductID:      toPgUUID(item.ProductID),
				Sku:            item.SKU,
				Quantity:       item.Quantity,
				UnitPriceCents: item.UnitPriceCents,
			})
			if err != nil {
				return err
			}

			createdItems = append(createdItems, createdItem)
		}

		if _, err := queries.CreateOrderEvent(ctx, db.CreateOrderEventParams{
			ID:         toPgUUID(uuid.New()),
			OrderID:    createdOrder.ID,
			ActorType:  actorTypeCustomer,
			ActorID:    nullUUID(cmd.ActorID),
			FromStatus: nullStatus(""),
			ToStatus:   nullStatus(StatusPending),
			Action:     actionCreated,
			Reason:     nullString(cmd.Reason),
			IpAddress:  inetValue(cmd.IPAddress),
		}); err != nil {
			return err
		}

		result = CreateOrderResult{
			Order: createdOrder,
			Items: createdItems,
		}

		return nil
	})
	if err != nil {
		return CreateOrderResult{}, err
	}

	return result, nil
}

func (r *Repository) CancelOrderWithAudit(ctx context.Context, cmd CancelOrderCommand) (db.Order, error) {
	var updatedOrder db.Order

	err := r.withTx(ctx, func(queries *db.Queries) error {
		cancelledOrder, err := queries.CancelPendingOrder(ctx, db.CancelPendingOrderParams{
			ID:         toPgUUID(cmd.OrderID),
			CustomerID: toPgUUID(cmd.CustomerID),
		})
		if err != nil {
			return err
		}

		if _, err := queries.CreateOrderEvent(ctx, db.CreateOrderEventParams{
			ID:         toPgUUID(uuid.New()),
			OrderID:    cancelledOrder.ID,
			ActorType:  actorTypeCustomer,
			ActorID:    nullUUID(cmd.ActorID),
			FromStatus: nullStatus(StatusPending),
			ToStatus:   nullStatus(StatusCancelled),
			Action:     actionCancelled,
			Reason:     nullString(cmd.Reason),
			IpAddress:  inetValue(cmd.IPAddress),
		}); err != nil {
			return err
		}

		updatedOrder = cancelledOrder
		return nil
	})
	if err != nil {
		return db.Order{}, err
	}

	return updatedOrder, nil
}

func (r *Repository) UpdateOrderStatusWithAudit(ctx context.Context, cmd UpdateOrderStatusCommand) (db.Order, error) {
	var updatedOrder db.Order

	err := r.withTx(ctx, func(queries *db.Queries) error {
		order, err := queries.UpdateOrderStatusIfCurrentStatusMatches(ctx, db.UpdateOrderStatusIfCurrentStatusMatchesParams{
			ID:       toPgUUID(cmd.OrderID),
			Status:   string(cmd.ExpectedStatus),
			Status_2: string(cmd.NewStatus),
		})
		if err != nil {
			return err
		}

		if _, err := queries.CreateOrderEvent(ctx, db.CreateOrderEventParams{
			ID:         toPgUUID(uuid.New()),
			OrderID:    order.ID,
			ActorType:  cmd.ActorType,
			ActorID:    nullUUID(cmd.ActorID),
			FromStatus: nullStatus(cmd.ExpectedStatus),
			ToStatus:   nullStatus(cmd.NewStatus),
			Action:     actionStatusUpdated,
			Reason:     nullString(cmd.Reason),
			IpAddress:  inetValue(cmd.IPAddress),
		}); err != nil {
			return err
		}

		updatedOrder = order
		return nil
	})
	if err != nil {
		return db.Order{}, err
	}

	return updatedOrder, nil
}

func (r *Repository) ProcessPendingOrdersBatchWithAudit(ctx context.Context, cmd ProcessPendingOrdersBatchCommand) ([]db.Order, error) {
	var updatedOrders []db.Order

	err := r.withTx(ctx, func(queries *db.Queries) error {
		ordersToProcess, err := queries.WorkerPickAndMovePendingOrdersToProcessing(ctx, cmd.Limit)
		if err != nil {
			return err
		}

		for _, order := range ordersToProcess {
			if _, err := queries.CreateOrderEvent(ctx, db.CreateOrderEventParams{
				ID:         toPgUUID(uuid.New()),
				OrderID:    order.ID,
				ActorType:  actorTypeSystem,
				ActorID:    nullUUID(cmd.ActorID),
				FromStatus: nullStatus(StatusPending),
				ToStatus:   nullStatus(StatusProcessing),
				Action:     actionWorkerMovedToProcessing,
				Reason:     nullString(cmd.Reason),
				IpAddress:  inetValue(cmd.IPAddress),
			}); err != nil {
				return err
			}
		}

		updatedOrders = ordersToProcess
		return nil
	})
	if err != nil {
		return nil, err
	}

	return updatedOrders, nil
}

func (r *Repository) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}

	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(ctx)
		}
	}()

	if err := fn(r.queries.WithTx(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	committed = true
	return nil
}

func toPgUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{
		Bytes: [16]byte(id),
		Valid: true,
	}
}

func toPgNullUUID(id uuid.NullUUID) pgtype.UUID {
	if !id.Valid {
		return pgtype.UUID{}
	}

	return toPgUUID(id.UUID)
}

func toPgTimestamptz(value sql.NullTime) pgtype.Timestamptz {
	if !value.Valid {
		return pgtype.Timestamptz{}
	}

	return pgtype.Timestamptz{
		Time:  value.Time,
		Valid: true,
	}
}

func nullUUID(id *uuid.UUID) pgtype.UUID {
	if id == nil {
		return pgtype.UUID{}
	}

	return toPgUUID(*id)
}

func nullStatus(status OrderStatus) pgtype.Text {
	if status == "" {
		return pgtype.Text{}
	}

	return pgtype.Text{
		String: string(status),
		Valid:  true,
	}
}

func nullString(value *string) pgtype.Text {
	if value == nil {
		return pgtype.Text{}
	}

	return pgtype.Text{
		String: *value,
		Valid:  true,
	}
}

func inetValue(value *string) *netip.Addr {
	if value == nil || *value == "" {
		return nil
	}

	addr, err := netip.ParseAddr(*value)
	if err != nil {
		return nil
	}

	return &addr
}
