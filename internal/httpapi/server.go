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
	"github.com/anthonywittig/rewards-poc/internal/rewards/workflows"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
)

// Server is the HTTP surface. The Temporal client is the only dependency.
type Server struct {
	temporal client.Client
	log      *slog.Logger
}

// New builds the server.
func New(c client.Client, log *slog.Logger) *Server {
	return &Server{temporal: c, log: log}
}

// Routes returns the mux.
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

// enroll starts a customer's workflow, or reactivates a soft-deactivated one.
//
// The ID is the caller's only when they send one. A signup from the UI does
// not: it sends a name, and the server derives the ID from it
// (rewards.CustomerIDForName).
//
// Re-enrollment keeps the existing balance: the workflow ID is still occupied,
// so we Update rather than Start, and Deactivated flips back to false with
// Points untouched. That path needs the ID, which is why the endpoint still
// takes one -- the detail page's Rejoin button sends the ID it is looking at.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) error {
	var req EnrollRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.CustomerID = strings.TrimSpace(req.CustomerID)
	if strings.ContainsAny(req.CustomerID, " \t\n/") {
		return badRequest("customerId must not contain whitespace or slashes")
	}
	// Required whether or not the ID is derived from it. A caller who sends
	// their own customerId would otherwise start a nameless customer, which the
	// workflow refuses anyway -- as a failed execution rather than this 400.
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return badRequest("name is required")
	}
	if req.CustomerID == "" {
		// Derived, not minted: the same name derives the same ID every time, so
		// a second enrollment under one name falls into the duplicate/rejoin
		// path below rather than starting a second customer.
		req.CustomerID = rewards.CustomerIDForName(req.Name)
		if req.CustomerID == "" {
			return badRequest("name must contain letters or digits; " +
				"the customer ID is derived from it")
		}
	}

	wfID := rewards.WorkflowID(req.CustomerID)
	run, err := s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:                                       wfID,
		TaskQueue:                                rewards.TaskQueue,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, workflows.CustomerRewardsWorkflow, rewards.CustomerState{
		CustomerID: req.CustomerID,
		Name:       req.Name,
	})
	if err == nil {
		writeJSON(w, s.log, http.StatusCreated, EnrollResponse{
			CustomerID: req.CustomerID,
			WorkflowID: run.GetID(),
			RunID:      run.GetRunID(),
		})
		return nil
	}

	var already *serviceerror.WorkflowExecutionAlreadyStarted
	if !errors.As(err, &already) {
		return mapStartError(err)
	}

	// ID is taken. Active → 409. Soft-deactivated → reactivate and restore.
	active, aerr := s.isActive(r.Context(), wfID)
	if aerr != nil {
		return aerr
	}
	if active {
		return &apiError{http.StatusConflict, CodeAlreadyExists,
			"customer is already enrolled and active"}
	}

	res, err := s.reactivate(r.Context(), wfID)
	if err != nil {
		return err
	}
	// The customer went active between the check above and the Update -- a
	// concurrent enroll won. The handler reports that rather than restarting a
	// membership that never ended, so this is still the duplicate 409.
	if !res.Changed {
		return &apiError{http.StatusConflict, CodeAlreadyExists,
			"customer is already enrolled and active"}
	}

	desc, derr := s.temporal.DescribeWorkflowExecution(r.Context(), wfID, "")
	runID := ""
	if derr == nil {
		runID = desc.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	} else {
		// Not fatal: the reactivation landed, and the run ID is a convenience
		// rather than part of the outcome.
		s.log.Warn("reactivated, but describe failed so the response carries no runId",
			"workflowId", wfID, "error", derr)
	}

	writeJSON(w, s.log, http.StatusOK, EnrollResponse{
		CustomerID: req.CustomerID,
		WorkflowID: wfID,
		RunID:      runID,
	})
	return nil
}

