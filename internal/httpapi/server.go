package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/anthonywittig/rewards-poc/internal/rewards"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// Server is the HTTP surface. The Temporal client is the only dependency --
// there is deliberately nothing else here.
type Server struct {
	temporal client.Client
	log      *slog.Logger
}

// New builds the server.
func New(c client.Client, log *slog.Logger) *Server {
	return &Server{temporal: c, log: log}
}

// Routes returns the mux. Method-and-wildcard patterns are stdlib as of Go 1.22,
// so there is no router dependency to justify.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/customers", s.handle(s.listCustomers))
	mux.HandleFunc("POST /api/customers", s.handle(s.enroll))
	mux.HandleFunc("GET /api/customers/{id}", s.handle(s.getCustomer))
	mux.HandleFunc("POST /api/customers/{id}/points", s.handle(s.addPoints))
	mux.HandleFunc("DELETE /api/customers/{id}", s.handle(s.deactivate))
	mux.HandleFunc("GET /api/customers/{id}/audit", s.handle(s.getAudit))
	mux.HandleFunc("GET /healthz", s.handle(s.health))
	return mux
}

// handle adapts a handler that returns an error into an http.HandlerFunc, so
// every failure goes through one mapping path and none can leak a raw error.
func (s *Server) handle(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(w, r); err != nil {
			writeError(w, s.log, err)
		}
	}
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, s.log, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

// enroll starts a customer's workflow.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) error {
	var req EnrollRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	// Checked here as well as in the workflow. The workflow is the integrity
	// boundary and keeps its own validation (PLAN.md 3.1), but letting a bad
	// request through would turn a 400 into a WorkflowExecutionFailed and a
	// burnt workflow ID, which is a poor way to report a typo.
	req.CustomerID = strings.TrimSpace(req.CustomerID)
	if req.CustomerID == "" {
		return badRequest("customerId is required")
	}
	if strings.TrimSpace(req.Email) == "" {
		return badRequest("email is required")
	}
	if strings.ContainsAny(req.CustomerID, " \t\n/") {
		return badRequest("customerId must not contain whitespace or slashes")
	}

	run, err := s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:        rewards.WorkflowID(req.CustomerID),
		TaskQueue: rewards.TaskQueue,
		// Both are required for a duplicate enrollment to surface as an error.
		// The conflict policy governs what the server does; the second flag
		// governs whether the SDK tells us. PLAN.md 3.6 and 12.7.
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, rewards.CustomerRewardsWorkflow, rewards.CustomerState{
		CustomerID: req.CustomerID,
		Name:       req.Name,
		Email:      req.Email,
	})
	if err != nil {
		return mapStartError(err)
	}

	writeJSON(w, s.log, http.StatusCreated, EnrollResponse{
		CustomerID: req.CustomerID,
		WorkflowID: run.GetID(),
		RunID:      run.GetRunID(),
	})
	return nil
}

