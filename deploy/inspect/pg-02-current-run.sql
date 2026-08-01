-- finding 2: current_executions is the continue-as-new indirection.
-- workflow_id stays constant ("the customer"); current run_id flips every time
-- a run continues-as-new. That single row is the stable identity the §6.1
-- audit crawl walks backwards through.
--
-- Usage: make inspect-pg Q=current-run ID=inspect
-- After 3 successful adds the run_id here should change; executions keeps a
-- row per closed generation until make reap deletes them.

\pset null '∅'
\echo '=== current_executions: stable workflow_id → current run ==='

SELECT workflow_id,
       encode(run_id, 'hex') AS current_run_id_hex,
       state,                -- 1=Created 2=Running 3=Completed …
       status                -- mirrors workflow execution status enum
FROM current_executions
WHERE workflow_id = :'wf';

\echo
\echo '=== every run still retained for that workflow_id ==='
\echo '    (ContinuedAsNew generations remain until retention / make reap)'

SELECT workflow_id,
       encode(run_id, 'hex') AS run_id_hex,
       next_event_id,
       length(data)          AS data_bytes
FROM executions
WHERE workflow_id = :'wf'
ORDER BY next_event_id DESC;
