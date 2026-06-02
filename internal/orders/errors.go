package orders

import "errors"

var (
	ErrUnknownStatus       = errors.New("unknown order status")
	ErrInvalidTransition   = errors.New("invalid order status transition")
	ErrOrderNotFound       = errors.New("order not found")
	ErrIdempotencyConflict = errors.New("idempotency conflict")
	ErrInvalidActorType    = errors.New("invalid actor type")
	ErrTotalCentsOverflow  = errors.New("total cents overflow")
)
