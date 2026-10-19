package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestParseBearer(t *testing.T) {
	tests := []struct {
		name    string
		header  string
		want    string
		wantErr error
	}{
		{name: "valid", header: "Bearer sk-abc123", want: "sk-abc123"},
		{name: "lowercase scheme", header: "bearer sk-abc123", want: "sk-abc123"},
		{name: "mixed case scheme", header: "BeArEr sk-abc123", want: "sk-abc123"},
		{name: "extra spaces", header: "Bearer   sk-abc123  ", want: "sk-abc123"},
		{name: "missing", header: "", wantErr: ErrMissing},
		{name: "whitespace only", header: "   ", wantErr: ErrMissing},
		{name: "scheme only", header: "Bearer", wantErr: ErrMalformed},
		{name: "wrong scheme", header: "Basic abc", wantErr: ErrMalformed},
		{name: "empty token", header: "Bearer    ", wantErr: ErrMalformed},
		{name: "space in token", header: "Bearer sk abc", wantErr: ErrMalformed},
		{name: "comma separated", header: "Bearer a,b", wantErr: ErrMalformed},
		{name: "quoted token", header: `Bearer "abc"`, wantErr: ErrMalformed},
		{name: "control character", header: "Bearer a\tb", wantErr: ErrMalformed},
		{name: "non ascii", header: "Bearer sk-密钥", wantErr: ErrMalformed},
		{name: "too long", header: "Bearer " + strings.Repeat("a", 4097), wantErr: ErrMalformed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseBearer(tt.header)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ParseBearer(%q) error = %v, want %v", tt.header, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("ParseBearer(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestFromHeader(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		if _, err := FromHeader(http.Header{}); !errors.Is(err, ErrMissing) {
			t.Fatalf("error = %v, want ErrMissing", err)
		}
	})
	t.Run("single", func(t *testing.T) {
		h := http.Header{}
		h.Set("Authorization", "Bearer sk-abc")
		token, err := FromHeader(h)
		if err != nil || token != "sk-abc" {
			t.Fatalf("token = %q, err = %v", token, err)
		}
	})
	t.Run("multiple", func(t *testing.T) {
		h := http.Header{}
		h.Add("Authorization", "Bearer sk-a")
		h.Add("Authorization", "Bearer sk-b")
		if _, err := FromHeader(h); !errors.Is(err, ErrMalformed) {
			t.Fatalf("error = %v, want ErrMalformed", err)
		}
	})
}

func TestContextToken(t *testing.T) {
	ctx := WithToken(context.Background(), "sk-abc")
	token, ok := FromContext(ctx)
	if !ok || token != "sk-abc" {
		t.Fatalf("FromContext() = %q, %v", token, ok)
	}
	if _, ok := FromContext(context.Background()); ok {
		t.Fatal("FromContext() on a plain context should report false")
	}
	if _, ok := FromContext(WithToken(context.Background(), "")); ok {
		t.Fatal("FromContext() should reject an empty token")
	}
}
