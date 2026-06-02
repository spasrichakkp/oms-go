package orders

import (
	"errors"
	"strings"
	"testing"
)

func TestValidationDecodeCreateOrderRequestValid(t *testing.T) {
	t.Parallel()

	req, err := DecodeCreateOrderRequest(strings.NewReader(`{
		"idempotency_key":"idem-123",
		"currency":"USD",
		"items":[{"product_id":"prod-1","sku":"sku-1","quantity":2,"unit_price_cents":500}]
	}`), DefaultRequestBodyLimit)
	if err != nil {
		t.Fatalf("expected valid create order payload, got %v", err)
	}

	if req.IdempotencyKey != "idem-123" {
		t.Fatalf("expected idempotency key to decode")
	}
}

func TestValidationDecodeCreateOrderRequestRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		body string
	}{
		{
			name: "customer id rejected",
			body: `{"idempotency_key":"idem-123","currency":"USD","customer_id":"cust-1","items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":500}]}`,
		},
		{
			name: "total cents rejected",
			body: `{"idempotency_key":"idem-123","currency":"USD","total_cents":500,"items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":500}]}`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeCreateOrderRequest(strings.NewReader(tc.body), DefaultRequestBodyLimit)
			if !errors.Is(err, ErrUnknownJSONField) {
				t.Fatalf("expected unknown field error, got %v", err)
			}
		})
	}
}

func TestValidationDecodeCreateOrderRequestRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	_, err := DecodeCreateOrderRequest(strings.NewReader(`{"idempotency_key":"idem-123","currency":"USD","items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":500}]}`), 16)
	if !errors.Is(err, ErrRequestBodyTooLarge) {
		t.Fatalf("expected request body too large error, got %v", err)
	}
}

func TestValidationCreateOrderInvalidPayloads(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		body string
	}{
		{
			name: "missing idempotency key",
			body: `{"currency":"USD","items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":500}]}`,
		},
		{
			name: "bad currency",
			body: `{"idempotency_key":"idem-123","currency":"usd","items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":500}]}`,
		},
		{
			name: "empty items",
			body: `{"idempotency_key":"idem-123","currency":"USD","items":[]}`,
		},
		{
			name: "missing product id",
			body: `{"idempotency_key":"idem-123","currency":"USD","items":[{"sku":"sku-1","quantity":1,"unit_price_cents":500}]}`,
		},
		{
			name: "missing sku",
			body: `{"idempotency_key":"idem-123","currency":"USD","items":[{"product_id":"prod-1","quantity":1,"unit_price_cents":500}]}`,
		},
		{
			name: "bad quantity",
			body: `{"idempotency_key":"idem-123","currency":"USD","items":[{"product_id":"prod-1","sku":"sku-1","quantity":0,"unit_price_cents":500}]}`,
		},
		{
			name: "bad unit price",
			body: `{"idempotency_key":"idem-123","currency":"USD","items":[{"product_id":"prod-1","sku":"sku-1","quantity":1,"unit_price_cents":-1}]}`,
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeCreateOrderRequest(strings.NewReader(tc.body), DefaultRequestBodyLimit)
			if !IsValidationError(err) {
				t.Fatalf("expected validation error, got %v", err)
			}
		})
	}
}

func TestValidationDecodeUpdateOrderStatusRequest(t *testing.T) {
	t.Parallel()

	req, err := DecodeUpdateOrderStatusRequest(strings.NewReader(`{"status":"SHIPPED"}`), DefaultRequestBodyLimit)
	if err != nil {
		t.Fatalf("expected valid status update payload, got %v", err)
	}

	if req.Status != StatusShipped {
		t.Fatalf("expected decoded status to match")
	}
}

func TestValidationDecodeUpdateOrderStatusRequestRejectsInvalidPayloads(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name  string
		body  string
		check func(error) bool
	}{
		{
			name:  "missing status",
			body:  `{}`,
			check: IsValidationError,
		},
		{
			name:  "invalid status",
			body:  `{"status":"PACKED"}`,
			check: IsValidationError,
		},
		{
			name:  "unknown field",
			body:  `{"status":"SHIPPED","customer_id":"cust-1"}`,
			check: func(err error) bool { return errors.Is(err, ErrUnknownJSONField) },
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := DecodeUpdateOrderStatusRequest(strings.NewReader(tc.body), DefaultRequestBodyLimit)
			if !tc.check(err) {
				t.Fatalf("unexpected error result: %v", err)
			}
		})
	}
}