// listCustomers serves the customer list straight out of the visibility store.
// Capped at ListLimit with no pagination -- see the note on ListLimit.
//
// Filtering is structured -- ?tier= ?status= ?name= become clauses here, see
// buildListFilter. There is no raw-query param: every query this sends is one
// the server built from validated values, so a rejection is our bug and
// surfaces as a logged 500 rather than a 400 blaming the caller.
func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) error {
	params := r.URL.Query()

	filter, err := buildListFilter(params.Get("tier"), params.Get("status"), params.Get("name"))
	if err != nil {
		return err
	}
	// The filter, echoed in the response so the UI can show the query it can
	// paste into the Temporal UI without building it itself.
	effectiveQuery := strings.Join(filter, " AND ")

	query := scopedQuery(effectiveQuery)

	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()

	// Count failures degrade to "of many" rather than failing a list we can
	// still serve; a query rejection fails the ListWorkflow below anyway.
	total := -1
	if cnt, err := s.temporal.CountWorkflow(ctx, &workflowservice.CountWorkflowExecutionsRequest{
		Query: query,
	}); err != nil {
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
		// Not mapQueryError: this read never involves a worker, so a timeout
		// here must not send anyone to go and restart one. An InvalidArgument
		// -- the server built a bad query -- falls through to a logged 500.
		return mapStoreReadError(err)
	}

	items := make([]CustomerListItem, 0, len(resp.GetExecutions()))
	for _, e := range resp.GetExecutions() {
		v := decodeSearchAttributes(e.GetSearchAttributes())

		// Soft-inactive: membership is RewardsActive. Deactivate fails the
		// Update if the upsert cannot land, so Active=false is present for
		// every soft leave. Nil Active + Running is only the pre-deploy case.
		status := "deactivated"
		switch {
		case v.Active != nil && *v.Active:
			status = "active"
		case v.Active == nil && e.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
			status = "active"
		}

		// Derive from the workflow ID if CustomerId is somehow absent -- the ID
		// is the real identity and is always present.
		id := v.CustomerID
		if id == "" {
			id = strings.TrimPrefix(e.GetExecution().GetWorkflowId(), rewards.WorkflowIDPrefix)
		}

		items = append(items, CustomerListItem{
			CustomerID: id,
			Name:       v.Name,
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
		Query:    effectiveQuery,
	})
	return nil
}

// scopedQuery constrains the list to our workflow type, and to one execution
// per customer.
//
// The second half is not an optimisation, it is a correctness fix. **The
// visibility store holds one document per Run, not per Workflow ID**, so a
// customer who has continued-as-new twice appears three times, each with the
// balance its generation froze:
//
//	WorkflowId = 'customer-dup-check'                          Total: 3
//	WorkflowId = 'customer-dup-check' AND status != CAN        Total: 1
//
// Excluding ContinuedAsNew leaves exactly the current generation. Soft-inactive
// customers are still Running; membership is RewardsActive, not ExecutionStatus.
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

// getCustomer reads current state via Query. Status is active only when the
// execution is still Running and soft-inactive says Active.
//
// The Query is the only source, including for customers who have left and for
// closed executions. Search attributes would answer most of this without a
// worker, but they lag writes and carry no LifetimeEarnEvents, so a page built
// from them looks ordinary while being stale and short a field. Better to fail
// and say which worker is missing.
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
	running := info.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING

	out := CustomerResponse{
		CustomerID: id,
		Status:     "deactivated",
		RunID:      info.GetExecution().GetRunId(),
	}

	enc, err := s.queryStatus(r.Context(), wfID)
	if err != nil {
		return err
	}

	var st rewards.CustomerStatus
	if err := enc.Get(&st); err != nil {
		return err
	}
	out.Name = st.Name
	out.Points = st.Points
	out.Level = st.Level
	out.NextTierAt = st.NextTierAt
	out.Tiers = st.Tiers
	out.EnrolledAt = st.EnrolledAt
	out.LifetimeEarnEvents = st.LifetimeEarnEvents
	out.Generation = st.Generation
	if running && st.Active {
		out.Status = "active"
	}

	writeJSON(w, s.log, http.StatusOK, out)
	return nil
}

// queryStatus runs getStatus, retrying once on an unrecognised failure.
//
// Querying immediately after a point-add that triggered continue-as-new returns
// "Workflow task is not scheduled yet.": the successor run exists but has
// nothing to dispatch the query to. Transient, and reachable by ordinary use at
// three adds per run.
//
// Only *unclassified* errors retry: a NotFound will not improve, and a
// worker-unavailable is already a clean 503 whose latency should not be doubled.
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

	res, err := s.addPointsWithRolloverRetry(r.Context(), rewards.WorkflowID(id), req)
	if err != nil {
		return err
	}

	writeJSON(w, s.log, http.StatusOK, AddPointsResponse{
		Balance: res.Balance,
		Level:   res.Level,
	})
	return nil
}

