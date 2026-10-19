package main

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type testBody struct {
	reader     io.Reader
	closeErr   error
	closeDelay time.Duration
	closed     bool
}

func (body *testBody) Read(p []byte) (int, error) { return body.reader.Read(p) }

func (body *testBody) Close() error {
	body.closed = true
	time.Sleep(body.closeDelay)
	return body.closeErr
}

func TestExecuteRequestMeasuresReadAndCloseAndRecordsFailures(t *testing.T) {
	readFailure := errors.New("body read failed")
	closeFailure := errors.New("body close failed")
	body := &testBody{reader: errorReader{err: readFailure}, closeErr: closeFailure, closeDelay: 15 * time.Millisecond}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: body, Header: make(http.Header)}, nil
	})}

	item := executeRequest(client, "http://loadgen.test/shops/1", 7, 1)
	if !body.closed {
		t.Fatal("response body was not closed after read failed")
	}
	if item.ResponseReadError != readFailure.Error() || item.ResponseCloseError != closeFailure.Error() {
		t.Fatalf("errors = read %q, close %q", item.ResponseReadError, item.ResponseCloseError)
	}
	if item.DurationMicros < 10_000 {
		t.Fatalf("duration %dµs did not include delayed Close", item.DurationMicros)
	}
	if item.completed() {
		t.Fatal("sample with read and close errors must not be completed")
	}
}

func TestSummarizeRunExcludesIncompleteResponsesFromLatency(t *testing.T) {
	result := summarizeRun([]sample{
		{HTTPStatus: http.StatusOK, DurationMicros: 10},
		{HTTPStatus: http.StatusInternalServerError, DurationMicros: 20},
		{HTTPStatus: http.StatusOK, DurationMicros: 30, ResponseReadError: "truncated"},
		{DurationMicros: 40, TransportError: "deadline exceeded"},
	}, time.Second)

	if result.CompletedRequests != 2 || result.IncompleteRequests != 2 {
		t.Fatalf("completed/incomplete = %d/%d, want 2/2", result.CompletedRequests, result.IncompleteRequests)
	}
	if result.AllCompleted.Count != 2 || result.AllCompleted.P50US != 10 {
		t.Fatalf("all completed = %#v, want only 10 and 20", result.AllCompleted)
	}
	if result.Successful200.Count != 1 || result.Successful200.P99US != 10 {
		t.Fatalf("successful 200 = %#v, want only complete 200", result.Successful200)
	}
	if result.AttemptedThroughputRPS != 4 || result.CompletedThroughputRPS != 2 {
		t.Fatalf("throughput = attempted %.1f, completed %.1f", result.AttemptedThroughputRPS, result.CompletedThroughputRPS)
	}
	if result.TransportErrors != 1 || result.ResponseReadErrors != 1 || result.HTTPStatuses["200"] != 2 {
		t.Fatalf("error/status accounting = %#v", result)
	}
}

func TestChooseShopIsDeterministicAndMatchesHotRatio(t *testing.T) {
	const (
		firstID = 1000
		total   = 100
		hot     = 10
	)
	hotRequests := 0
	for index := 0; index < 100; index++ {
		first := chooseShop(index, firstID, total, hot, 80)
		second := chooseShop(index, firstID, total, hot, 80)
		if first != second {
			t.Fatalf("index %d produced unstable shop ids %d and %d", index, first, second)
		}
		if first < firstID+hot {
			hotRequests++
		}
	}
	if hotRequests != 80 {
		t.Fatalf("hot requests = %d, want 80", hotRequests)
	}
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestNearestRank(t *testing.T) {
	values := []int64{1, 2, 3, 4}
	if got := nearestRank(values, 95); got != 4 {
		t.Fatalf("P95 = %d, want 4", got)
	}
	if got := nearestRank(values, 50); got != 2 {
		t.Fatalf("P50 = %d, want 2", got)
	}
}

func TestLoadSettingsClearlySeparatesModes(t *testing.T) {
	if mode, seconds := (loadSettings{requests: 20_000, concurrency: 100}).metadata(); mode != "fixed_requests" || seconds != 0 {
		t.Fatalf("request mode metadata = %q, %d", mode, seconds)
	}
	if mode, seconds := (loadSettings{requests: 20_000, concurrency: 100, duration: 60 * time.Second}).metadata(); mode != "fixed_duration" || seconds != 60 {
		t.Fatalf("duration mode metadata = %q, %d", mode, seconds)
	}
}

func TestCollectSamplesSortsByGloballyUniqueIndex(t *testing.T) {
	items := make(chan sample, 3)
	items <- sample{Index: 7}
	items <- sample{Index: 2}
	items <- sample{Index: 5}
	close(items)
	got := collectSamples(items)
	if len(got) != 3 || got[0].Index != 2 || got[1].Index != 5 || got[2].Index != 7 {
		t.Fatalf("sorted samples = %#v", got)
	}
}

func TestDurationModeDrainsResultsWhileItSubmits(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("ok")), Header: make(http.Header)}, nil
	})}
	const concurrency = 10
	samples := runLoad(client, "http://loadgen.test", 1, 100, 10, 80, loadSettings{
		concurrency: concurrency,
		duration:    35 * time.Millisecond,
	})
	if len(samples) <= 2*concurrency {
		t.Fatalf("duration mode submitted only %d samples; results were likely not drained concurrently", len(samples))
	}
	for index, item := range samples {
		if item.Index != index {
			t.Fatalf("samples are not sorted/unique at %d: %#v", index, item)
		}
	}
}
