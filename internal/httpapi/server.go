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
	mux.HandleFunc("POST /api/customers", s.handle(s.enroll))
	mux.HandleFunc("GET /api/customers/{id}", s.handle(s.getCustomer))
	mux.HandleFunc("POST /api/customers/{id}/points", s.handle(s.addPoints))
	mux.HandleFunc("DELETE /api/customers/{id}", s.handle(s.deactivate))
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

// getCustomer reads current state via Query, and liveness via Describe.
//
// Two calls rather than one because they answer different questions: Query asks
// the workflow what it holds, Describe asks the server whether it is still
// running. A cancelled customer cannot answer a Query at all, so Describe has to
// come first to tell "deactivated" apart from "no worker".
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

	// A closed execution cannot answer a Query -- there is no workflow left to
	// ask. Its search attributes survive on the execution record though, and
	// they carry exactly the fields the detail page needs, so a deactivated
	// customer still renders with their final balance and tier instead of a row
	// of zeroes.
	//
	// This is the search attributes earning their keep somewhere other than the
	// customer list: for a departed customer they are the only readable state
	// short of crawling history.
	if status == "deactivated" {
		fillFromSearchAttributes(&out, info.GetSearchAttributes())
		writeJSON(w, s.log, http.StatusOK, out)
		return nil
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
	out.Email = st.Email
	out.Points = st.Points
	out.Level = st.Level
	out.NextTierAt = st.NextTierAt
	out.EnrolledAt = st.EnrolledAt
	out.LifetimeEarnEvents = st.LifetimeEarnEvents
	out.Generation = st.Generation

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

// fillFromSearchAttributes populates the fields a Query would have provided,
// for a customer whose workflow is closed.
//
// Recovers everything the detail page shows except LifetimeEarnEvents, which is
// carried in workflow state but is not a registered search attribute (PLAN.md 4
// lists the set deliberately). It stays zero for a deactivated customer until
// the Phase 5 history crawl can supply it. Adding an eighth attribute purely to
// close that gap would mean a bootstrap change and one more thing to keep in
// sync, for a number nobody reads on a departed customer.
//
// Best-effort by design: a missing or undecodable attribute leaves its field at
// the zero value rather than failing the request. The customer is gone either
// way, and a partial record beats a 500.
func fillFromSearchAttributes(out *CustomerResponse, sa *commonpb.SearchAttributes) {
	if sa == nil {
		return
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

	decodeStr(rewards.KeyCustomerName.GetName(), &out.Name)
	decodeStr(rewards.KeyCustomerEmail.GetName(), &out.Email)
	decodeStr(rewards.KeyRewardsLevel.GetName(), &out.Level)
	decodeInt(rewards.KeyRewardsPoints.GetName(), &out.Points)
	decodeInt(rewards.KeyGeneration.GetName(), &out.Generation)

	if p, ok := fields[rewards.KeyEnrolledAt.GetName()]; ok {
		_ = dc.FromPayload(p, &out.EnrolledAt)
	}

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
