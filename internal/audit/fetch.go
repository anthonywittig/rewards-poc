package audit

import (
	"context"

	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/sdk/client"
)

// Fetcher reads one run's events.
type Fetcher func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error)

// HistorySource is the GetWorkflowHistory half of a Temporal client.
type HistorySource interface {
	GetWorkflowHistory(
		ctx context.Context,
		workflowID string,
		runID string,
		isLongPoll bool,
		filterType enumspb.HistoryEventFilterType,
	) client.HistoryEventIterator
}

// NewFetcher reads runs of workflowID via the Temporal client.
// isLongPoll is always false so a live workflow does not hang the crawl
// waiting for future events.
func NewFetcher(src HistorySource, workflowID string) Fetcher {
	return func(ctx context.Context, runID string) ([]*historypb.HistoryEvent, error) {
		iter := src.GetWorkflowHistory(ctx, workflowID, runID, false,
			enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)

		var events []*historypb.HistoryEvent
		for iter.HasNext() {
			e, err := iter.Next()
			if err != nil {
				return nil, err
			}
			events = append(events, e)
		}
		return events, nil
	}
}
