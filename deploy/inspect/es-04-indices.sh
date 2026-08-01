#!/usr/bin/env bash
# index size for our handful of customers — ES-is-overkill, made concrete.
# Run via: make inspect-es Q=indices
#
# Caveat: after make reap, docs.count from _cat/indices can stay high until
# Lucene merges drop soft-deleted docs. Prefer _search/_count for truth.

set -euo pipefail

ES="${ES:-http://elasticsearch:9200}"

echo "=== temporal visibility indices ==="
curl -s "${ES}/_cat/indices/temporal_visibility*?v&h=index,docs.count,store.size,docs.deleted"
echo
echo "=== searchable doc count (ignores soft-deletes still awaiting merge) ==="
curl -s "${ES}/temporal_visibility_v1_dev/_count?pretty"
