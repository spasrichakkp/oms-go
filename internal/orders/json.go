package orders

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

var (
	ErrRequestBodyTooLarge = errors.New("request body too large")
	ErrMalformedJSON       = errors.New("malformed json")
	ErrUnknownJSONField    = errors.New("unknown json field")
)

func decodeJSONStrict(r io.Reader, maxBytes int64, dst any) error {
	payload, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read request body: %w", err)
	}

	if int64(len(payload)) > maxBytes {
		return ErrRequestBodyTooLarge
	}

	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return normalizeJSONDecodeError(err)
	}

	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%w: multiple JSON values", ErrMalformedJSON)
		}

		return fmt.Errorf("%w: multiple JSON values", ErrMalformedJSON)
	}

	return nil
}

func normalizeJSONDecodeError(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError

	switch {
	case errors.Is(err, io.EOF):
		return fmt.Errorf("%w: empty body", ErrMalformedJSON)
	case errors.As(err, &syntaxErr):
		return fmt.Errorf("%w: syntax error at byte %d", ErrMalformedJSON, syntaxErr.Offset)
	case errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: unexpected EOF", ErrMalformedJSON)
	case errors.As(err, &typeErr):
		if typeErr.Field != "" {
			return fmt.Errorf("%w: invalid value for field %q", ErrMalformedJSON, typeErr.Field)
		}

		return fmt.Errorf("%w: invalid value at byte %d", ErrMalformedJSON, typeErr.Offset)
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		field := strings.TrimPrefix(err.Error(), "json: unknown field ")
		return fmt.Errorf("%w %s", ErrUnknownJSONField, field)
	default:
		return fmt.Errorf("%w: %v", ErrMalformedJSON, err)
	}
}