// listCustomers serves the customer list straight out of the visibility store.
//
// This is where search attributes earn their keep: no lookup table, no local
// index, and the same query works unchanged in the Temporal UI -- which is the
// demonstration PLAN.md 4 is after.
//
// Capped at ListLimit with no pagination. That follows from the platform rather
// than from laziness: ORDER BY is rejected outright (PLAN.md 12.8), so there is
// no stable ordering, and "page 2" of an unordered set could overlap or skip
// rows. A small slice plus an exact count plus a nudge to filter is the honest
// shape. See PLAN.md 5.1.
func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) error {
	userQuery := strings.TrimSpace(r.URL.Query().Get("q"))

	// Caught before it reaches the server, purely for the error message.
	// Temporal rejects ORDER BY with a clear "ORDER BY clause is not supported",
	// but wrapping the caller's filter in parentheses (see scopedQuery) turns it
	// into a bare syntax error first -- so the useful diagnostic is destroyed by
	// our own scoping. Reproduce it, and add the part Temporal cannot know: what
	// to do instead.
	if hasOrderBy(userQuery) {
		return badRequest("ORDER BY is not supported by Temporal's visibility store " +
			"(PLAN.md 12.8); filter to narrow the result set and sort client-side")
	}

	query := scopedQuery(userQuery)

	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()

	// Count first: it is the call most likely to reject a malformed user query,
	// and failing before fetching rows keeps a bad query from looking half-done.
	total := -1
	if cnt, err := s.temporal.CountWorkflow(ctx, &workflowservice.CountWorkflowExecutionsRequest{
		Query: query,
	}); err != nil {
		if apiErr := mapListError(err, userQuery); apiErr != nil {
			return apiErr
		}
		// Countable failures that are not the caller's fault degrade to "of
		// many" rather than failing a list we can still serve. PLAN.md 5.1.
		s.log.Warn("count failed; falling back to an unknown total",
			"query", query, "error", err)
	} else {
		total = int(cnt.GetCount())
	}

	resp, err := s.temporal.ListWorkflow(ctx, &workflowservice.ListWorkflowExecutionsRequest{
		PageSize: int32(ListLimit),
		Query:    query,
	})
	if err != nil {
		if apiErr := mapListError(err, userQuery); apiErr != nil {
			return apiErr
		}
		// Not mapQueryError: this read never involves a worker, so a timeout
		// here must not send anyone to go and restart one.
		return mapStoreReadError(err, "the customer list")
	}

	items := make([]CustomerListItem, 0, len(resp.GetExecutions()))
	for _, e := range resp.GetExecutions() {
		v := decodeSearchAttributes(e.GetSearchAttributes())

		// Status is a built-in rather than one of ours: a workflow cannot
		// record its own closure. PLAN.md 3.6.
		status := "deactivated"
		if e.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			status = "active"
		}

		// CustomerId is upserted by the workflow, but derive from the workflow
		// ID if it is somehow absent -- the ID is the real identity and is
		// always present. PLAN.md 3.1.
		id := v.CustomerID
		if id == "" {
			id = strings.TrimPrefix(e.GetExecution().GetWorkflowId(), rewards.WorkflowIDPrefix)
		}

		items = append(items, CustomerListItem{
			CustomerID: id,
			Name:       v.Name,
			Email:      v.Email,
			Points:     v.Points,
			Level:      v.Level,
			EnrolledAt: v.EnrolledAt,
			Generation: v.Generation,
			Status:     status,
			RunID:      e.GetExecution().GetRunId(),
		})
	}

	writeJSON(w, s.log, http.StatusOK, CustomerListResponse{
		Items:    items,
		Limit:    ListLimit,
		Total:    total,
		Complete: total >= 0 && total <= ListLimit,
		Query:    userQuery,
	})
	return nil
}

// hasOrderBy reports whether the query contains an ORDER BY clause, ignoring
// anything inside single-quoted literals -- so a customer actually named
// "order by" is searchable rather than mysteriously rejected.
func hasOrderBy(q string) bool {
	var b strings.Builder
	inQuote := false
	for _, r := range q {
		if r == '\'' {
			inQuote = !inQuote
			continue
		}
		if !inQuote {
			b.WriteRune(r)
		}
	}
	return strings.Contains(strings.ToLower(b.String()), "order by")
}

// scopedQuery constrains the list to our workflow type, and to one execution
// per customer.
//
// The second half is not an optimisation, it is a correctness fix. **The
// visibility store holds one document per Run, not per Workflow ID**, so a
// customer who has continued-as-new twice appears three times — with three
// different balances, since each closed generation froze its own. Left alone
// the "customer list" lists executions:
//
//	WorkflowId = 'customer-dup-check'                          Total: 3
//	WorkflowId = 'customer-dup-check' AND status != CAN        Total: 1
//
// Excluding ContinuedAsNew leaves exactly the current generation, whatever its
// final state: Running for an active customer, Canceled for a departed one, and
// Failed for an enrollment that never validated. `IN ('Running','Canceled')`
// would look equivalent and silently drop that last group — measured at 45
// against 47 on the same data.
//
// Found by the Phase 7 datastore inspection, which is exactly the sort of thing
// looking directly at Elasticsearch surfaces and an API test does not: every
// row was individually correct.
func scopedQuery(userQuery string) string {
	scope := "WorkflowType = '" + rewards.WorkflowTypeName + "'" +
		" AND ExecutionStatus != 'ContinuedAsNew'"
	if userQuery == "" {
		return scope
	}
	// Parenthesised so an OR in the caller's filter cannot escape the scope --
	// "a OR b" ANDed bare would bind as "(scope AND a) OR b".
	return scope + " AND (" + userQuery + ")"
}

