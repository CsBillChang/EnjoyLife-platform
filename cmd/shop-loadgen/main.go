package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type sample struct {
	Index              int    `json:"index"`
	ShopID             int    `json:"shop_id"`
	DurationMicros     int64  `json:"duration_microseconds"`
	HTTPStatus         int    `json:"http_status"`
	TransportError     string `json:"transport_error,omitempty"`
	ResponseReadError  string `json:"response_read_error,omitempty"`
	ResponseCloseError string `json:"response_close_error,omitempty"`
}

type percentileSummary struct {
	Count int   `json:"count"`
	P50US int64 `json:"p50_microseconds"`
	P95US int64 `json:"p95_microseconds"`
	P99US int64 `json:"p99_microseconds"`
}

type summary struct {
	RunID                  string            `json:"run_id"`
	BaseURL                string            `json:"base_url"`
	Requests               int               `json:"requests"`
	Concurrency            int               `json:"concurrency"`
	FirstShopID            int               `json:"first_shop_id"`
	ShopCount              int               `json:"shop_count"`
	HotShopCount           int               `json:"hot_shop_count"`
	HotPercent             int               `json:"hot_percent"`
	LoadMode               string            `json:"load_mode"`
	RequestedDurationSecs  int               `json:"requested_duration_seconds,omitempty"`
	ElapsedMillis          int64             `json:"elapsed_milliseconds"`
	AttemptedThroughputRPS float64           `json:"attempted_requests_per_second"`
	CompletedRequests      int               `json:"completed_requests"`
	IncompleteRequests     int               `json:"incomplete_requests"`
	CompletedThroughputRPS float64           `json:"completed_requests_per_second"`
	AllCompleted           percentileSummary `json:"all_completed"`
	Successful200          percentileSummary `json:"successful_200"`
	HTTPStatuses           map[string]int    `json:"http_statuses"`
	TransportErrors        int               `json:"transport_errors"`
	ResponseReadErrors     int               `json:"response_read_errors"`
	ResponseCloseErrors    int               `json:"response_close_errors"`
}

func main() {
	baseURL := envString("BASE_URL", "http://127.0.0.1:18080")
	runID := envString("RUN_ID", time.Now().UTC().Format("20060102T150405Z")+"-shop-query")
	requests := envInt("REQUESTS", 20_000)
	concurrency := envInt("CONCURRENCY", 100)
	firstID := envInt("FIRST_SHOP_ID", 1_000_001)
	shopCount := envInt("SHOP_COUNT", 10_000)
	hotCount := envInt("HOT_SHOP_COUNT", 100)
	hotPercent := envInt("HOT_PERCENT", 80)
	durationSeconds := envInt("DURATION_SECONDS", 0)
	timeout := time.Duration(envInt("TIMEOUT_MILLISECONDS", 2_000)) * time.Millisecond
	if requests < 1 || concurrency < 1 || firstID < 1 || shopCount < 2 || hotCount < 1 || hotCount >= shopCount || hotPercent < 0 || hotPercent > 100 || durationSeconds < 0 {
		log.Fatal("invalid load configuration")
	}

	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          concurrency * 2,
		MaxIdleConnsPerHost:   concurrency * 2,
		MaxConnsPerHost:       concurrency * 2,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: timeout,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: timeout}
	started := time.Now()
	load := loadSettings{requests: requests, concurrency: concurrency, duration: time.Duration(durationSeconds) * time.Second}
	samples := runLoad(client, baseURL, firstID, shopCount, hotCount, hotPercent, load)
	elapsed := time.Since(started)

	result := summarizeRun(samples, elapsed)
	result.RunID, result.BaseURL = runID, baseURL
	result.Requests, result.Concurrency = requests, concurrency
	if load.durationMode() {
		// In duration mode REQUESTS is not a target. Record the actual number
		// submitted before the deadline, while load_mode preserves that meaning.
		result.Requests = len(samples)
	}
	result.FirstShopID, result.ShopCount = firstID, shopCount
	result.HotShopCount, result.HotPercent = hotCount, hotPercent
	result.LoadMode, result.RequestedDurationSecs = load.metadata()
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(encoded))
	if output := os.Getenv("OUTPUT_DIR"); output != "" {
		if err := writeArtifacts(output, samples, encoded); err != nil {
			log.Fatal(err)
		}
	}
}

type loadSettings struct {
	requests    int
	concurrency int
	duration    time.Duration
}

func (s loadSettings) durationMode() bool { return s.duration > 0 }

func (s loadSettings) metadata() (string, int) {
	if s.durationMode() {
		return "fixed_duration", int(s.duration / time.Second)
	}
	return "fixed_requests", 0
}

