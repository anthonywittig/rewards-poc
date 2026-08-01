-- finding 3: visibility_tasks is the async queue feeding Elasticsearch.
-- At rest it is usually empty — the worker drains it within the flush interval
-- (~ESProcessorFlushInterval, see dynamicconfig/dev.yaml). To catch a row in
-- flight, poll this query in a tight loop while another terminal runs
-- `make add ID=…`, or use `make write-trace` which does both.
--
-- Usage: make inspect-pg Q=visibility-tasks

\echo '=== visibility_tasks backlog (usually 0; non-zero = ES lag you can watch) ==='

SELECT shard_id,
       task_id,
       length(data)  AS blob_bytes,
       data_encoding
FROM visibility_tasks
ORDER BY shard_id, task_id;

\echo
SELECT count(*) AS pending_visibility_tasks FROM visibility_tasks;
