package orders

import (
	"errors"
	"testing"
)

func TestStateMachineCanTransitionAllowed(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		from OrderStatus
		to   OrderStatus
	}{
		{name: "pending to processing", from: StatusPending, to: StatusProcessing},
		{name: "processing to shipped", from: StatusProcessing, to: StatusShipped},
		{name: "shipped to delivered", from: StatusShipped, to: StatusDelivered},
		{name: "pending to cancelled", from: StatusPending, to: StatusCancelled},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if !CanTransition(tc.from, tc.to) {
				t.Fatalf("expected transition %s -> %s to be allowed", tc.from, tc.to)
			}

			if err := ValidateTransition(tc.from, tc.to); err != nil {
				t.Fatalf("expected transition %s -> %s to validate: %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestStateMachineCanTransitionForbidden(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		from OrderStatus
		to   OrderStatus
	}{
		{name: "processing to cancelled", from: StatusProcessing, to: StatusCancelled},
		{name: "shipped to cancelled", from: StatusShipped, to: StatusCancelled},
		{name: "delivered to cancelled", from: StatusDelivered, to: StatusCancelled},
		{name: "cancelled to processing", from: StatusCancelled, to: StatusProcessing},
		{name: "delivered to pending", from: StatusDelivered, to: StatusPending},
		{name: "cancelled to pending", from: StatusCancelled, to: StatusPending},
		{name: "shipped to processing", from: StatusShipped, to: StatusProcessing},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if CanTransition(tc.from, tc.to) {
				t.Fatalf("expected transition %s -> %s to be forbidden", tc.from, tc.to)
			}

			err := ValidateTransition(tc.from, tc.to)
			if !errors.Is(err, ErrInvalidTransition) {
				t.Fatalf("expected invalid transition error for %s -> %s, got %v", tc.from, tc.to, err)
			}
		})
	}
}

func TestStateMachineRejectsUnknownStatuses(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name string
		from OrderStatus
		to   OrderStatus
	}{
		{name: "unknown from status", from: OrderStatus("UNKNOWN"), to: StatusPending},
		{name: "unknown to status", from: StatusPending, to: OrderStatus("UNKNOWN")},
		{name: "both statuses unknown", from: OrderStatus("OLD"), to: OrderStatus("NEW")},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if CanTransition(tc.from, tc.to) {
				t.Fatalf("expected unknown status transition %s -> %s to be rejected", tc.from, tc.to)
			}

			err := ValidateTransition(tc.from, tc.to)
			if !errors.Is(err, ErrUnknownStatus) {
				t.Fatalf("expected unknown status error for %s -> %s, got %v", tc.from, tc.to, err)
			}
		})
	}
}
