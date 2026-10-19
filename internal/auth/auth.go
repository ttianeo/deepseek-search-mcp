// Package auth extracts the caller-supplied DeepSeek API key from the
// HTTP Authorization header.
//
// The MCP server never stores or issues tokens: every request must carry the
// caller's own DeepSeek API key as "Authorization: Bearer <key>", and that
// key is forwarded to the DeepSeek API on behalf of the caller.
package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

const maxTokenLength = 4096

var (
	// ErrMissing reports that no Authorization header was supplied.
	ErrMissing = errors.New("missing Authorization header")
	// ErrMalformed reports an Authorization header that is not a valid
	// "Bearer <token>" credential.
	ErrMalformed = errors.New("invalid Authorization header")
)

// ParseBearer parses an Authorization header value of the form
// "Bearer <token>". The scheme is matched case-insensitively.
func ParseBearer(header string) (string, error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return "", ErrMissing
	}

	scheme, token, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", ErrMalformed
	}
	token = strings.TrimSpace(token)
	if token == "" || len(token) > maxTokenLength {
		return "", ErrMalformed
	}
	for _, r := range token {
		// API keys are printable ASCII. Rejecting control characters,
		// whitespace, commas, quotes and backslashes keeps malformed or
		// credential-list headers from being forwarded upstream.
		if r < 0x21 || r > 0x7e || r == ',' || r == '"' || r == '\\' {
			return "", ErrMalformed
		}
	}
	return token, nil
}

// FromHeader extracts and validates the bearer token from h.
//
// Multiple Authorization headers are rejected rather than guessing which one
// the caller intended.
func FromHeader(h http.Header) (string, error) {
	values := h.Values("Authorization")
	if len(values) == 0 {
		return "", ErrMissing
	}
	if len(values) > 1 {
		return "", ErrMalformed
	}
	return ParseBearer(values[0])
}

type tokenContextKey struct{}

// WithToken stores token in ctx. It lets HTTP middleware hand the token to the
// MCP tool handlers without re-parsing the request.
func WithToken(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, tokenContextKey{}, token)
}

// FromContext returns the token stored by WithToken.
func FromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(tokenContextKey{}).(string)
	return token, ok && token != ""
}