// mapListError turns a rejected visibility query into a 400 carrying the
// server's own diagnostics, and returns nil for anything else.
//
// Passing the message through is deliberate. The query is user input from a
// raw-query box, and Temporal's errors are genuinely better than anything this
// layer could write:
//
//	invalid search attribute: NoSuchAttribute
//	invalid value for search attribute RewardsPoints of type Int: "not-an-int"
//	malformed SQL query: syntax error at position 41 near 'a'
//
// ORDER BY is the exception, handled before the query is sent -- see hasOrderBy.
func mapListError(err error, userQuery string) error {
	var invalid *serviceerror.InvalidArgument
	if !errors.As(err, &invalid) {
		return nil
	}
	msg := invalid.Error()
	if userQuery == "" {
		// Our own scoping clause was rejected, which is our bug, not theirs.
		return &apiError{http.StatusInternalServerError, CodeInternal, "internal error"}
	}
	return &apiError{http.StatusBadRequest, CodeInvalidRequest, msg}
}

// getCustomer reads current state via Query, and liveness via Describe.
//
// Two calls rather than one because they answer different questions: Query asks
// the workflow what it holds, Describe asks the server whether it is still
// running. A workflow cannot report its own closure, so Describe is what
// distinguishes active from deactivated.
func (s *Server) getCustomer(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}
	wfID := rewards.WorkflowID(id)

	desc, err := s.temporal.DescribeWorkflowExecution(r.Context(), wfID, "")
	if err != nil {
		return mapQueryError(err)
	}

	info := desc.GetWorkflowExecutionInfo()
	status := "active"
	if info.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		status = "deactivated"
	}

	out := CustomerResponse{
		CustomerID: id,
		Status:     status,
		RunID:      info.GetExecution().GetRunId(),
	}

	// Queried regardless of whether the customer is still active. A closed
	// execution answers Queries perfectly well -- Temporal replays its history
	// to serve them -- which is worth knowing, because assuming otherwise is
	// easy and costs real fidelity: search attributes carry no
	// LifetimeEarnEvents, so a departed customer would read back missing a field
	// that was available all along.
	enc, qerr := s.queryStatus(r.Context(), wfID)
	switch {
	case qerr == nil:
		var st rewards.CustomerStatus
		if err := enc.Get(&st); err != nil {
			return err
		}
		out.Name = st.Name
		out.Email = st.Email
		out.Points = st.Points
		out.Level = st.Level
		out.NextTierAt = st.NextTierAt
		out.EnrolledAt = st.EnrolledAt
		out.LifetimeEarnEvents = st.LifetimeEarnEvents
		out.Generation = st.Generation

	case status == "deactivated":
		// Answering a closed customer needs a worker to replay their history,
		// so with no worker polling they would otherwise 503 -- despite the
		// execution record in hand already carrying most of what the page
		// shows. Degrade to that rather than failing: a departed customer is
		// not going to change, and a stale-but-complete-enough record beats an
		// outage for someone who is only being looked up.
		//
		// Note this does NOT cover a reaped customer. `make reap` deletes the
		// whole execution record, search attributes included, so those fail at
		// Describe above and surface as a 404 -- see PLAN.md 6.3.
		s.log.Info("query failed for a closed customer, falling back to search attributes",
			"workflowId", wfID, "error", qerr)
		fillFromSearchAttributes(&out, info.GetSearchAttributes())

	default:
		// A running customer that cannot answer is a real failure.
		return qerr
	}

	writeJSON(w, s.log, http.StatusOK, out)
	return nil
}

