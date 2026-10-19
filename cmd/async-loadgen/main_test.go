package main

import (
	"testing"
	"time"
)

func TestNearestRank(t *testing.T) {
	values := []int64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	for _, test := range []struct {
		percentile int
		want       int64
	}{{50, 50}, {95, 100}, {99, 100}} {
		if got := nearestRank(values, test.percentile); got != test.want {
			t.Errorf("P%d = %d, want %d", test.percentile, got, test.want)
		}
	}
}

func TestCalculatePercentilesDoesNotMutateInput(t *testing.T) {
	values := []int64{100, 1, 10, 50}
	got := calculatePercentiles(values)
	if got.Count != 4 || *got.P50 != 10 || *got.P95 != 100 || *got.P99 != 100 {
		t.Fatalf("percentiles = %+v", got)
	}
	if values[0] != 100 {
		t.Fatalf("input was sorted in place: %v", values)
	}
}

func TestSummarizeCountsTerminalAndFailures(t *testing.T) {
	rowLatency, observedLatency := int64(42), int64(47)
	got := summarize(testTime, testTime, runConfig{ActivityID: 7, Requests: 4, Concurrency: 2}, []*requestResult{
		{HTTPStatus: 202, TerminalStatus: "SUCCEEDED", AcceptedToOrderRowCreatedUS: &rowLatency, AcceptedToTerminalObservedUS: &observedLatency, TerminalPollErrors: 2},
		{HTTPStatus: 409, TerminalStatus: "HTTP_REJECTED", FailureCode: "PURCHASE_CONFLICT"},
		{HTTPStatus: 202, TerminalStatus: "FAILED", FailureCode: "SOLD_OUT"},
		{HTTPStatus: 202, TerminalStatus: "UNFINISHED"},
	})
	if got.HTTPStatus["202"] != 3 || got.Terminal["SUCCEEDED"] != 1 || got.Failures["PURCHASE_CONFLICT"] != 1 || got.Unfinished != 1 || got.ExactFailureTimesUnavailable != 1 || got.AcceptedToOrderRowCreatedUS.Count != 1 || got.AcceptedToTerminalObservedUS.Count != 1 || got.TerminalPollErrors != 2 {
		t.Fatalf("summary = %+v", got)
	}
}

func TestOutputPathsAreDeterministic(t *testing.T) {
	requests, summary := outputPaths("artifacts/async-run")
	if requests != "artifacts/async-run/requests.jsonl" || summary != "artifacts/async-run/summary.json" {
		t.Fatalf("paths = %q, %q", requests, summary)
	}
}

var testTime = mustTime("2026-09-22T00:00:00Z")

func mustTime(raw string) (value time.Time) {
	var err error
	value, err = time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return value
}
