package platform

import (
	"errors"
	"strings"
	"testing"
)

func TestDecodeJSONStrictRejectsUnknownFields(t *testing.T) {
	t.Parallel()

	var dst struct {
		Name string `json:"name"`
	}

	err := DecodeJSONStrict(strings.NewReader(`{"name":"oms","extra":"nope"}`), 1024, &dst)
	if !errors.Is(err, ErrUnknownJSONField) {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDecodeJSONStrictRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	var dst struct {
		Name string `json:"name"`
	}

	err := DecodeJSONStrict(strings.NewReader(`{"name":"too-large"}`), 4, &dst)
	if !errors.Is(err, ErrRequestBodyTooLarge) {
		t.Fatalf("expected oversized body error, got %v", err)
	}
}

func TestDecodeJSONStrictRejectsTrailingJSON(t *testing.T) {
	t.Parallel()

	var dst struct {
		Name string `json:"name"`
	}

	err := DecodeJSONStrict(strings.NewReader(`{"name":"oms"}{"next":"value"}`), 1024, &dst)
	if !errors.Is(err, ErrMalformedJSON) {
		t.Fatalf("expected malformed json error for trailing value, got %v", err)
	}
}