// runLoad does not cancel an already-submitted request at a duration boundary:
// it only stops issuing new work, then waits for every submitted response to be
// fully read and closed. Index assignment is atomic and therefore globally
// unique even when duration-mode worker scheduling changes completion order.
func runLoad(client *http.Client, baseURL string, firstID, shopCount, hotCount, hotPercent int, settings loadSettings) []sample {
	jobs := make(chan int)
	results := make(chan sample, settings.concurrency)
	var workers sync.WaitGroup
	for range settings.concurrency {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				shopID := chooseShop(index, firstID, shopCount, hotCount, hotPercent)
				results <- executeRequest(client, fmt.Sprintf("%s/shops/%d", baseURL, shopID), index, shopID)
			}
		}()
	}
	go func() {
		workers.Wait()
		close(results)
	}()

	// Jobs must be produced while results are collected. Waiting to consume
	// results until after closing jobs would fill the bounded results channel,
	// block every worker, then block the producer forever behind those workers.
	go func() {
		defer close(jobs)
		if settings.durationMode() {
			timer := time.NewTimer(settings.duration)
			defer timer.Stop()
			for index := 0; ; index++ {
				select {
				case jobs <- index:
				case <-timer.C:
					return
				}
			}
		}
		for index := 0; index < settings.requests; index++ {
			jobs <- index
		}
	}()
	return collectSamples(results)
}

func collectSamples(results <-chan sample) []sample {
	items := make([]sample, 0)
	for item := range results {
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Index < items[j].Index })
	return items
}

func chooseShop(index, firstID, total, hot, hotPercent int) int {
	if index%100 < hotPercent {
		return firstID + (index*37)%hot
	}
	return firstID + hot + (index*7919)%(total-hot)
}

// executeRequest measures the entire client-visible response lifecycle. A status
// code alone is not a successful sample: the body must be drained and closed so
// keep-alive reuse and response integrity are both represented in the latency.
func executeRequest(client *http.Client, url string, index, shopID int) sample {
	started := time.Now()
	item := sample{Index: index, ShopID: shopID}
	response, err := client.Get(url)
	if err != nil {
		item.TransportError = err.Error()
		item.DurationMicros = time.Since(started).Microseconds()
		return item
	}
	item.HTTPStatus = response.StatusCode
	if _, err := io.Copy(io.Discard, response.Body); err != nil {
		item.ResponseReadError = err.Error()
	}
	if err := response.Body.Close(); err != nil {
		item.ResponseCloseError = err.Error()
	}
	item.DurationMicros = time.Since(started).Microseconds()
	return item
}

func (item sample) completed() bool {
	return item.TransportError == "" && item.ResponseReadError == "" && item.ResponseCloseError == ""
}

func summarizeRun(samples []sample, elapsed time.Duration) summary {
	all := make([]int64, 0, len(samples))
	success := make([]int64, 0, len(samples))
	statuses := make(map[string]int)
	var completed, transportErrors, readErrors, closeErrors int
	for _, item := range samples {
		if item.HTTPStatus != 0 {
			statuses[strconv.Itoa(item.HTTPStatus)]++
		}
		if item.TransportError != "" {
			transportErrors++
		}
		if item.ResponseReadError != "" {
			readErrors++
		}
		if item.ResponseCloseError != "" {
			closeErrors++
		}
		if !item.completed() {
			continue
		}
		completed++
		all = append(all, item.DurationMicros)
		if item.HTTPStatus == http.StatusOK {
			success = append(success, item.DurationMicros)
		}
	}
	seconds := elapsed.Seconds()
	attemptedRPS, completedRPS := 0.0, 0.0
	if seconds > 0 {
		attemptedRPS = float64(len(samples)) / seconds
		completedRPS = float64(completed) / seconds
	}
	return summary{
		ElapsedMillis:          elapsed.Milliseconds(),
		AttemptedThroughputRPS: attemptedRPS,
		CompletedRequests:      completed,
		IncompleteRequests:     len(samples) - completed,
		CompletedThroughputRPS: completedRPS,
		AllCompleted:           summarize(all),
		Successful200:          summarize(success),
		HTTPStatuses:           statuses,
		TransportErrors:        transportErrors,
		ResponseReadErrors:     readErrors,
		ResponseCloseErrors:    closeErrors,
	}
}

func summarize(values []int64) percentileSummary {
	if len(values) == 0 {
		return percentileSummary{}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return percentileSummary{Count: len(values), P50US: nearestRank(values, 50), P95US: nearestRank(values, 95), P99US: nearestRank(values, 99)}
}

func nearestRank(sorted []int64, percentile int) int64 {
	index := (percentile*len(sorted) + 99) / 100
	if index < 1 {
		index = 1
	}
	return sorted[index-1]
}

func writeArtifacts(directory string, samples []sample, encodedSummary []byte) error {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	raw, err := os.Create(filepath.Join(directory, "raw-results.jsonl"))
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(raw)
	encoder := json.NewEncoder(writer)
	for _, item := range samples {
		if err := encoder.Encode(item); err != nil {
			raw.Close()
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		raw.Close()
		return err
	}
	if err := raw.Close(); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(directory, "summary.json"), append(encodedSummary, '\n'), 0o644)
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("%s must be an integer", name)
	}
	return parsed
}