// queryStatus runs getStatus, retrying once on an unrecognised failure.
//
// The retry exists for a real observation. Querying a customer immediately after
// a point-add that triggered continue-as-new returned:
//
//	Workflow task is not scheduled yet.
//
// The successor run exists but has no workflow task yet, so there is nothing to
// dispatch the query to. It is transient -- the identical request succeeded
// moments later -- and it is a sibling of the update-side rollover race in
// PLAN.md 12.2, which anticipated this for Updates but not for Queries. At three
// adds per run it is reachable by ordinary use.
//
// Retrying only the *unclassified* errors is deliberate. A NotFound will not
// improve, and a worker-unavailable is already a clean 503 whose latency should
// not be doubled by a pointless second attempt. Everything left is a failure we
// could not name, where one more try of an idempotent ~30ms read is cheap
// insurance. This is a bounded retry rather than a matched one because the error
// above could not be reproduced on demand -- see PLAN.md 12.13.
func (s *Server) queryStatus(ctx context.Context, wfID string) (converter.EncodedValue, error) {
	const attempts = 2

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		qctx, cancel := context.WithTimeout(ctx, queryTimeout)
		enc, err := s.temporal.QueryWorkflow(qctx, wfID, "", rewards.QueryGetStatus)
		cancel()
		if err == nil {
			return enc, nil
		}
		lastErr = err

		mapped := mapQueryError(err)
		var apiErr *apiError
		if errors.As(mapped, &apiErr) {
			return nil, mapped // recognised, and retrying will not help
		}
		if attempt < attempts {
			s.log.Info("unrecognised query failure, retrying once",
				"workflowId", wfID, "error", err)
			// Long enough for a successor run's first workflow task to be
			// scheduled, short enough to stay inside a page load.
			time.Sleep(250 * time.Millisecond)
		}
	}
	return nil, mapQueryError(lastErr)
}

// addPoints applies an Update, retrying once if the run rolled over underneath.
func (s *Server) addPoints(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}

	var req AddPointsRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	res, err := s.updateWithRolloverRetry(r.Context(), rewards.WorkflowID(id), req)
	if err != nil {
		return err
	}

	writeJSON(w, s.log, http.StatusOK, AddPointsResponse{
		Balance: res.Balance,
		Level:   res.Level,
		EventID: res.EventID,
	})
	return nil
}

// updateWithRolloverRetry sends the Update, and sends it again if the run it
// addressed closed because of continue-as-new.
//
// This is not defensive coding for a rare event. Continue-as-new fires every 3
// adds, so a client that keeps adding points will hit it regularly -- an update
// losing its run is the *expected* outcome of racing a rollover, and without a
// transparent retry the demo looks broken roughly every third click. PLAN.md 12.2.
//
// The subtlety is that "the run you addressed has closed" arrives as one
// ambiguous NotFound covering two opposite situations: a rollover, where a
// successor is already running and retrying is exactly right, and a
// deactivation, where nothing is running and retrying can never succeed. So we
// ask the server which it is rather than guessing from the message -- an extra
// Describe, only ever on the error path.
//
// Retrying is safe because the update did not run: the run it targeted closed
// before applying it. That safety comes from the abort semantics, not from the
// UpdateID -- Update dedup is scoped to a run, so the ID buys nothing across the
// boundary. PLAN.md 12.3.
func (s *Server) updateWithRolloverRetry(
	ctx context.Context, wfID string, req AddPointsRequest,
) (rewards.AddPointsResult, error) {
	const attempts = 2

	for attempt := 1; attempt <= attempts; attempt++ {
		res, err := s.sendUpdate(ctx, wfID, req)
		if err == nil {
			return res, nil
		}
		if !isClosedRun(err) {
			return rewards.AddPointsResult{}, mapUpdateError(err)
		}

		// The run is gone. Is a successor running, or is the customer?
		running, describeErr := s.hasRunningExecution(ctx, wfID)
		if describeErr != nil {
			return rewards.AddPointsResult{}, mapQueryError(describeErr)
		}
		if !running {
			return rewards.AddPointsResult{}, &apiError{
				http.StatusConflict, CodeDeactivated,
				"customer is deactivated; re-enroll them before adding points",
			}
		}

		s.log.Info("update lost its run to continue-as-new, retrying against the successor",
			"workflowId", wfID, "attempt", attempt)
	}

	// Two rollovers inside one request. Possible under sustained load at three
	// adds per run; an honest 409 beats a retry loop that could chase a busy
	// customer indefinitely.
	s.log.Warn("update lost its run twice in a row", "workflowId", wfID)
	return rewards.AddPointsResult{}, &apiError{
		http.StatusConflict, CodeRolloverRace,
		"the customer's workflow rolled over while applying this request; please retry",
	}
}

