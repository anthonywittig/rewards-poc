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
// every failure goes through one mapping path.
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
// Without a customerId in the body, the server derives one from the name; the
// same name always derives the same ID, so a second signup under one name is a
// duplicate (409) or a rejoin, never a second customer.
func (s *Server) enroll(w http.ResponseWriter, r *http.Request) error {
	var req EnrollRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	req.CustomerID = strings.TrimSpace(req.CustomerID)
	if strings.ContainsAny(req.CustomerID, " \t\n/") {
		return badRequest("customerId must not contain whitespace or slashes")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return badRequest("name is required")
	}
	if req.CustomerID == "" {
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

	// The ID is taken. Active -> 409. Soft-deactivated -> reactivate, which
	// restores the prior balance.
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
	// Changed=false means a concurrent enroll won the race; still a duplicate.
	if !res.Changed {
		return &apiError{http.StatusConflict, CodeAlreadyExists,
			"customer is already enrolled and active"}
	}

	desc, derr := s.temporal.DescribeWorkflowExecution(r.Context(), wfID, "")
	runID := ""
	if derr == nil {
		runID = desc.GetWorkflowExecutionInfo().GetExecution().GetRunId()
	} else {
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

// listCustomers serves the customer list straight out of the visibility store:
// a ListWorkflow plus a CountWorkflow, no lookup table.
func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) error {
	userQuery := strings.TrimSpace(r.URL.Query().Get("q"))

	// Caught here only for the error message: wrapped in parentheses below,
	// Temporal's clear "ORDER BY not supported" becomes a bare syntax error.
	if hasOrderBy(userQuery) {
		return badRequest("ORDER BY is not supported by Temporal's visibility store; " +
			"filter to narrow the result set and sort client-side")
	}

	query := scopedQuery(userQuery)

	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()

	// Count first: it is the call most likely to reject a malformed user query.
	total := -1
	if cnt, err := s.temporal.CountWorkflow(ctx, &workflowservice.CountWorkflowExecutionsRequest{
		Query: query,
	}); err != nil {
		if apiErr := mapListError(err, userQuery); apiErr != nil {
			return apiErr
		}
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
		return mapStoreReadError(err)
	}

	items := make([]CustomerListItem, 0, len(resp.GetExecutions()))
	for _, e := range resp.GetExecutions() {
		v := decodeSearchAttributes(e.GetSearchAttributes())

		// Membership is the RewardsActive attribute, not ExecutionStatus:
		// soft-deactivated customers are still Running.
		status := "deactivated"
		switch {
		case v.Active != nil && *v.Active:
			status = "active"
		case v.Active == nil && e.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING:
			status = "active"
		}

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
		Query:    userQuery,
	})
	return nil
}

// hasOrderBy is a plain substring test -- good enough for a friendly error.
func hasOrderBy(q string) bool {
	return strings.Contains(strings.ToLower(q), "order by")
}

// scopedQuery constrains the list to our workflow type and to one execution
// per customer. The visibility store holds one document per *run*, so a
// customer who has continued-as-new twice appears three times; excluding
// ContinuedAsNew leaves exactly the current generation.
func scopedQuery(userQuery string) string {
	scope := "WorkflowType = '" + rewards.WorkflowTypeName + "'" +
		" AND ExecutionStatus != 'ContinuedAsNew'"
	if userQuery == "" {
		return scope
	}
	// Parenthesised so an OR in the caller's filter cannot escape the scope.
	return scope + " AND (" + userQuery + ")"
}

// mapListError turns a rejected visibility query into a 400 carrying
// Temporal's own diagnostics, and returns nil for anything else.
func mapListError(err error, userQuery string) error {
	var invalid *serviceerror.InvalidArgument
	if !errors.As(err, &invalid) {
		return nil
	}
	if userQuery == "" {
		// Our own scoping clause was rejected: our bug, not the caller's.
		return &apiError{http.StatusInternalServerError, CodeInternal, "internal error"}
	}
	return &apiError{http.StatusBadRequest, CodeInvalidRequest, invalid.Error()}
}

// getCustomer reads current state via the getStatus Query. Search attributes
// could answer most of this without a worker, but they lag writes; the Query
// reads the workflow's own state.
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

// queryStatus runs getStatus, retrying once on an unrecognised failure --
// typically "Workflow task is not scheduled yet." right after a
// continue-as-new, when the successor run has nothing to dispatch the query to.
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
// the run it addressed closed because of continue-as-new. Rollover fires every
// EarnsPerRun adds, so without this retry the demo looks broken roughly every
// third click. Retrying is safe because the lost Update never ran.
//
// A closed run with no successor means a force-closed execution, which is
// refused; product deactivation rejects inside the handler instead.
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

	// Two rollovers inside one request: report honestly rather than chase.
	s.log.Warn("update lost its run twice in a row", "workflowId", wfID)
	return zero, &apiError{
		http.StatusConflict, CodeRolloverRace,
		"the customer's workflow rolled over while applying this request; please retry",
	}
}

func (s *Server) hasRunningExecution(ctx context.Context, wfID string) (bool, error) {
	desc, err := s.temporal.DescribeWorkflowExecution(ctx, wfID, "")
	if err != nil {
		return false, err
	}
	return desc.GetWorkflowExecutionInfo().GetStatus() ==
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING, nil
}

// isActive asks the workflow itself rather than visibility: enroll reactivates
// on false, and that decision must not rest on a store that lags writes.
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

// listTimeout bounds the two visibility calls, which never involve a worker.
const listTimeout = 10 * time.Second

// queryTimeout bounds how long a Query may wait for a worker. Deliberately
// aggressive: with the worker stopped the SDK fails in several shapes on its
// own schedule, and bounding at 2s collapses them into one predictable 503. A
// healthy query answers in ~30ms.
const queryTimeout = 2 * time.Second

// updateTimeout bounds how long an Update may wait for a worker. Load-bearing:
// an Update with no worker does not fail, it blocks indefinitely.
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

// reactivate sends the reactivate Update.
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
// Idempotent: a repeat DELETE completes with Changed=false.
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
			// A rollover consumed the Update; report rather than retry.
			return &apiError{
				http.StatusConflict, CodeRolloverRace,
				"the customer's workflow rolled over while applying this request; please retry",
			}
		default:
			err = nil // force-closed: already as gone as deactivation gets
		}
	}
	if err != nil {
		return mapUpdateError(err)
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

// searchAttrValues is a customer's search attributes, decoded.
type searchAttrValues struct {
	CustomerID string
	Name       string
	Points     int
	Level      string
	EnrolledAt time.Time
	Generation int
	Active     *bool // nil when the attribute was never upserted
}

// decodeSearchAttributes is best-effort: a missing or undecodable attribute
// leaves its field at the zero value rather than failing the request.
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

// DefaultTimeouts is applied by cmd/api when building the http.Server.
var DefaultTimeouts = struct{ Read, Write, Idle time.Duration }{
	Read: 10 * time.Second, Write: 30 * time.Second, Idle: 60 * time.Second,
}
