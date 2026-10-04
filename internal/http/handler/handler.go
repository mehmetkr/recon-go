// Package handler provides HTTP request handlers for the reconciliation API.
package handler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"unicode/utf8"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/ingest"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/recon"
	"github.com/mehmetkr/recon-go/internal/store"
)

const maxBodySize = 50 << 20 // 50 MB
const maxMemory = 10 << 20   // 10 MB in-RAM before spilling to disk

// Handler serves the reconciliation HTTP API.
type Handler struct {
	Store   store.Store
	Service *recon.Service
	Log     *slog.Logger
}

// Routes returns a ServeMux with all API routes registered.
func (h *Handler) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /ready", h.Ready)
	mux.HandleFunc("POST /reconcile", h.Reconcile)
	mux.HandleFunc("GET /reconcile/{id}", h.GetRun)
	mux.HandleFunc("GET /reconcile/{id}/exceptions", h.GetExceptions)
	return mux
}

// Health reports that the server is alive.
func (h *Handler) Health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Ready reports that the server can serve requests.
func (h *Handler) Ready(w http.ResponseWriter, r *http.Request) {
	if _, err := h.Store.LoadState(r.Context()); err != nil {
		h.Log.Error("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// Reconcile handles POST /reconcile with multipart file upload.
func (h *Handler) Reconcile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodySize)

	if err := r.ParseMultipartForm(maxMemory); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid multipart form: %v", err)
		return
	}

	bankRaw, err := readFormFile(r, "bank")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bank file: %v", err)
		return
	}
	ledgerRaw, err := readFormFile(r, "ledger")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ledger file: %v", err)
		return
	}

	tz := formDefault(r, "tz", "UTC")
	loc, err := ingest.LoadZone(tz)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	bankLayouts := formLayouts(r, "bank_date_layout")
	ledgerLayouts := formLayouts(r, "ledger_date_layout")
	bankCfg := ingest.Config{Layouts: bankLayouts, Location: loc}
	ledgerCfg := ingest.Config{Layouts: ledgerLayouts, Location: loc}
	for _, c := range []ingest.Config{bankCfg, ledgerCfg} {
		if err := c.Validate(); err != nil {
			writeErr(w, http.StatusBadRequest, "%v", err)
			return
		}
	}

	params, err := parseParams(r)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "%v", err)
		return
	}

	bank, bankErrs, err := ingest.Parse(bytes.NewReader(bankRaw), domain.Bank, bankCfg)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "bank: %v", err)
		return
	}
	ledger, ledgerErrs, err := ingest.Parse(bytes.NewReader(ledgerRaw), domain.Ledger, ledgerCfg)
	if err != nil {
		writeErr(w, http.StatusUnprocessableEntity, "ledger: %v", err)
		return
	}

	ctx := r.Context()
	state, err := h.Store.LoadState(ctx)
	if err != nil {
		h.respondErr(w, err)
		return
	}
	if err := state.Lock(tz, bankCfg.Layouts, ledgerCfg.Layouts); err != nil {
		writeErr(w, http.StatusConflict, "%v", err)
		return
	}

	force := r.FormValue("force") == "true"
	out, err := h.Service.Reconcile(ctx, state, recon.Input{
		BankRaw:       bankRaw,
		LedgerRaw:     ledgerRaw,
		Bank:          bank,
		Ledger:        ledger,
		BankErrs:      bankErrs,
		LedgerErrs:    ledgerErrs,
		TZ:            tz,
		BankLayouts:   bankCfg.Layouts,
		LedgerLayouts: ledgerCfg.Layouts,
		Params:        params,
		Workers:       runtime.GOMAXPROCS(0),
		Force:         force,
	})
	if err != nil {
		h.respondErr(w, err)
		return
	}

	if out.Skipped {
		report, err := h.Store.GetReport(ctx, out.Report.RunID)
		if err != nil {
			h.Log.Error("report lookup for skipped run", "run_id", out.Report.RunID, "err", err)
			writeJSON(w, http.StatusOK, map[string]any{
				"run_id":  out.Report.RunID,
				"status":  "skipped",
				"message": "already reconciled; use force=true to run again",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write(report)
		return
	}

	if err := h.Store.SaveState(ctx, state); err != nil {
		h.respondErr(w, err)
		return
	}
	if err := h.Store.SaveReport(ctx, out.Report.RunID, out.Encoded); err != nil {
		h.respondErr(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(out.Encoded)
}

// GetRun handles GET /reconcile/{id}.
func (h *Handler) GetRun(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	report, err := h.Store.GetReport(r.Context(), runID)
	if err != nil {
		h.respondErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(report)
}

// GetExceptions handles GET /reconcile/{id}/exceptions.
func (h *Handler) GetExceptions(w http.ResponseWriter, r *http.Request) {
	runID := r.PathValue("id")
	data, err := h.Store.GetReport(r.Context(), runID)
	if err != nil {
		h.respondErr(w, err)
		return
	}
	var full struct {
		Exceptions json.RawMessage `json:"exceptions"`
	}
	if err := json.Unmarshal(data, &full); err != nil {
		h.Log.Error("corrupt report", "run_id", runID, "err", err)
		writeErr(w, http.StatusInternalServerError, "corrupt report")
		return
	}
	if full.Exceptions == nil {
		full.Exceptions = json.RawMessage("[]")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_id":     runID,
		"exceptions": json.RawMessage(full.Exceptions),
	})
}

func (h *Handler) respondErr(w http.ResponseWriter, err error) {
	code := statusFromErr(err)
	if code == http.StatusInternalServerError {
		h.Log.Error("internal error", "err", err)
	}
	writeErr(w, code, "%v", err)
}

func readFormFile(r *http.Request, field string) ([]byte, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		return nil, fmt.Errorf("missing %q file field", field)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%s is not valid UTF-8", field)
	}
	return b, nil
}

func formDefault(r *http.Request, key, fallback string) string {
	v := r.FormValue(key)
	if v == "" {
		return fallback
	}
	return v
}

func formLayouts(r *http.Request, key string) []string {
	vals := r.Form[key]
	if len(vals) == 0 {
		return ingest.DefaultLayouts()
	}
	return vals
}

func parseParams(r *http.Request) (match.Params, error) {
	p := match.Params{DateTolerance: 3, FuzzyWindow: 7}
	if v := r.FormValue("date_tolerance"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, fmt.Errorf("date_tolerance: %w", err)
		}
		p.DateTolerance = n
	}
	if v := r.FormValue("fuzzy_window"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return p, fmt.Errorf("fuzzy_window: %w", err)
		}
		p.FuzzyWindow = n
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

func statusFromErr(err error) int {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, format string, args ...any) {
	writeJSON(w, code, map[string]string{"error": fmt.Sprintf(format, args...)})
}
