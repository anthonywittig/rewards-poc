// Command mockapi serves the frozen API contract from fixtures, with no
// Temporal and no Docker.
//
// It exists so the web UI (Phase 8) can be built in parallel with the endpoints
// it consumes: the customer list is Phase 4 and the audit timeline is Phase 5,
// and neither exists yet. Because it shares internal/httpapi's DTOs, the mock
// cannot drift from the real contract without failing to compile.
//
//	make mockapi        # :8082
//
// What it deliberately reproduces, because these are the behaviours a UI built
// only against a happy path gets wrong:
//
//   - list results are unsorted and may be paginated, with `complete` saying
//     whether client-side sorting is even meaningful
//   - visibility lag: a newly created customer is missing from the list for a
//     beat, exactly as Elasticsearch behaves
//   - a truncated audit log ("grace")
//   - a deactivated customer who cannot take points ("departed")
//   - every documented error code, on demand
//
// What it does not reproduce: real workflow behaviour. Points added here are
// kept in a map. Continue-as-new, the validator/handler split, and the rollover
// races are all real-stack concerns -- switch to `make api` for those.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/httpapi"
)

// visibilityLag mimics the Elasticsearch delay a newly created customer is
// subject to before appearing in the list. Real value after tuning is
// ~200-300ms and never zero (PLAN.md 7.5), so a UI that assumes otherwise
// breaks here rather than in front of someone.
const visibilityLag = 400 * time.Millisecond

type mock struct {
	mu        sync.Mutex
	customers map[string]httpapi.CustomerResponse
	audits    map[string]httpapi.AuditResponse
	// When each customer becomes list-visible.
	visibleAt map[string]time.Time
}

func main() {
	addr := ":" + env("MOCK_PORT", "8082")

	m := &mock{
		customers: map[string]httpapi.CustomerResponse{},
		audits:    map[string]httpapi.AuditResponse{},
		visibleAt: map[string]time.Time{},
	}
	for id, c := range httpapi.FixtureCustomers {
		m.customers[id] = c
		m.visibleAt[id] = time.Time{} // fixtures are already visible
	}
	for id, a := range httpapi.FixtureAudits {
		m.audits[id] = a
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/customers", m.list)
	mux.HandleFunc("POST /api/customers", m.enroll)
	mux.HandleFunc("GET /api/customers/{id}", m.get)
	mux.HandleFunc("POST /api/customers/{id}/points", m.addPoints)
	mux.HandleFunc("DELETE /api/customers/{id}", m.deactivate)
	mux.HandleFunc("GET /api/customers/{id}/audit", m.audit)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok", "mode": "mock"})
	})

	log.Printf("mockapi listening on %s -- fixtures: %s", addr, strings.Join(sortedKeys(m.customers), ", "))
	log.Printf("no Temporal required; see cmd/mockapi/main.go for what is and is not simulated")

	srv := &http.Server{Addr: addr, Handler: cors(mux), ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("mockapi stopped: %v", err)
	}
}

// cors lets a Vite dev server on another port talk to this directly.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (m *mock) list(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()

	q := r.URL.Query().Get("q")

	// Everything matching, which is the count the real endpoint gets from
	// CountWorkflow rather than by materialising rows.
	var matched []httpapi.CustomerListItem
	now := time.Now()
	for _, id := range sortedKeys(m.customers) {
		c := m.customers[id]
		if vis, ok := m.visibleAt[id]; ok && now.Before(vis) {
			continue // still lagging, exactly as ES would
		}
		if !matches(c, q) {
			continue
		}
		matched = append(matched, httpapi.FixtureListItem(c))
	}

	total := len(matched)
	items := matched
	if len(items) > httpapi.ListLimit {
		items = items[:httpapi.ListLimit]
	}
	if items == nil {
		items = []httpapi.CustomerListItem{}
	}

	writeJSON(w, 200, httpapi.CustomerListResponse{
		Items:    items,
		Limit:    httpapi.ListLimit,
		Total:    total,
		Complete: total <= httpapi.ListLimit,
		Query:    q,
	})
}

