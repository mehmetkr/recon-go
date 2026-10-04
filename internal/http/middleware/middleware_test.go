package middleware_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mehmetkr/recon-go/internal/http/middleware"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func TestRequestID_Generated(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := middleware.RequestIDFrom(r.Context())
		if id == "" {
			t.Error("context has no request ID")
		}
		w.Write([]byte(id))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if got := rec.Header().Get("X-Request-ID"); got == "" {
		t.Error("response missing X-Request-ID header")
	}
	if len(rec.Body.String()) != 16 {
		t.Errorf("generated ID length = %d, want 16", len(rec.Body.String()))
	}
}

func TestRequestID_Forwarded(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(middleware.RequestIDFrom(r.Context())))
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Request-ID", "caller-provided-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Body.String() != "caller-provided-id" {
		t.Errorf("ID = %q, want caller-provided-id", rec.Body.String())
	}
	if rec.Header().Get("X-Request-ID") != "caller-provided-id" {
		t.Error("response header should echo the caller-provided ID")
	}
}

func TestRequestID_InvalidRejected(t *testing.T) {
	h := middleware.RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(middleware.RequestIDFrom(r.Context())))
	}))

	for _, bad := range []string{
		"has spaces",
		"newline\ninjection",
		strings.Repeat("a", 200),
		"control\x00char",
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.Header.Set("X-Request-ID", bad)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		got := rec.Body.String()
		if got == bad {
			t.Errorf("invalid ID %q was accepted", bad)
		}
		if len(got) != 16 {
			t.Errorf("replacement ID length = %d, want 16", len(got))
		}
	}
}

func TestLogger(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	h := middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/reconcile", nil))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	out := buf.String()
	for _, want := range []string{"method=POST", "path=/reconcile", "status=201", "duration_ms="} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
}

func TestLogger_DefaultStatus(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	h := middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if !strings.Contains(buf.String(), "status=200") {
		t.Errorf("implicit 200 not logged:\n%s", buf.String())
	}
}

func TestRecovery(t *testing.T) {
	h := middleware.Recovery(discardLog)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("test panic")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal server error") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestRecovery_HeadersAlreadyWritten(t *testing.T) {
	h := middleware.Recovery(discardLog)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("after headers sent")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (headers already committed)", rec.Code)
	}
}

func TestRecovery_NoPanic(t *testing.T) {
	h := middleware.Recovery(discardLog)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestTimeout(t *testing.T) {
	h := middleware.Timeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		w.Write([]byte("late"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "request timeout") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestChain(t *testing.T) {
	var order []string
	mw := func(name string) func(http.Handler) http.Handler {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name+"-before")
				next.ServeHTTP(w, r)
				order = append(order, name+"-after")
			})
		}
	}

	h := middleware.Chain(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			order = append(order, "handler")
		}),
		mw("A"),
		mw("B"),
	)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	want := "A-before,B-before,handler,B-after,A-after"
	got := strings.Join(order, ",")
	if got != want {
		t.Errorf("order = %s, want %s", got, want)
	}
}

func TestChain_NoMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("direct"))
	})
	h := middleware.Chain(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Body.String() != "direct" {
		t.Errorf("body = %q, want direct", rec.Body.String())
	}
}

func TestLogger_DoubleWriteHeader(t *testing.T) {
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, nil))

	h := middleware.Logger(log)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.WriteHeader(http.StatusNotFound)
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if !strings.Contains(buf.String(), "status=201") {
		t.Errorf("first status should be captured:\n%s", buf.String())
	}
}
