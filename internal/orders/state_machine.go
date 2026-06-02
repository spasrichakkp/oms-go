package orders

import "fmt"

type OrderStatus string

const (
	StatusPending    OrderStatus = "PENDING"
	StatusProcessing OrderStatus = "PROCESSING"
	StatusShipped    OrderStatus = "SHIPPED"
	StatusDelivered  OrderStatus = "DELIVERED"
	StatusCancelled  OrderStatus = "CANCELLED"
)

var validStatuses = map[OrderStatus]struct{}{
	StatusPending:    {},
	StatusProcessing: {},
	StatusShipped:    {},
	StatusDelivered:  {},
	StatusCancelled:  {},
}

var allowedTransitions = map[OrderStatus]map[OrderStatus]struct{}{
	StatusPending: {
		StatusProcessing: {},
		StatusCancelled:  {},
	},
	StatusProcessing: {
		StatusShipped: {},
	},
	StatusShipped: {
		StatusDelivered: {},
	},
}

func CanTransition(from, to OrderStatus) bool {
	if !IsValidStatus(from) || !IsValidStatus(to) {
		return false
	}

	nextStatuses, ok := allowedTransitions[from]
	if !ok {
		return false
	}

	_, ok = nextStatuses[to]
	return ok
}

func ValidateTransition(from, to OrderStatus) error {
	if !IsValidStatus(from) {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, from)
	}

	if !IsValidStatus(to) {
		return fmt.Errorf("%w: %q", ErrUnknownStatus, to)
	}

	if !CanTransition(from, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}

	return nil
}

func IsValidStatus(status OrderStatus) bool {
	_, ok := validStatuses[status]
	return ok
}