// matches supports the small slice of Temporal's query language the UI actually
// sends. Not a parser -- if the real Phase 4 endpoint needs more, it gets it
// from the server rather than from here.
func matches(c httpapi.CustomerResponse, q string) bool {
	if strings.TrimSpace(q) == "" {
		return true
	}
	ok := true
	for _, clause := range strings.Split(q, " AND ") {
		clause = strings.TrimSpace(clause)
		switch {
		case strings.HasPrefix(clause, "RewardsLevel"):
			ok = ok && strings.Contains(clause, `'`+c.Level+`'`)
		case strings.HasPrefix(clause, "ExecutionStatus"):
			want := "Running"
			if c.Status == "deactivated" {
				want = "Canceled"
			}
			ok = ok && strings.Contains(clause, `'`+want+`'`)
		case strings.HasPrefix(clause, "CustomerName"):
			ok = ok && strings.Contains(strings.ToLower(clause), strings.ToLower(firstWord(c.Name)))
		case strings.HasPrefix(clause, "RewardsPoints >="):
			ok = ok && c.Points >= atoiOr(lastField(clause), 0)
		}
	}
	return ok
}

func (m *mock) get(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[r.PathValue("id")]
	if !ok {
		writeErr(w, 404, "not_found", "customer not found, or their history has been deleted")
		return
	}
	writeJSON(w, 200, c)
}

func (m *mock) audit(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	if _, ok := m.customers[id]; !ok {
		writeErr(w, 404, "not_found", "customer not found, or their history has been deleted")
		return
	}
	a, ok := m.audits[id]
	if !ok {
		a = httpapi.AuditResponse{CustomerID: id, Entries: []httpapi.AuditEntry{}, RunsWalked: 1}
	}
	writeJSON(w, 200, a)
}

func (m *mock) enroll(w http.ResponseWriter, r *http.Request) {
	var req httpapi.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}
	req.CustomerID = strings.TrimSpace(req.CustomerID)
	if req.CustomerID == "" {
		writeErr(w, 400, "invalid_request", "customerId is required")
		return
	}
	if strings.TrimSpace(req.Email) == "" {
		writeErr(w, 400, "invalid_request", "email is required")
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.customers[req.CustomerID]; ok {
		if existing.Status == "active" {
			writeErr(w, 409, "already_exists", "customer is already enrolled and active")
			return
		}
		// Soft re-enroll: keep points, lifetime counters, and enrollment date.
		existing.Status = "active"
		existing.Name = req.Name
		existing.Email = req.Email
		m.customers[req.CustomerID] = existing
		m.visibleAt[req.CustomerID] = time.Now().Add(visibilityLag)
		writeJSON(w, 200, httpapi.EnrollResponse{
			CustomerID: req.CustomerID, WorkflowID: "customer-" + req.CustomerID, RunID: existing.RunID,
		})
		return
	}

	runID := "mock-run-" + req.CustomerID
	m.customers[req.CustomerID] = httpapi.CustomerResponse{
		CustomerID: req.CustomerID, Name: req.Name, Email: req.Email,
		Points: 0, Level: "basic", NextTierAt: 500,
		EnrolledAt: time.Now().UTC(), Status: "active", RunID: runID,
	}
	// Not list-visible yet: this is the lag the UI must design around.
	m.visibleAt[req.CustomerID] = time.Now().Add(visibilityLag)
	m.audits[req.CustomerID] = httpapi.AuditResponse{
		CustomerID: req.CustomerID, RunsWalked: 1,
		Entries: []httpapi.AuditEntry{{
			Kind: httpapi.AuditEnrolled, At: time.Now().UTC(), RunID: runID, EventID: 1,
		}},
	}

	writeJSON(w, 201, httpapi.EnrollResponse{
		CustomerID: req.CustomerID, WorkflowID: "customer-" + req.CustomerID, RunID: runID,
	})
}

