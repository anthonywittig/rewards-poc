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

// Server is the HTTP surface. The Temporal client is the only dependency;
// ui holds where the Temporal UI lives so responses can carry deep links.
type Server struct {
	temporal client.Client
	log      *slog.Logger
	ui       temporalUI
}

// New builds the server. temporalUIURL is the browser-facing base URL of the
// Temporal UI; namespace must match the one the client dialed.
func New(c client.Client, log *slog.Logger, temporalUIURL, namespace string) *Server {
	return &Server{temporal: c, log: log, ui: newTemporalUI(temporalUIURL, namespace)}
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

// enroll starts a customer's workflow. Without a customerId in the body, the
// server derives one from the name; the same name always derives the same ID,
// so a second signup under one name is a duplicate, never a second customer.
//
// Deactivation is one-way and completes the workflow, so an occupied ID means
// either a Running execution (an active customer -- duplicate signup) or a
// Completed one (a departed customer, whose ID stays retired until their
// history is reaped). ALLOW_DUPLICATE_FAILED_ONLY is what retires it: a
// *failed* execution -- an enrollment payload the workflow refused -- may be
// retried, a completed one may not be restarted.
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
		WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
		WorkflowIDConflictPolicy:                 enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, workflows.CustomerRewardsWorkflow, rewards.CustomerState{
		CustomerID: req.CustomerID,
		Name:       req.Name,
		RunNumber:  1, // run numbers are 1-based; the enrollment run is run 1
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
		return classifyCommon(err)
	}

	// ID is taken. Running -> a duplicate signup. Closed -> the customer left,
	// and deactivation is one-way. Describe reads persistence, so neither
	// answer needs a worker.
	running, derr := s.hasRunningExecution(r.Context(), wfID)
	if derr != nil {
		return mapStoreReadError(derr)
	}
	if running {
		return &apiError{http.StatusConflict, CodeAlreadyExists,
			"customer is already enrolled and active"}
	}
	return &apiError{http.StatusConflict, CodeDeactivated,
		"this customer has been deactivated; deactivation is permanent"}
}

// listCustomers serves the customer list straight out of the visibility store:
// a ListWorkflow plus a CountWorkflow, no lookup table.
//
// Filtering is structured -- ?tier= ?status= ?name= become clauses here, see
// buildListFilter. Every query this sends is one the server built from
// validated values, so a rejection is our bug and surfaces as a logged 500.
func (s *Server) listCustomers(w http.ResponseWriter, r *http.Request) error {
	params := r.URL.Query()

	filter, err := buildListFilter(params.Get("tier"), params.Get("status"), params.Get("name"))
	if err != nil {
		return err
	}
	// Echoed in the response so the UI can show a query that pastes into the
	// Temporal UI unchanged.
	effectiveQuery := strings.Join(filter, " AND ")

	query := scopedQuery(effectiveQuery)

	ctx, cancel := context.WithTimeout(r.Context(), listTimeout)
	defer cancel()

	// Count failures degrade to "of many" rather than failing a list we can
	// still serve.
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
		// mapStoreReadError, not mapQueryError: no worker is involved, so a
		// timeout here must not blame one.
		return mapStoreReadError(err)
	}

	items := make([]CustomerListItem, 0, len(resp.GetExecutions()))
	for _, e := range resp.GetExecutions() {
		v := decodeSearchAttributes(e.GetSearchAttributes())

		// Membership is the RewardsActive attribute, not ExecutionStatus. The
		// completed final run keeps Active=false, which is what puts departed
		// customers in this list until their history is reaped.
		//
		// No ExecutionStatus fallback for a missing attribute: a fresh
		// enrollment that hasn't reached its first upsert briefly reads as
		// deactivated here, but enrollment lands the UI on the detail page,
		// which asks the workflow directly.
		status := "deactivated"
		if v.Active != nil && *v.Active {
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
			RunNumber:  v.RunNumber,
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
		QueryURL: s.ui.queryURL(effectiveQuery),
	})
	return nil
}

// scopedQuery constrains the list to our workflow type and to one execution
// per customer. The visibility store holds one document per *run*, so a
// customer who has continued-as-new twice appears three times; excluding
// ContinuedAsNew leaves exactly the current run -- for a departed
// customer, the Completed run their deactivation closed.
func scopedQuery(userQuery string) string {
	scope := "WorkflowType = '" + rewards.WorkflowTypeName + "'" +
		" AND ExecutionStatus != 'ContinuedAsNew'"
	if userQuery == "" {
		return scope
	}
	// Parenthesised so an OR in the filter cannot escape the scope.
	return scope + " AND (" + userQuery + ")"
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
	out.PrevTierAt = st.PrevTierAt
	out.NextTierAt = st.NextTierAt
	out.EnrolledAt = st.EnrolledAt
	out.LifetimeEarnEvents = st.LifetimeEarnEvents
	out.RunNumber = st.RunNumber
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
// Deactivation completes the workflow, so a closed run with no successor is
// the ordinary shape of a departed customer, and adding points to one is
// refused. Only an add that races the deactivate into its final run is
// rejected inside the Update handler as ErrTypeDeactivated instead.
func (s *Server) addPointsWithRolloverRetry(
	ctx context.Context, wfID string, req AddPointsRequest,
) (rewards.AddPointsResult, error) {
	var zero rewards.AddPointsResult
	const attempts = 2

	for attempt := 1; attempt <= attempts; attempt++ {
		var res rewards.AddPointsResult
		err := sendUpdate(ctx, s.temporal, client.UpdateWorkflowOptions{
			WorkflowID: wfID,
			UpdateName: rewards.UpdateAddPoints,
			UpdateID:   req.RequestID, // empty means the SDK generates one
			Args:       []any{rewards.AddPointsRequest{Amount: req.Amount, Reason: req.Reason}},
		}, &res)
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
				"customer is deactivated; deactivation is permanent",
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

// sendUpdate sends one Update, waits for it to complete, and decodes its
// result into valuePtr (nil when the Update returns nothing).
func sendUpdate(
	ctx context.Context, c client.Client, opts client.UpdateWorkflowOptions, valuePtr any,
) error {
	ctx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	opts.WaitForStage = client.WorkflowUpdateStageCompleted
	handle, err := c.UpdateWorkflow(ctx, opts)
	if err != nil {
		return err
	}
	return handle.Get(ctx, valuePtr)
}

// deactivate ends the customer's membership via Update. One-way: the workflow
// records the leave and then completes; there is no reactivation.
//
// Idempotent: repeating DELETE against a departed customer finds their run
// already closed, and closed is exactly what deactivation leaves behind.
func (s *Server) deactivate(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	if id == "" {
		return badRequest("customer id is required")
	}
	wfID := rewards.WorkflowID(id)

	err := sendUpdate(r.Context(), s.temporal, client.UpdateWorkflowOptions{
		WorkflowID: wfID,
		UpdateName: rewards.UpdateDeactivate,
	}, nil)
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
			err = nil // already closed: as deactivated as it gets, DELETE is idempotent
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
	RunNumber  int
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
	decodeInt(rewards.KeyRunNumber.GetName(), &out.RunNumber)

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
