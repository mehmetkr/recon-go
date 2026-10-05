//go:build integration

package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mehmetkr/recon-go/internal/http/handler"
	"github.com/mehmetkr/recon-go/internal/http/middleware"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/recon"
	"github.com/mehmetkr/recon-go/internal/store/postgres"
)

func setupIntegration(t *testing.T) *httptest.Server {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpg.Run(ctx, "postgres:17",
		tcpg.WithDatabase("recon_test"),
		tcpg.WithUsername("recon"),
		tcpg.WithPassword("recon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctr.Terminate(context.Background()) })

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if err := postgres.Migrate(connStr); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &handler.Handler{
		Store:   postgres.New(pool),
		Service: &recon.Service{Matcher: match.Run},
		Log:     log,
	}

	mux := middleware.Chain(
		h.Routes(),
		middleware.RequestID,
		middleware.Logger(log),
		middleware.Recovery(log),
		middleware.Timeout(30*time.Second),
	)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

const integBankCSV = "account,date,amount,currency,reference\nACC-1,2024-01-15,100.00,USD,INV10023\nACC-2,2024-01-16,250.50,EUR,PAY-9981\n"
const integLedgerCSV = "account,date,debit,credit,currency,reference\nACC-1,2024-01-15,100.00,0.00,USD,INV10023\nACC-2,2024-01-16,250.50,0.00,EUR,PAY-9981\n"

func postReconcile(t *testing.T, base string, bank, ledger string) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	bankField, err := w.CreateFormFile("bank", "bank.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bankField.Write([]byte(bank)); err != nil {
		t.Fatal(err)
	}
	ledgerField, err := w.CreateFormFile("ledger", "ledger.csv")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledgerField.Write([]byte(ledger)); err != nil {
		t.Fatal(err)
	}
	w.Close()

	resp, err := http.Post(base+"/reconcile", w.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readJSON(t *testing.T, resp *http.Response) map[string]any {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, b)
	}
	return v
}

func TestIntegrationPostReconcile(t *testing.T) {
	srv := setupIntegration(t)

	resp := postReconcile(t, srv.URL, integBankCSV, integLedgerCSV)
	if resp.StatusCode != http.StatusCreated {
		body := readJSON(t, resp)
		t.Fatalf("status = %d, want 201; body: %v", resp.StatusCode, body)
	}

	body := readJSON(t, resp)
	runID, ok := body["run_id"].(string)
	if !ok || runID == "" {
		t.Fatal("response missing run_id")
	}
	matches, ok := body["matches"].([]any)
	if !ok {
		t.Fatal("response missing matches array")
	}
	if len(matches) != 2 {
		t.Errorf("want 2 matches, got %d", len(matches))
	}

	if resp.Header.Get("X-Request-ID") == "" {
		t.Error("response missing X-Request-ID header (middleware not applied)")
	}
}

func TestIntegrationGetRun(t *testing.T) {
	srv := setupIntegration(t)

	createResp := postReconcile(t, srv.URL, integBankCSV, integLedgerCSV)
	created := readJSON(t, createResp)
	runID := created["run_id"].(string)

	resp, err := http.Get(srv.URL + "/reconcile/" + runID)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readJSON(t, resp)
	if body["run_id"] != runID {
		t.Errorf("run_id = %v, want %s", body["run_id"], runID)
	}
}

func TestIntegrationGetExceptions(t *testing.T) {
	srv := setupIntegration(t)

	createResp := postReconcile(t, srv.URL, integBankCSV, integLedgerCSV)
	created := readJSON(t, createResp)
	runID := created["run_id"].(string)

	resp, err := http.Get(srv.URL + "/reconcile/" + runID + "/exceptions")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readJSON(t, resp)
	if body["run_id"] != runID {
		t.Errorf("run_id = %v, want %s", body["run_id"], runID)
	}
	excs, ok := body["exceptions"].([]any)
	if !ok {
		t.Fatal("exceptions is not an array")
	}
	if len(excs) != 0 {
		t.Errorf("want 0 exceptions, got %d", len(excs))
	}
}

func TestIntegrationDuplicateRun(t *testing.T) {
	srv := setupIntegration(t)

	resp1 := postReconcile(t, srv.URL, integBankCSV, integLedgerCSV)
	if resp1.StatusCode != http.StatusCreated {
		body := readJSON(t, resp1)
		t.Fatalf("first: status = %d, want 201; body: %v", resp1.StatusCode, body)
	}
	created := readJSON(t, resp1)
	runID := created["run_id"].(string)

	resp2 := postReconcile(t, srv.URL, integBankCSV, integLedgerCSV)
	if resp2.StatusCode != http.StatusOK {
		body := readJSON(t, resp2)
		t.Fatalf("second: status = %d, want 200; body: %v", resp2.StatusCode, body)
	}
	body := readJSON(t, resp2)
	if body["run_id"] != runID {
		t.Errorf("duplicate run_id = %v, want %s", body["run_id"], runID)
	}
}

func TestIntegrationGetRunNotFound(t *testing.T) {
	srv := setupIntegration(t)

	resp, err := http.Get(srv.URL + "/reconcile/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestIntegrationHealth(t *testing.T) {
	srv := setupIntegration(t)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readJSON(t, resp)
	if body["status"] != "ok" {
		t.Errorf("status = %v, want ok", body["status"])
	}
}

func TestIntegrationReady(t *testing.T) {
	srv := setupIntegration(t)

	resp, err := http.Get(srv.URL + "/ready")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readJSON(t, resp)
	if body["status"] != "ready" {
		t.Errorf("status = %v, want ready", body["status"])
	}
}