func (m *mock) addPoints(w http.ResponseWriter, r *http.Request) {
	var req httpapi.AddPointsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "invalid_request", "invalid JSON body: "+err.Error())
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	c, ok := m.customers[id]
	if !ok {
		writeErr(w, 404, "not_found", "customer not found, or their history has been deleted")
		return
	}
	if c.Status == "deactivated" {
		writeErr(w, 409, "deactivated", "customer is deactivated; re-enroll them before adding points")
		return
	}

	// The validator half of PLAN.md 3.4. Rejections here leave no audit row,
	// which is the behaviour the timeline has to look right without.
	switch {
	case req.Amount <= 0:
		writeErr(w, 422, "rejected", "amount must be positive, got "+strconv.Itoa(req.Amount))
		return
	case req.Amount > 1000:
		writeErr(w, 422, "rejected", "amount "+strconv.Itoa(req.Amount)+" exceeds the per-transaction maximum of 1000")
		return
	case strings.TrimSpace(req.Reason) == "":
		writeErr(w, 422, "rejected", "reason is required")
		return
	}
	// The handler half: recorded, so it *does* get an audit row.
	if c.Points+req.Amount > 100000 {
		a := m.audits[id]
		a.Entries = append(a.Entries, httpapi.AuditEntry{
			Kind: httpapi.AuditPointsRejected, At: time.Now().UTC(),
			Generation: c.Generation, RunID: c.RunID,
			Amount: req.Amount, Reason: req.Reason, RequestID: req.RequestID,
			Failure: "add of " + strconv.Itoa(req.Amount) + " would exceed the cap of 100000 (balance is " + strconv.Itoa(c.Points) + ")",
		})
		m.audits[id] = a
		writeErr(w, 422, "rejected",
			"add of "+strconv.Itoa(req.Amount)+" would exceed the cap of 100000 (balance is "+strconv.Itoa(c.Points)+")")
		return
	}

	before := c.Level
	c.Points += req.Amount
	c.Level = level(c.Points)
	c.NextTierAt = nextTierAt(c.Points)
	c.LifetimeEarnEvents++
	m.customers[id] = c
	promoted := c.Level != before

	a := m.audits[id]
	a.Entries = append(a.Entries, httpapi.AuditEntry{
		Kind: httpapi.AuditPointsAdded, At: time.Now().UTC(),
		Generation: c.Generation, RunID: c.RunID,
		Amount: req.Amount, Reason: req.Reason, Balance: c.Points, Level: c.Level,
		RequestID: req.RequestID,
	})

	// Phase 6: a tier crossing schedules the notification Activity, and because
	// Activities are history events the crawl renders a row for it. Reproduced
	// here so the UI sees promotion rows arrive from ordinary use rather than
	// only in the canned fixtures.
	//
	// Only promotions. The departure notice uses the same Activity but produces
	// no audit row, because notification_sent carries a level and nothing else --
	// see PLAN.md 12.31. The real API behaves the same way.
	if promoted {
		a.Entries = append(a.Entries, httpapi.AuditEntry{
			Kind: httpapi.AuditNotificationSent, At: time.Now().UTC(),
			Generation: c.Generation, RunID: c.RunID,
			NotifiedLevel: c.Level,
		})
	}
	a.ShownEarnEvents++
	a.LifetimeEarnEvents++
	m.audits[id] = a

	writeJSON(w, 200, httpapi.AddPointsResponse{
		Balance: c.Points, Level: c.Level,
		EventID: id + ":" + strconv.Itoa(c.LifetimeEarnEvents),
	})
}

func (m *mock) deactivate(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := r.PathValue("id")
	c, ok := m.customers[id]
	if !ok {
		writeErr(w, 404, "not_found", "customer not found, or their history has been deleted")
		return
	}
	// Cancel is idempotent server-side, so a repeat DELETE is a 204 no-op.
	if c.Status == "active" {
		c.Status = "deactivated"
		m.customers[id] = c
		a := m.audits[id]
		a.Entries = append(a.Entries, httpapi.AuditEntry{
			Kind: httpapi.AuditDeactivated, At: time.Now().UTC(),
			Generation: c.Generation, RunID: c.RunID,
		})
		m.audits[id] = a
	}
	w.WriteHeader(http.StatusNoContent)
}

func level(p int) string {
	switch {
	case p >= 1000:
		return "platinum"
	case p >= 500:
		return "gold"
	default:
		return "basic"
	}
}

func nextTierAt(p int) int {
	switch {
	case p >= 1000:
		return 0
	case p >= 500:
		return 1000
	default:
		return 500
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, httpapi.ErrorResponse{Error: httpapi.ErrorBody{Code: code, Message: msg}})
}

func sortedKeys(m map[string]httpapi.CustomerResponse) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func lastField(s string) string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return ""
	}
	return f[len(f)-1]
}

func atoiOr(s string, fallback int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return fallback
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
