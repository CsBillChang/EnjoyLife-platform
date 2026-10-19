// async-loadgen measures the durable asynchronous purchase path.  In
// particular, an HTTP 202 only means that a request was admitted; it is never
// used as the order-completion latency.
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/CsBillChang/local-life-platform/internal/platform/config"
	"github.com/CsBillChang/local-life-platform/internal/platform/database"
)

const (
	requestsFileName = "requests.jsonl"
	summaryFileName  = "summary.json"
)

type runConfig struct {
	BaseURL        string
	ActivityID     uint64
	UserStart      uint64
	UserCount      int
	Requests       int
	Concurrency    int
	Timeout        time.Duration
	OutputDir      string
	UsernameFormat string
	Password       string
	MySQLDSN       string
}

// requestResult deliberately excludes Authorization values and passwords.
type requestResult struct {
	Sequence                     int       `json:"sequence"`
	UserNumber                   uint64    `json:"user_number"`
	Username                     string    `json:"username"`
	HTTPStatus                   int       `json:"http_status"`
	RequestID                    string    `json:"request_id,omitempty"`
	AcceptedAt                   time.Time `json:"accepted_at,omitempty"`
	TerminalStatus               string    `json:"terminal_status"`
	FailureCode                  string    `json:"failure_code,omitempty"`
	OrderCreatedAt               time.Time `json:"order_created_at,omitempty"`
	TerminalObservedAt           time.Time `json:"terminal_observed_at,omitempty"`
	RequestUpdatedAt             time.Time `json:"request_updated_at,omitempty"`
	AcceptedToOrderRowCreatedUS  *int64    `json:"accepted_to_order_row_created_us,omitempty"`
	AcceptedToTerminalObservedUS *int64    `json:"accepted_to_terminal_observed_us,omitempty"`
	TerminalPollErrors           int       `json:"terminal_poll_errors,omitempty"`
	LastTerminalPollError        string    `json:"last_terminal_poll_error,omitempty"`
	ClientError                  string    `json:"client_error,omitempty"`
}

type summary struct {
	StartedAt                    time.Time      `json:"started_at"`
	FinishedAt                   time.Time      `json:"finished_at"`
	ActivityID                   uint64         `json:"activity_id"`
	Requests                     int            `json:"requests"`
	Concurrency                  int            `json:"concurrency"`
	Timeout                      string         `json:"timeout"`
	HTTPStatus                   map[string]int `json:"http_status"`
	Terminal                     map[string]int `json:"terminal_status"`
	Failures                     map[string]int `json:"failures"`
	Unfinished                   int            `json:"unfinished"`
	ExactFailureTimesUnavailable int            `json:"exact_failure_times_unavailable"`
	TerminalPollErrors           int            `json:"terminal_poll_errors"`
	AcceptedToOrderRowCreatedUS  percentiles    `json:"accepted_to_order_row_created_us"`
	AcceptedToTerminalObservedUS percentiles    `json:"accepted_to_terminal_observed_us"`
}

type percentiles struct {
	Count int    `json:"count"`
	P50   *int64 `json:"p50,omitempty"`
	P95   *int64 `json:"p95,omitempty"`
	P99   *int64 `json:"p99,omitempty"`
}

