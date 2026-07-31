-- §8.1 finding 1: history_node is opaque blobs.
-- You cannot SELECT a customer's balance out of Postgres. The Event History is
-- serialized Proto3 batches, not queryable rows — which is why a separate
-- visibility store exists, and why the audit log crawls history through the SDK.
--
-- Usage: make inspect-pg Q=history-blob ID=inspect

\pset null '∅'
\echo '=== history_node blobs (joined via executions.run_id = tree_id) ==='

SELECT e.workflow_id,
       encode(e.run_id, 'hex')              AS run_id_hex,
       h.node_id,
       length(h.data)                       AS blob_bytes,
       h.data_encoding,
       left(encode(h.data, 'hex'), 32)      AS data_hex_prefix
FROM executions e
JOIN history_node h ON h.tree_id = e.run_id
WHERE e.workflow_id = :'wf'
ORDER BY encode(e.run_id, 'hex'), h.node_id;

\echo
\echo '=== same runs: mutable-state blob sizes in executions ==='
\echo '    (CustomerState lives inside state/data — also Proto3, also opaque)'

SELECT workflow_id,
       encode(run_id, 'hex') AS run_id_hex,
       next_event_id,
       length(data)          AS data_bytes,
       length(state)         AS state_bytes,
       data_encoding,
       state_encoding
FROM executions
WHERE workflow_id = :'wf'
ORDER BY next_event_id;
