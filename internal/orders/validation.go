package orders

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const DefaultRequestBodyLimit int64 = 1 << 20

var currencyCodePattern = regexp.MustCompile(`^[A-Z]{3}$`)

type FieldError struct {
	Field   string
	Message string
}

type ValidationErrors []FieldError

func (ve ValidationErrors) Error() string {
	parts := make([]string, 0, len(ve))
	for _, fieldErr := range ve {
		parts = append(parts, fmt.Sprintf("%s: %s", fieldErr.Field, fieldErr.Message))
	}

	return strings.Join(parts, "; ")
}

func (ve ValidationErrors) HasErrors() bool {
	return len(ve) > 0
}

type CreateOrderItemRequest struct {
	ProductID      string `json:"product_id"`
	SKU            string `json:"sku"`
	Quantity       int32  `json:"quantity"`
	UnitPriceCents int64  `json:"unit_price_cents"`
}

type CreateOrderRequest struct {
	IdempotencyKey string                   `json:"idempotency_key"`
	Currency       string                   `json:"currency"`
	Items          []CreateOrderItemRequest `json:"items"`
}

type UpdateOrderStatusRequest struct {
	Status OrderStatus `json:"status"`
}

func DecodeCreateOrderRequest(body io.Reader, maxBytes int64) (CreateOrderRequest, error) {
	var req CreateOrderRequest
	if err := decodeJSONStrict(body, maxBytes, &req); err != nil {
		return CreateOrderRequest{}, err
	}

	if err := ValidateCreateOrderRequest(req); err != nil {
		return CreateOrderRequest{}, err
	}

	return req, nil
}

func DecodeUpdateOrderStatusRequest(body io.Reader, maxBytes int64) (UpdateOrderStatusRequest, error) {
	var req UpdateOrderStatusRequest
	if err := decodeJSONStrict(body, maxBytes, &req); err != nil {
		return UpdateOrderStatusRequest{}, err
	}

	if err := ValidateUpdateOrderStatusRequest(req); err != nil {
		return UpdateOrderStatusRequest{}, err
	}

	return req, nil
}

func ValidateCreateOrderRequest(req CreateOrderRequest) error {
	var validationErrs ValidationErrors

	if strings.TrimSpace(req.IdempotencyKey) == "" {
		validationErrs = append(validationErrs, FieldError{Field: "idempotency_key", Message: "is required"})
	}

	if !currencyCodePattern.MatchString(req.Currency) {
		validationErrs = append(validationErrs, FieldError{Field: "currency", Message: "must be a 3-letter uppercase code"})
	}

	if len(req.Items) == 0 {
		validationErrs = append(validationErrs, FieldError{Field: "items", Message: "must contain at least one item"})
	}

	for i, item := range req.Items {
		prefix := fmt.Sprintf("items[%d]", i)

		if strings.TrimSpace(item.ProductID) == "" {
			validationErrs = append(validationErrs, FieldError{Field: prefix + ".product_id", Message: "is required"})
		}

		if strings.TrimSpace(item.SKU) == "" {
			validationErrs = append(validationErrs, FieldError{Field: prefix + ".sku", Message: "is required"})
		}

		if item.Quantity <= 0 {
			validationErrs = append(validationErrs, FieldError{Field: prefix + ".quantity", Message: "must be greater than 0"})
		}

		if item.UnitPriceCents < 0 {
			validationErrs = append(validationErrs, FieldError{Field: prefix + ".unit_price_cents", Message: "must be greater than or equal to 0"})
		}
	}

	if validationErrs.HasErrors() {
		return validationErrs
	}

	return nil
}

func ValidateUpdateOrderStatusRequest(req UpdateOrderStatusRequest) error {
	var validationErrs ValidationErrors

	if req.Status == "" {
		validationErrs = append(validationErrs, FieldError{Field: "status", Message: "is required"})
	} else if !IsValidStatus(req.Status) {
		validationErrs = append(validationErrs, FieldError{Field: "status", Message: "must be a valid OMS status"})
	}

	if validationErrs.HasErrors() {
		return validationErrs
	}

	return nil
}

func IsValidationError(err error) bool {
	var validationErrs ValidationErrors
	return errors.As(err, &validationErrs)
}