// addPointsWithRolloverRetry sends the addPoints Update, and sends it again if
// the run it addressed closed because of continue-as-new.
//
// Not defensive coding for a rare event: continue-as-new fires every 3 adds, so
// without a transparent retry the demo looks broken roughly every third click.
// Only addPoints carries the retry -- rollover is driven by point-adds, so it
// is the one verb that routinely races itself; a membership Update losing its
// run needs concurrent adds in the same instant, which this single-user demo
// never produces.
//
// Soft-inactive customers stay Running, so a closed-run NotFound with no
// successor means a force-closed execution, and adding points to it is refused.
// Product deactivation rejects inside the Update handler as ErrTypeDeactivated
// instead.
//
// Retrying is safe because the update did not run -- the run it targeted closed
// before applying it. That safety comes from the abort semantics, not from the
// UpdateID, which buys nothing across a run boundary.
func (s *Server) addPointsWithRolloverRetry(
	ctx context.Context, wfID string, req AddPointsRequest,
) (rewards.AddPointsResult, error) {
	var zero rewards.AddPointsResult
	const attempts = 2

	for attempt := 1; attempt <= attempts; attempt++ {
		res, err := sendUpdate[rewards.AddPointsResult](ctx, s.temporal, client.UpdateWorkflowOptions{
			WorkflowID: wfID,
			UpdateName: rewards.UpdateAddPoints,
			UpdateID:   req.RequestID, // empty means the SDK generates one
			Args:       []any{rewards.AddPointsRequest{Amount: req.Amount, Reason: req.Reason}},
		})
		if err == nil {
			return res, nil
		}
		if !isClosedRun(err) {
			return zero, mapUpdateError(err)
		}

		// The run is gone. Is a successor running, or is the customer?
		running, describeErr := s.hasRunningExecution(ctx, wfID)
		if describeErr != nil {
			return zero, mapQueryError(describeErr)
		}
		if !running {
			return zero, &apiError{
				http.StatusConflict, CodeDeactivated,
				"customer workflow is closed; re-enroll them before adding points",
			}
		}

		s.log.Info("update lost its run to continue-as-new, retrying against the successor",
			"workflowId", wfID, "attempt", attempt)
	}

	// Two rollovers inside one request. An honest 409 beats a retry loop that
	// could chase a busy customer indefinitely.
	s.log.Warn("update lost its run twice in a row", "workflowId", wfID)
	return zero, &apiError{
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

// isActive answers "is this customer currently enrolled and active", from the
// workflow's own word and nothing else.
//
// Deliberately not answerable from visibility, however tempting: enroll
// *reactivates* on a false, rewriting a live customer's name, so this
// is the question here whose wrong answer is destructive and the last one to
// settle from a store that lags writes. An error the caller cannot act on is
// both the honest answer and the safe one.
func (s *Server) isActive(ctx context.Context, wfID string) (bool, error) {
	enc, err := s.queryStatus(ctx, wfID)
	if err != nil {
		return false, err
	}
	var st rewards.CustomerStatus
	if err := enc.Get(&st); err != nil {
		return false, err
	}
	return st.Active, nil
}

// listTimeout bounds the two visibility calls. Generous next to queryTimeout
// because these read Elasticsearch rather than replaying a workflow, so no
// worker is involved and the no-poller failure mode does not apply.
const listTimeout = 10 * time.Second

// queryTimeout bounds how long a Query may wait for a worker.
//
// Deliberately aggressive. With the worker stopped, an unanswered Query comes
// back as any of three things depending on how long ago the worker died:
//
//	FailedPrecondition  "no poller seen for task queue recently"   ~9s
//	DeadlineExceeded    "context deadline exceeded"                ~9s
//	gRPC RST_STREAM     "stream terminated ... error code: CANCEL" ~2.5s
//
// The third is a bare transport error that would become a 500. Bounding at 2s
// means our own deadline usually wins, collapsing all three into one predictable
// 503; a healthy query answers in ~30ms.
const queryTimeout = 2 * time.Second

// updateTimeout bounds how long an Update may wait for a worker.
//
// Load-bearing, not belt-and-braces: a Query with no poller fails fast, but an
// Update with WaitForStage: Completed simply *blocks* -- observed still waiting
// after two minutes. Without this bound `POST /points` hangs for as long as the
// client holds the connection.
const updateTimeout = 15 * time.Second

// sendUpdate sends one Update, waits for it to complete, and decodes its result.
func sendUpdate[T any](
	ctx context.Context, c client.Client, opts client.UpdateWorkflowOptions,
) (T, error) {
	var res T

	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	opts.WaitForStage = client.WorkflowUpdateStageCompleted
	handle, err := c.UpdateWorkflow(ctx, opts)
	if err != nil {
		return res, err
	}
	err = handle.Get(ctx, &res)
	return res, err
}

// reactivate sends the reactivate Update. No rollover retry: enroll only calls
// this after ExecuteWorkflow reported a running execution, so a closed-run
// NotFound means the workflow closed in the instant since -- there is nothing
// left to update, and the honest answer is "enroll again".
func (s *Server) reactivate(ctx context.Context, wfID string) (rewards.ReactivateResult, error) {
	res, err := sendUpdate[rewards.ReactivateResult](ctx, s.temporal, client.UpdateWorkflowOptions{
		WorkflowID: wfID,
		UpdateName: rewards.UpdateReactivate,
	})
	switch {
	case err == nil:
		return res, nil
	case isClosedRun(err):
		return rewards.ReactivateResult{}, &apiError{
			http.StatusConflict, CodeDeactivated,
			"customer workflow is closed; enroll them again to start fresh",
		}
	default:
		return rewards.ReactivateResult{}, mapUpdateError(err)
	}
}

// deactivate soft-leaves the customer via Update. The workflow stays Running
// with Deactivated set so re-enrollment can restore the prior balance.
//
// Idempotent: repeating DELETE against an already-deactivated customer completes
// with Changed=false (no extra audit row). A closed-run NotFound takes one
// Describe to resolve: a customer who never enrolled is the Describe's own 404,
// and a force-closed execution is already as gone as deactivation gets, so
// DELETE has nothing left to do.
func (s *Server) deactivate(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}
	wfID := rewards.WorkflowID(id)

	_, err := sendUpdate[rewards.DeactivateResult](r.Context(), s.temporal, client.UpdateWorkflowOptions{
		WorkflowID: wfID,
		UpdateName: rewards.UpdateDeactivate,
	})
	if err != nil && isClosedRun(err) {
		running, describeErr := s.hasRunningExecution(r.Context(), wfID)
		switch {
		case describeErr != nil:
			return mapQueryError(describeErr)
		case running:
			// The targeted run closed with a successor already going: a rollover
			// consumed it, which takes concurrent point-adds this single-user
			// demo never produces. Reported honestly rather than retried.
			return &apiError{
				http.StatusConflict, CodeRolloverRace,
				"the customer's workflow rolled over while applying this request; please retry",
			}
		default:
			err = nil // force-closed: already deactivated, DELETE is idempotent
		}
	}
	if err != nil {
		return mapUpdateError(err)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// searchAttrValues is a customer's search attributes, decoded.
//
// The decoding quirk that bites: CustomerName is registered as Text, and the
// SDK's constructor for Text is NewSearchAttributeKeyString -- so "String" here
// means the server's "Text".
type searchAttrValues struct {
	CustomerID string
	Name       string
	Points     int
	Level      string
	EnrolledAt time.Time
	Generation int
	Active     *bool // nil when the attribute was never upserted
}

// decodeSearchAttributes is best-effort by design: a missing or undecodable
// attribute leaves its field at the zero value rather than failing the request.
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
	decodeStr(rewards.KeyRewardsLevel.GetName(), &out.Level)
	decodeInt(rewards.KeyRewardsPoints.GetName(), &out.Points)
	decodeInt(rewards.KeyGeneration.GetName(), &out.Generation)

	if p, ok := fields[rewards.KeyEnrolledAt.GetName()]; ok {
		_ = dc.FromPayload(p, &out.EnrolledAt)
	}
	if p, ok := fields[rewards.KeyActive.GetName()]; ok {
		var active bool
		if err := dc.FromPayload(p, &active); err == nil {
			out.Active = &active
		}
	}
	return out
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