func main() {
	cfg, err := loadRunConfig()
	if err != nil {
		fmt.Fprintln(os.Stderr, "async-loadgen:", err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg); err != nil {
		fmt.Fprintln(os.Stderr, "async-loadgen:", err)
		os.Exit(1)
	}
}

func loadRunConfig() (runConfig, error) {
	platform, err := config.Load()
	if err != nil {
		return runConfig{}, fmt.Errorf("load project MySQL configuration: %w", err)
	}
	activity, err := requiredUint("BENCHMARK_ACTIVITY_ID")
	if err != nil {
		return runConfig{}, err
	}
	userStart, err := requiredUint("BENCHMARK_USER_START")
	if err != nil {
		return runConfig{}, err
	}
	userCount, err := positiveEnv("BENCHMARK_USER_COUNT", 1, 100000)
	if err != nil {
		return runConfig{}, err
	}
	requests, err := positiveEnv("BENCHMARK_REQUESTS", 1, 100000)
	if err != nil {
		return runConfig{}, err
	}
	concurrency, err := positiveEnv("BENCHMARK_CONCURRENCY", 1, 10000)
	if err != nil {
		return runConfig{}, err
	}
	if concurrency > requests {
		concurrency = requests
	}
	timeout, err := durationEnv("BENCHMARK_TIMEOUT", 30*time.Second)
	if err != nil {
		return runConfig{}, err
	}
	password := os.Getenv("BENCHMARK_PASSWORD")
	if password == "" {
		return runConfig{}, errors.New("BENCHMARK_PASSWORD is required (it is never written to output)")
	}
	outputDir := os.Getenv("BENCHMARK_OUTPUT_DIR")
	if outputDir == "" {
		return runConfig{}, errors.New("BENCHMARK_OUTPUT_DIR is required")
	}
	usernameFormat := envOr("BENCHMARK_USERNAME_FORMAT", "benchmark_customer_%d")
	if !strings.Contains(usernameFormat, "%") {
		return runConfig{}, errors.New("BENCHMARK_USERNAME_FORMAT must contain an integer format verb such as %d")
	}
	return runConfig{
		BaseURL: strings.TrimRight(envOr("BENCHMARK_BASE_URL", "http://127.0.0.1:18080"), "/"), ActivityID: activity,
		UserStart: userStart, UserCount: userCount, Requests: requests, Concurrency: concurrency, Timeout: timeout,
		OutputDir: outputDir, UsernameFormat: usernameFormat, Password: password, MySQLDSN: platform.MySQLDSN,
	}, nil
}

func run(parent context.Context, cfg runConfig) error {
	if err := os.MkdirAll(cfg.OutputDir, 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	db, err := database.OpenMySQL(parent, cfg.MySQLDSN)
	if err != nil {
		return fmt.Errorf("connect MySQL for durable timing: %w", err)
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(parent, cfg.Timeout)
	defer cancel()
	client := &http.Client{Timeout: cfg.Timeout}
	tokens, err := loginUsers(ctx, client, cfg)
	if err != nil {
		return err
	}
	started := time.Now().UTC()
	results := submitAll(ctx, client, cfg, tokens)
	awaitTerminals(ctx, db, results)
	finished := time.Now().UTC()
	requestsPath, summaryPath := outputPaths(cfg.OutputDir)
	if err := writeJSONL(requestsPath, results); err != nil {
		return err
	}
	data := summarize(started, finished, cfg, results)
	if err := writeJSON(summaryPath, data); err != nil {
		return err
	}
	fmt.Printf("wrote %s and %s; terminal=%v unfinished=%d\n", requestsPath, summaryPath, data.Terminal, data.Unfinished)
	return nil
}

func loginUsers(ctx context.Context, client *http.Client, cfg runConfig) (map[uint64]string, error) {
	tokens := make(map[uint64]string, cfg.UserCount)
	for i := 0; i < cfg.UserCount; i++ {
		userID := cfg.UserStart + uint64(i)
		username := formatUsername(cfg.UsernameFormat, userID)
		if strings.Contains(username, "%!") {
			return nil, fmt.Errorf("BENCHMARK_USERNAME_FORMAT %q is not compatible with an integer user range", cfg.UsernameFormat)
		}
		body, _ := json.Marshal(map[string]string{"username": username, "password": cfg.Password})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/auth/login", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("create login request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("login user %d: %w", userID, err)
		}
		var payload struct {
			Data struct {
				AccessToken string `json:"access_token"`
			} `json:"data"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK || payload.Data.AccessToken == "" {
			return nil, fmt.Errorf("login user %d failed with HTTP %d", userID, resp.StatusCode)
		}
		tokens[userID] = payload.Data.AccessToken
	}
	return tokens, nil
}

func submitAll(ctx context.Context, client *http.Client, cfg runConfig, tokens map[uint64]string) []*requestResult {
	results := make([]*requestResult, cfg.Requests)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for worker := 0; worker < cfg.Concurrency; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				userID := cfg.UserStart + uint64(i%cfg.UserCount)
				results[i] = submitOne(ctx, client, cfg, i+1, userID, tokens[userID])
			}
		}()
	}
	for i := range results {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return results
}

func submitOne(ctx context.Context, client *http.Client, cfg runConfig, sequence int, userID uint64, token string) *requestResult {
	result := &requestResult{Sequence: sequence, UserNumber: userID, Username: formatUsername(cfg.UsernameFormat, userID), TerminalStatus: "HTTP_REJECTED"}
	body, _ := json.Marshal(map[string]uint64{"activity_id": cfg.ActivityID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.BaseURL+"/purchase-requests", bytes.NewReader(body))
	if err != nil {
		result.ClientError = err.Error()
		return result
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Idempotency-Key", fmt.Sprintf("async-loadgen-%d-%d", time.Now().UnixNano(), sequence))
	resp, err := client.Do(req)
	if err != nil {
		result.ClientError = err.Error()
		result.TerminalStatus = "CLIENT_ERROR"
		return result
	}
	defer resp.Body.Close()
	result.HTTPStatus = resp.StatusCode
	var payload struct {
		Data struct {
			RequestID string `json:"request_id"`
		} `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		result.ClientError = "decode response: " + err.Error()
		return result
	}
	if resp.StatusCode == http.StatusAccepted && payload.Data.RequestID != "" {
		result.RequestID = payload.Data.RequestID
		result.TerminalStatus = "PENDING"
		return result
	}
	result.FailureCode = payload.Error.Code
	return result
}

func awaitTerminals(ctx context.Context, db *sql.DB, results []*requestResult) {
	for {
		pending := 0
		for _, result := range results {
			if result.RequestID == "" || result.TerminalStatus != "PENDING" {
				continue
			}
			pending++
			row, terminal, found, err := queryTerminal(ctx, db, result.RequestID)
			if err != nil {
				if ctx.Err() == nil {
					result.TerminalPollErrors++
					result.LastTerminalPollError = err.Error()
				}
				continue
			}
			if found {
				result.AcceptedAt = row.acceptedAt
			}
			if terminal {
				observedAt := time.Now().UTC()
				result.TerminalStatus, result.FailureCode = row.status, row.failure
				result.TerminalObservedAt = observedAt
				observedLatency := observedAt.Sub(result.AcceptedAt).Microseconds()
				result.AcceptedToTerminalObservedUS = &observedLatency
				if row.status == "SUCCEEDED" {
					result.OrderCreatedAt = row.at
					latency := result.OrderCreatedAt.Sub(result.AcceptedAt).Microseconds()
					result.AcceptedToOrderRowCreatedUS = &latency
				} else {
					// This is audit evidence only. A later reservation repair can
					// update the row; there is no immutable FAILED-at column.
					result.RequestUpdatedAt = row.at
				}
			}
		}
		if pending == 0 || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(25 * time.Millisecond):
		}
	}
	for _, result := range results {
		if result.TerminalStatus == "PENDING" {
			result.TerminalStatus = "UNFINISHED"
		}
	}
}

type terminalRow struct {
	acceptedAt, at  time.Time
	status, failure string
}

func queryTerminal(ctx context.Context, db *sql.DB, requestID string) (terminalRow, bool, bool, error) {
	var row terminalRow
	var orderAt sql.NullTime
	var failure sql.NullString
	var updatedAt time.Time
	err := db.QueryRowContext(ctx, `SELECT p.accepted_at, p.status, p.error_code, p.updated_at, o.created_at FROM purchase_requests p LEFT JOIN orders o ON o.request_id = p.request_id WHERE p.request_id = ?`, requestID).Scan(&row.acceptedAt, &row.status, &failure, &updatedAt, &orderAt)
	if errors.Is(err, sql.ErrNoRows) {
		return terminalRow{}, false, false, nil
	}
	if err != nil {
		return terminalRow{}, false, false, fmt.Errorf("query terminal request %s: %w", requestID, err)
	}
	if failure.Valid {
		row.failure = failure.String
	}
	switch row.status {
	case "SUCCEEDED":
		if orderAt.Valid {
			row.at = orderAt.Time
			return row, true, true, nil
		}
	case "FAILED":
		row.at = updatedAt
		return row, true, true, nil
	}
	return row, false, true, nil
}

func summarize(started, finished time.Time, cfg runConfig, results []*requestResult) summary {
	s := summary{StartedAt: started, FinishedAt: finished, ActivityID: cfg.ActivityID, Requests: cfg.Requests, Concurrency: cfg.Concurrency, Timeout: cfg.Timeout.String(), HTTPStatus: map[string]int{}, Terminal: map[string]int{}, Failures: map[string]int{}}
	rowLatencies := make([]int64, 0, len(results))
	observedLatencies := make([]int64, 0, len(results))
	for _, result := range results {
		s.HTTPStatus[strconv.Itoa(result.HTTPStatus)]++
		s.Terminal[result.TerminalStatus]++
		if result.TerminalStatus == "UNFINISHED" {
			s.Unfinished++
		}
		if result.FailureCode != "" {
			s.Failures[result.FailureCode]++
		}
		if result.TerminalStatus == "FAILED" {
			s.ExactFailureTimesUnavailable++
		}
		s.TerminalPollErrors += result.TerminalPollErrors
		if result.AcceptedToOrderRowCreatedUS != nil {
			rowLatencies = append(rowLatencies, *result.AcceptedToOrderRowCreatedUS)
		}
		if result.AcceptedToTerminalObservedUS != nil {
			observedLatencies = append(observedLatencies, *result.AcceptedToTerminalObservedUS)
		}
	}
	s.AcceptedToOrderRowCreatedUS = calculatePercentiles(rowLatencies)
	s.AcceptedToTerminalObservedUS = calculatePercentiles(observedLatencies)
	return s
}

func calculatePercentiles(values []int64) percentiles {
	p := percentiles{Count: len(values)}
	if len(values) == 0 {
		return p
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50, p95, p99 := nearestRank(sorted, 50), nearestRank(sorted, 95), nearestRank(sorted, 99)
	p.P50, p.P95, p.P99 = &p50, &p95, &p99
	return p
}

func nearestRank(sorted []int64, percentile int) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := (len(sorted)*percentile+99)/100 - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

func writeJSONL(path string, results []*requestResult) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
	}
	return f.Sync()
}

func writeJSON(path string, value any) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return f.Sync()
}

func outputPaths(outputDir string) (requestsPath, summaryPath string) {
	return filepath.Join(outputDir, requestsFileName), filepath.Join(outputDir, summaryFileName)
}

func formatUsername(format string, number uint64) string { return fmt.Sprintf(format, number) }

func requiredUint(name string) (uint64, error) {
	raw := os.Getenv(name)
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || value == 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}
func positiveEnv(name string, min, max int) (int, error) {
	raw := envOr(name, strconv.Itoa(min))
	value, err := strconv.Atoi(raw)
	if err != nil || value < min || value > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", name, min, max)
	}
	return value, nil
}
func durationEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := envOr(name, fallback.String())
	value, err := time.ParseDuration(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration such as 30s", name)
	}
	return value, nil
}
func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
