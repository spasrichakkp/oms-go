package orders

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"oms/internal/auth"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	defaultListLimit = 50
	maxListLimit     = 100
)

type handlerService interface {
	CreateOrder(ctx context.Context, input CreateOrderInput) (CreateOrderResult, error)
	GetOrder(ctx context.Context, orderID, customerID uuid.UUID) (OrderDetails, error)
	ListOrderDetails(ctx context.Context, input ListOrdersInput) ([]OrderDetails, error)
	CancelOrderDetails(ctx context.Context, input CancelOrderInput) (OrderDetails, error)
	UpdateOrderStatusByTarget(ctx context.Context, orderID uuid.UUID, newStatus OrderStatus, actorType string, actorID *uuid.UUID, reason, ipAddress *string) (OrderDetails, error)
}

type Handler struct {
	service handlerService
}

type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type orderItemResponse struct {
	ProductID      string `json:"product_id"`
	SKU            string `json:"sku"`
	Quantity       int32  `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type orderResponse struct {
	ID         string              `json:"id"`
	CustomerID string              `json:"customer_id"`
	Status     string              `json:"status"`
	Currency   string              `json:"currency"`
	TotalCents int64               `json:"total_cents"`
	Items      []orderItemResponse `json:"items"`
	CreatedAt  time.Time           `json:"created_at"`
	UpdatedAt  time.Time           `json:"updated_at"`
}

type orderListResponse struct {
	Orders     []orderResponse `json:"orders"`
	NextCursor *string         `json:"next_cursor"`
}

func NewHandler(service handlerService) *Handler {
	return &Handler{service: service}
}

func RegisterRoutes(r chi.Router, handler *Handler) {
	r.Post("/", handler.CreateOrder)
	r.Get("/", handler.ListOrders)
	r.Get("/{order_id}", handler.GetOrder)
	r.Post("/{order_id}/cancel", handler.CancelOrder)
	r.With(auth.RequireRole(auth.RoleAdmin, auth.RoleSystem)).Patch("/{order_id}/status", handler.UpdateOrderStatus)
}

func (h *Handler) CreateOrder(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "service unavailable")
		return
	}

	customerID, ok := auth.CustomerIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
		return
	}

	req, err := DecodeCreateOrderRequest(r.Body, DefaultRequestBodyLimit)
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	result, err := h.service.CreateOrder(r.Context(), CreateOrderInput{
		CustomerID:     customerID,
		IdempotencyKey: req.IdempotencyKey,
		Currency:       req.Currency,
		Items:          req.Items,
		ActorID:        uuidPointer(customerID),
		IPAddress:      requestIPAddress(r),
	})
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, mapOrderResponse(OrderDetails{
		Order: result.Order,
		Items: result.Items,
	}))
}

func (h *Handler) GetOrder(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "service unavailable")
		return
	}

	customerID, ok := auth.CustomerIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
		return
	}

	orderID, err := parseOrderID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	details, err := h.service.GetOrder(r.Context(), orderID, customerID)
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, mapOrderResponse(details))
}

func (h *Handler) ListOrders(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "service unavailable")
		return
	}

	customerID, ok := auth.CustomerIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
		return
	}

	limit, err := parseListLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	var status *OrderStatus
	if rawStatus := strings.TrimSpace(r.URL.Query().Get("status")); rawStatus != "" {
		parsedStatus := OrderStatus(rawStatus)
		if !IsValidStatus(parsedStatus) {
			writeJSONError(w, http.StatusBadRequest, "invalid_request", "status must be a valid OMS status")
			return
		}
		status = &parsedStatus
	}

	cursorCreatedAt, cursorID, err := parseCursor(r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	details, err := h.service.ListOrderDetails(r.Context(), ListOrdersInput{
		CustomerID:      customerID,
		Limit:           limit,
		Status:          status,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
	})
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	response := orderListResponse{
		Orders: make([]orderResponse, 0, len(details)),
	}
	for _, detail := range details {
		response.Orders = append(response.Orders, mapOrderResponse(detail))
	}

	if len(details) == int(limit) {
		nextCursor := encodeCursor(details[len(details)-1].Order.CreatedAt.Time, uuid.UUID(details[len(details)-1].Order.ID.Bytes))
		response.NextCursor = &nextCursor
	}

	writeJSON(w, http.StatusOK, response)
}

func (h *Handler) CancelOrder(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "service unavailable")
		return
	}

	customerID, ok := auth.CustomerIDFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
		return
	}

	orderID, err := parseOrderID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	details, err := h.service.CancelOrderDetails(r.Context(), CancelOrderInput{
		OrderID:    orderID,
		CustomerID: customerID,
		ActorID:    uuidPointer(customerID),
		IPAddress:  requestIPAddress(r),
	})
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, mapOrderResponse(details))
}

func (h *Handler) UpdateOrderStatus(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSONError(w, http.StatusInternalServerError, "internal_error", "service unavailable")
		return
	}

	orderID, err := parseOrderID(r)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	req, err := DecodeUpdateOrderStatusRequest(r.Body, DefaultRequestBodyLimit)
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	role, ok := auth.RoleFromContext(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "unauthorized", http.StatusText(http.StatusUnauthorized))
		return
	}

	identity, _ := auth.IdentityFromContext(r.Context())

	details, err := h.service.UpdateOrderStatusByTarget(
		r.Context(),
		orderID,
		req.Status,
		string(role),
		privilegedActorID(identity),
		nil,
		requestIPAddress(r),
	)
	if err != nil {
		writeHandlerError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, mapOrderResponse(details))
}

func parseOrderID(r *http.Request) (uuid.UUID, error) {
	orderID, err := uuid.Parse(chi.URLParam(r, "order_id"))
	if err != nil {
		return uuid.UUID{}, errors.New("order_id must be a valid UUID")
	}

	return orderID, nil
}

func parseListLimit(raw string) (int32, error) {
	if strings.TrimSpace(raw) == "" {
		return defaultListLimit, nil
	}

	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("limit must be an integer")
	}

	if value < 1 || value > maxListLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", maxListLimit)
	}

	return int32(value), nil
}

func parseCursor(raw string) (sql.NullTime, uuid.NullUUID, error) {
	if strings.TrimSpace(raw) == "" {
		return sql.NullTime{}, uuid.NullUUID{}, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return sql.NullTime{}, uuid.NullUUID{}, errors.New("cursor must be a valid opaque cursor")
	}

	parts := strings.Split(string(decoded), "|")
	if len(parts) != 2 {
		return sql.NullTime{}, uuid.NullUUID{}, errors.New("cursor must be a valid opaque cursor")
	}

	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return sql.NullTime{}, uuid.NullUUID{}, errors.New("cursor must be a valid opaque cursor")
	}

	orderID, err := uuid.Parse(parts[1])
	if err != nil {
		return sql.NullTime{}, uuid.NullUUID{}, errors.New("cursor must be a valid opaque cursor")
	}

	return sql.NullTime{
			Time:  createdAt,
			Valid: true,
		}, uuid.NullUUID{
			UUID:  orderID,
			Valid: true,
		}, nil
}

func encodeCursor(createdAt time.Time, orderID uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(createdAt.UTC().Format(time.RFC3339Nano) + "|" + orderID.String()))
}

func mapOrderResponse(detail OrderDetails) orderResponse {
	items := make([]orderItemResponse, 0, len(detail.Items))
	for _, item := range detail.Items {
		items = append(items, orderItemResponse{
			ProductID:      item.ProductID.String(),
			SKU:            item.Sku,
			Quantity:       item.Quantity,
			UnitPriceCents: item.UnitPriceCents,
		})
	}

	return orderResponse{
		ID:         detail.Order.ID.String(),
		CustomerID: detail.Order.CustomerID.String(),
		Status:     detail.Order.Status,
		Currency:   detail.Order.Currency,
		TotalCents: detail.Order.TotalCents,
		Items:      items,
		CreatedAt:  detail.Order.CreatedAt.Time,
		UpdatedAt:  detail.Order.UpdatedAt.Time,
	}
}

func writeHandlerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrRequestBodyTooLarge), errors.Is(err, ErrMalformedJSON), errors.Is(err, ErrUnknownJSONField), IsValidationError(err):
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
	case errors.Is(err, ErrOrderNotFound):
		writeJSONError(w, http.StatusNotFound, "not_found", err.Error())
	case errors.Is(err, ErrInvalidTransition):
		writeJSONError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrIdempotencyConflict):
		writeJSONError(w, http.StatusConflict, "conflict", "idempotency conflict")
	default:
		writeJSONError(w, http.StatusInternalServerError, "internal_error", http.StatusText(http.StatusInternalServerError))
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeJSONError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Error: errorBody{
			Code:    code,
			Message: message,
		},
	})
}

func uuidPointer(value uuid.UUID) *uuid.UUID {
	return &value
}

func privilegedActorID(identity auth.Identity) *uuid.UUID {
	switch identity.Role {
	case auth.RoleAdmin, auth.RoleSystem:
		if identity.HasSubjectID {
			return uuidPointer(identity.SubjectID)
		}
	default:
	}

	return nil
}

func requestIPAddress(r *http.Request) *string {
	remoteAddr := strings.TrimSpace(r.RemoteAddr)
	if remoteAddr == "" {
		return nil
	}

	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return parseIPAddress(host)
	}

	return parseIPAddress(remoteAddr)
}

func parseIPAddress(raw string) *string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return nil
	}

	value := addr.String()
	return &value
}