// hasRunningExecution reports whether the workflow ID currently has an open
// execution. Used only to disambiguate a closed run, so its cost lands on the
// error path alone.
func (s *Server) hasRunningExecution(ctx context.Context, wfID string) (bool, error) {
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, wfID, "")
	if err != nil {
		return false, err
	}
	return desc.GetWorkflowExecutionInfo().GetStatus() ==
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

// queryTimeout bounds how long a Query may wait for a worker.
//
// Deliberately aggressive, because the failure mode it guards against is not
// stable. Measured against a real server with the worker stopped, a Query that
// nobody answers comes back as any of three different things depending on how
// long ago the worker died:
//
//	FailedPrecondition  "no poller seen for task queue recently"   ~9s
//	DeadlineExceeded    "context deadline exceeded"                ~9s
//	gRPC RST_STREAM     "stream terminated ... error code: CANCEL" ~2.5s
//
// The first two are typed and mapped; the third is a bare transport error that
// would become a 500. Bounding the call at 2s means our own deadline usually
// wins the race, collapsing all three into one predictable 503 -- and a healthy
// query answers in ~30ms, so this leaves roughly 60x headroom. PLAN.md 12.4.
// listTimeout bounds the two visibility calls. Generous next to queryTimeout
// because these read Elasticsearch rather than replaying a workflow, so no
// worker is involved and the no-poller failure mode does not apply.
const listTimeout = 10 * time.Second

const queryTimeout = 2 * time.Second

// updateTimeout bounds how long an Update may wait for a worker.
//
// This is load-bearing, not belt-and-braces. A Query against a task queue with
// no poller fails fast with FailedPrecondition, but an Update with
// WaitForStage: Completed simply *blocks* -- observed still waiting after two
// minutes with the worker stopped. Without this bound, `POST /points` hangs for
// as long as the client will hold the connection whenever the worker is down,
// which during development is often. PLAN.md 12.12.
const updateTimeout = 15 * time.Second

func (s *Server) sendUpdate(
	ctx context.Context, wfID string, req AddPointsRequest,
) (rewards.AddPointsResult, error) {
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	handle, err := s.temporal.UpdateWorkflow(ctx, client.UpdateWorkflowOptions{
		WorkflowID:   wfID,
		UpdateName:   rewards.UpdateAddPoints,
		UpdateID:     req.RequestID, // empty means the SDK generates one
		Args:         []any{rewards.AddPointsRequest{Amount: req.Amount, Reason: req.Reason}},
		WaitForStage: client.WorkflowUpdateStageCompleted,
	})
	if err != nil {
		return rewards.AddPointsResult{}, err
	}

	var res rewards.AddPointsResult
	if err := handle.Get(ctx, &res); err != nil {
		return rewards.AddPointsResult{}, err
	}
	return res, nil
}

