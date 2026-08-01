-- finding 4: retention / make reap is visible at the storage layer.
-- Closed runs (ContinuedAsNew, Canceled, …) lose their executions + history_node
-- rows after delete propagates (~25–40s). The Running run for the same
-- workflow_id is untouched — that is why reap.sh filters
-- ExecutionStatus != "Running".
--
-- Usage:
--   make inspect-pg Q=after-reap ID=inspect          # before
--   make reap WF=customer-inspect                    # deletes closed gens only
--   # wait ~30s
--   make inspect-pg Q=after-reap ID=inspect          # after: one row left if still Running

\pset null '∅'
\echo '=== executions retained for this workflow_id ==='

SELECT workflow_id,
       encode(run_id, 'hex') AS run_id_hex,
       next_event_id
FROM executions
WHERE workflow_id = :'wf'
ORDER BY next_event_id;

\echo
\echo '=== history_node rows still reachable from those runs ==='

SELECT e.workflow_id,
       encode(e.run_id, 'hex') AS run_id_hex,
       count(h.*)              AS history_nodes,
       coalesce(sum(length(h.data)), 0) AS history_bytes
FROM executions e
LEFT JOIN history_node h ON h.tree_id = e.run_id
WHERE e.workflow_id = :'wf'
GROUP BY 1, 2
ORDER BY 1, 2;

\echo
\echo '=== current pointer (should still resolve if the customer is active) ==='

SELECT workflow_id,
       encode(run_id, 'hex') AS current_run_id_hex,
       state,
       status
FROM current_executions
WHERE workflow_id = :'wf';
