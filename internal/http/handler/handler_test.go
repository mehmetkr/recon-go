package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/http/handler"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/recon"
	"github.com/mehmetkr/recon-go/internal/store"
)

type mockStore struct {
	state   *store.State
	report  map[string][]byte
	loadErr error
}

func newMockStore() *mockStore {
	return &mockStore{
		state: &store.State{
			Version: 1, IDScheme: 1,
			Matches: map[string]store.Committed{},
		},
		report: map[string][]byte{},
	}
}

func (m *mockStore) LoadState(_ context.Context) (*store.State, error) {
	if m.loadErr != nil {
		return nil, m.loadErr
	}
	return m.state, nil
}

func (m *mockStore) SaveState(_ context.Context, s *store.State) error {
	m.state = s
	return nil
}

func (m *mockStore) SaveReport(_ context.Context, runID string, data []byte) error {
	if !m.state.HasRun(runID) {
		return fmt.Errorf("%w: run %s", domain.ErrNotFound, runID)
	}
	m.report[runID] = data
	return nil
}

func (m *mockStore) GetReport(_ context.Context, runID string) ([]byte, error) {
	data, ok := m.report[runID]
	if !ok {
		return nil, fmt.Errorf("%w: report %s", domain.ErrNotFound, runID)
	}
	return data, nil
}

func newHandler(ms *mockStore) *handler.Handler {
	return &handler.Handler{
		Store:   ms,
		Service: &recon.Service{Matcher: match.Run},
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func unmarshalResponse(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, body)
	}
	return v
}

func TestHealth(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := unmarshalResponse(t, rec.Body.Bytes())
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
}

func TestReady(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestReadyUnavailable(t *testing.T) {
	ms := newMockStore()
	ms.loadErr = errors.New("connection refused")
	h := newHandler(ms)
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/ready", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

const bankCSV = "account,date,amount,currency,reference\nACC-1,2024-01-15,100.00,USD,INV10023\n"
const ledgerCSV = "account,date,debit,credit,currency,reference\nACC-1,2024-01-15,100.00,0.00,USD,INV10023\n"

func reconcileRequest(t *testing.T, fields map[string]string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	bankField, _ := w.CreateFormFile("bank", "bank.csv")
	bankField.Write([]byte(fields["bank"]))
	ledgerField, _ := w.CreateFormFile("ledger", "ledger.csv")
	ledgerField.Write([]byte(fields["ledger"]))

	for k, v := range fields {
		if k == "bank" || k == "ledger" {
			continue
		}
		w.WriteField(k, v)
	}
	w.Close()

	req := httptest.NewRequest("POST", "/reconcile", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func TestReconcileSuccess(t *testing.T) {
	ms := newMockStore()
	h := newHandler(ms)
	rec := httptest.NewRecorder()

	req := reconcileRequest(t, map[string]string{"bank": bankCSV, "ledger": ledgerCSV})
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", rec.Code, rec.Body.String())
	}
	body := unmarshalResponse(t, rec.Body.Bytes())
	runID, ok := body["run_id"].(string)
	if !ok || runID == "" {
		t.Fatal("response missing run_id")
	}
	if len(ms.state.Runs) != 1 {
		t.Errorf("want 1 run in state, got %d", len(ms.state.Runs))
	}
	if len(ms.report) != 1 {
		t.Errorf("want 1 stored report, got %d", len(ms.report))
	}
}

func TestReconcileSkipped(t *testing.T) {
	ms := newMockStore()
	h := newHandler(ms)

	rec1 := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec1, reconcileRequest(t, map[string]string{"bank": bankCSV, "ledger": ledgerCSV}))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec2, reconcileRequest(t, map[string]string{"bank": bankCSV, "ledger": ledgerCSV}))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second: status = %d, want 200; body: %s", rec2.Code, rec2.Body.String())
	}
}

func TestReconcileMissingFile(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	bankField, _ := w.CreateFormFile("bank", "bank.csv")
	bankField.Write([]byte(bankCSV))
	w.Close()

	req := httptest.NewRequest("POST", "/reconcile", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestReconcileBadParams(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()

	req := reconcileRequest(t, map[string]string{
		"bank": bankCSV, "ledger": ledgerCSV,
		"date_tolerance": "abc",
	})
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestReconcileInvalidCSV(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()

	req := reconcileRequest(t, map[string]string{
		"bank":   "not,a,valid,header\n",
		"ledger": ledgerCSV,
	})
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body: %s", rec.Code, rec.Body.String())
	}
}

func TestReconcileBadTimezone(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()

	req := reconcileRequest(t, map[string]string{
		"bank": bankCSV, "ledger": ledgerCSV,
		"tz": "Not/A/Zone",
	})
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestReconcileNonUTF8(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()

	req := reconcileRequest(t, map[string]string{
		"bank":   "account,date,amount,currency,reference\nACC-1,2024-01-15,100.00,USD,\xff\xfe\n",
		"ledger": ledgerCSV,
	})
	h.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", rec.Code, rec.Body.String())
	}
}

func TestGetRun(t *testing.T) {
	ms := newMockStore()
	h := newHandler(ms)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, reconcileRequest(t, map[string]string{"bank": bankCSV, "ledger": ledgerCSV}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d", rec.Code)
	}
	created := unmarshalResponse(t, rec.Body.Bytes())
	runID := created["run_id"].(string)

	rec2 := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec2, httptest.NewRequest("GET", "/reconcile/"+runID, nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("get: status = %d; body: %s", rec2.Code, rec2.Body.String())
	}
	report := unmarshalResponse(t, rec2.Body.Bytes())
	if report["run_id"] != runID {
		t.Errorf("run_id = %v, want %s", report["run_id"], runID)
	}
}

func TestGetRunNotFound(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/reconcile/nonexistent", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGetExceptions(t *testing.T) {
	ms := newMockStore()
	h := newHandler(ms)

	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, reconcileRequest(t, map[string]string{"bank": bankCSV, "ledger": ledgerCSV}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201", rec.Code)
	}
	created := unmarshalResponse(t, rec.Body.Bytes())
	runID := created["run_id"].(string)

	rec2 := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec2, httptest.NewRequest("GET", "/reconcile/"+runID+"/exceptions", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("status = %d; body: %s", rec2.Code, rec2.Body.String())
	}
	body := unmarshalResponse(t, rec2.Body.Bytes())
	if body["run_id"] != runID {
		t.Errorf("run_id = %v, want %s", body["run_id"], runID)
	}
	if body["exceptions"] == nil {
		t.Error("missing exceptions key")
	}
}

func TestGetExceptionsNotFound(t *testing.T) {
	h := newHandler(newMockStore())
	rec := httptest.NewRecorder()
	h.Routes().ServeHTTP(rec, httptest.NewRequest("GET", "/reconcile/nonexistent/exceptions", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