// deactivate cancels the workflow. Cancel rather than Terminate so the
// workflow's own departure code runs. PLAN.md 3.6.
//
// Deliberately does NOT disambiguate a closed execution the way the update path
// does, because Cancel and Update disagree about what a closed run means.
// Measured against a real server:
//
//	operation on a closed execution   Canceled   Failed   Terminated   never existed
//	------------------------------    --------   ------   ----------   -------------
//	CancelWorkflow                     nil        nil      nil          NotFound
//	UpdateWorkflow                     NotFound   -        -            NotFound
//
// So Cancel is already idempotent server-side: deactivating a departed customer
// is a successful no-op, and only a workflow ID that never existed produces the
// NotFound that becomes a 404. That is exactly the REST semantics wanted here,
// and it is why this handler is three lines while updateWithRolloverRetry needs
// an extra Describe to decide the same question.
//
// Reviewed on PR #6 as a suspected bug -- repeat DELETE misreporting 404 -- on
// the reasonable assumption that Cancel behaves like Update. It does not.
func (s *Server) deactivate(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}

	if err := s.temporal.CancelWorkflow(r.Context(), rewards.WorkflowID(id), ""); err != nil {
		return mapQueryError(err)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// searchAttrValues is a customer's search attributes, decoded.
//
// Shared by the two callers that read them -- the list, where they are the only
// thing available, and the detail endpoint's degraded path -- so the decoding
// quirks live in one place. The one that bites: CustomerName is registered as
// Text, and the SDK's constructor for Text is NewSearchAttributeKeyString, so
// "String" here means the server's "Text".
type searchAttrValues struct {
	CustomerID string
	Name       string
	Email      string
	Points     int
	Level      string
	EnrolledAt time.Time
	Generation int
}

// decodeSearchAttributes is best-effort by design: a missing or undecodable
// attribute leaves its field at the zero value rather than failing the request.
// Both callers would rather serve a partial record than a 500, and neither can
// do anything about a customer whose attributes are incomplete.
func decodeSearchAttributes(sa *commonpb.SearchAttributes) searchAttrValues {
	var out searchAttrValues
	if sa == nil {
		return out
	}
	fields := sa.GetIndexedFields()
	dc := converter.GetDefaultDataConverter()

	decodeStr := func(key string, dst *string) {
		if p, ok := fields[key]; ok {
			_ = dc.FromPayload(p, dst)
		}
	}
	decodeInt := func(key string, dst *int) {
		if p, ok := fields[key]; ok {
			var v int64
			if err := dc.FromPayload(p, &v); err == nil {
				*dst = int(v)
			}
		}
	}

	decodeStr(rewards.KeyCustomerID.GetName(), &out.CustomerID)
	decodeStr(rewards.KeyCustomerName.GetName(), &out.Name)
	decodeStr(rewards.KeyCustomerEmail.GetName(), &out.Email)
	decodeStr(rewards.KeyRewardsLevel.GetName(), &out.Level)
	decodeInt(rewards.KeyRewardsPoints.GetName(), &out.Points)
	decodeInt(rewards.KeyGeneration.GetName(), &out.Generation)

	if p, ok := fields[rewards.KeyEnrolledAt.GetName()]; ok {
		_ = dc.FromPayload(p, &out.EnrolledAt)
	}
	return out
}

// fillFromSearchAttributes populates the fields a Query would have provided,
// for a closed customer no worker is available to replay.
//
// Recovers everything the detail page shows except LifetimeEarnEvents, which is
// workflow state rather than a registered search attribute (PLAN.md 4 lists the
// set deliberately). Only reached on the degraded path, so that field is
// present whenever the query works -- which is nearly always.
func fillFromSearchAttributes(out *CustomerResponse, sa *commonpb.SearchAttributes) {
	v := decodeSearchAttributes(sa)
	out.Name = v.Name
	out.Email = v.Email
	out.Level = v.Level
	out.Points = v.Points
	out.Generation = v.Generation
	out.EnrolledAt = v.EnrolledAt

	// Derived rather than stored, so it stays consistent with the balance we
	// just recovered. PLAN.md 3.2.
	out.NextTierAt, _ = rewards.NextTierAt(out.Points)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			return badRequest("request body too large")
		}
		return badRequest("invalid JSON body: " + err.Error())
	}
	return nil
}

// ReadTimeout etc. are set by the caller; exposed so cmd/api stays declarative.
var DefaultTimeouts = struct{ Read, Write, Idle time.Duration }{
	Read: 10 * time.Second, Write: 30 * time.Second, Idle: 60 * time.Second,
}
