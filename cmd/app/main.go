package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	v1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/prometheus/client_golang/api/prometheus"
)

const (
	defaultPrometheusURL     = "http://localhost:8428" // Default VictoriaMetrics URL
	defaultJobName           = "blackbox"
	defaultTargetValue       = "http://example.com"
	defaultAggregationPeriod = 1 * time.Minute
	defaultQueryStep         = 2 * time.Hour
	// Default range for queries (e.g., for sum_over_time and increase)
	// This should cover the full month effectively.
	// We'll calculate it dynamically based on the start and end time of the month.
	defaultQueryRange = "24h" 
)

// MetricResult represents a single calculated metric value for a specific time
type MetricResult struct {
	Timestamp time.Time
	Value     float64
}

// getEnv retrieves an environment variable or returns a default value
func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

// parseDuration attempts to parse a duration string (e.g., "1m", "2h")
// If parsing fails, it prints a warning and returns the default.
func parseDuration(s string, defaultVal time.Duration, varName string) time.Duration {
	if s == "" {
		return defaultVal
	}
	d, err := model.ParseDuration(s)
	if err != nil {
		fmt.Printf("Warning: Invalid %s '%s', using default %s. Error: %v\n", varName, s, defaultVal, err)
		return defaultVal
	}
	return time.Duration(d)
}

func main() {
	// Configure from environment variables
	prometheusURL := getEnv("VM_PROM_URL", defaultPrometheusURL)
	jobName := getEnv("JOB_NAME", defaultJobName)
	targetValue := getEnv("TARGET_VALUE", defaultTargetValue)

	aggregationPeriodStr := getEnv("AGGREGATION_PERIOD", "1m") // e.g., "1m", "5m"
	aggregationPeriod := parseDuration(aggregationPeriodStr, defaultAggregationPeriod, "AGGREGATION_PERIOD")

	queryStepStr := getEnv("QUERY_STEP", "2h") // e.g., "1h", "2h"
	queryStep := parseDuration(queryStepStr, defaultQueryStep, "QUERY_STEP")

	queryRangeStr := getEnv("QUERY_RANGE", defaultQueryRange) // e.g., "24h", "7d"
	// We parse this for validation and later use its string representation in queries.
	queryRangeDuration := parseDuration(queryRangeStr, 24*time.Hour, "QUERY_RANGE") // Not directly used as time.Duration in range queries here

	fmt.Printf("Connecting to VictoriaMetrics at: %s\n", prometheusURL)
	fmt.Printf("Monitoring target: job=\"%s\", target=\"%s\"\n", jobName, targetValue)
	fmt.Printf("Aggregation period: %s\n", aggregationPeriod)
	fmt.Printf("Query step (interval for each request): %s\n", queryStep)
	fmt.Printf("Query range for calculations (e.g., for sum_over_time): %s\n", queryRangeStr)

	client, err := prometheus.NewClient(prometheus.Config{
		Address: prometheusURL,
	})
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return
	}

	api := v1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute) // Increased timeout
	defer cancel()

	// Calculate start and end times for the last month
	endTime := time.Now()
	startTime := endTime.AddDate(0, -1, 0) // One month ago from now

	fmt.Printf("\nCalculating metrics for the period: %s to %s\n", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	// Maps to store aggregated results for each metric type
	availabilityResults := make(map[time.Time]float64)
	downtimeSecondsResults := make(map[time.Time]float64)
	mttrResults := make(map[time.Time]float64)
	incidentsResults := make(map[time.Time]float64)

	currentQueryTime := startTime
	for currentQueryTime.Before(endTime) {
		rangeEnd := currentQueryTime.Add(queryStep)
		if rangeEnd.After(endTime) {
			rangeEnd = endTime
		}

		// Calculate __interval and __range for the current sub-query window
		// __interval will be aggregationPeriod
		// __range will be the duration of the current query step or queryRangeDuration if it's smaller
		effectiveInterval := aggregationPeriod.String()
		effectiveRange := (rangeEnd.Sub(currentQueryTime)).String()
		if queryRangeDuration > 0 && queryRangeDuration < (rangeEnd.Sub(currentQueryTime)) {
			effectiveRange = queryRangeDuration.String()
		}

		fmt.Printf("\n--- Querying slice: %s to %s ---\n", currentQueryTime.Format(time.RFC3339), rangeEnd.Format(time.RFC3339))

		// 1. Доступность (Availability)
		// avg_over_time(probe_success{job="$job", target="$target"}[$__interval]) * 100
		availabilityQuery := fmt.Sprintf(`avg_over_time(probe_success{job="%s", target="%s"}[%s]) * 100`, jobName, targetValue, effectiveInterval)
		fmt.Printf("Running Availability Query: %s\n", availabilityQuery)
		queryAndStore(api, ctx, availabilityQuery, currentQueryTime, rangeEnd, aggregationPeriod, availabilityResults)

		// 2. Простой системы (Downtime) - Total seconds of downtime
		// sum_over_time((1 - probe_success{job="$job", target="$target"})[$__range:])
		downtimeQuery := fmt.Sprintf(`sum_over_time((1 - probe_success{job="%s", target="%s"})[%s:])`, jobName, targetValue, effectiveRange)
		fmt.Printf("Running Downtime Query: %s\n", downtimeQuery)
		queryAndStore(api, ctx, downtimeQuery, currentQueryTime, rangeEnd, aggregationPeriod, downtimeSecondsResults)

		// 3. Среднее время восстановления (Mean Time To Recovery - MTTR)
		// sum_over_time((1 - probe_success{job="$job", target="$target"}) [$__range:]) / (sum(increase((probe_success{job="$job", target="$target"} == bool 0)[$__range:])) + 0.00000001)
		// Prometheus `increase` counts events, so `increase(probe_success == bool 0)` counts transitions from good to bad.
		// We'll calculate the numerator (total downtime) and denominator (number of incidents) separately to be robust,
		// and then combine them in the final output. Or, we can directly use the PromQL expression.
		// Using the direct PromQL for simplicity as per the request.
		mttrQuery := fmt.Sprintf(`sum_over_time((1 - probe_success{job="%s", target="%s"})[%s:]) / (sum(increase((probe_success{job="%s", target="%s"} == bool 0)[%s:])) + 0.00000001)`,
			jobName, targetValue, effectiveRange, jobName, targetValue, effectiveRange)
		fmt.Printf("Running MTTR Query: %s\n", mttrQuery)
		queryAndStore(api, ctx, mttrQuery, currentQueryTime, rangeEnd, aggregationPeriod, mttrResults)

		// 4. Простоев (Number of Incidents)
		// sum(increase((probe_success{job="$job", target="$target"} == bool 0)[$__range:]))
		// This counts how many times probe_success changed from 1 to 0 (i.e., went down).
		incidentsQuery := fmt.Sprintf(`sum(increase((probe_success{job="%s", target="%s"} == bool 0)[%s:]))`, jobName, targetValue, effectiveRange)
		fmt.Printf("Running Incidents Query: %s\n", incidentsQuery)
		queryAndStore(api, ctx, incidentsQuery, currentQueryTime, rangeEnd, aggregationPeriod, incidentsResults)

		currentQueryTime = rangeEnd
	}

	fmt.Println("\n--- Final Aggregated Results (Sorted by Time) ---")

	printSortedResults("Доступность (%)", availabilityResults)
	printSortedResults("Простой системы (секунды)", downtimeSecondsResults)
	printSortedResults("Среднее время восстановления (секунды)", mttrResults)
	printSortedResults("Количество простоев", incidentsResults)

	fmt.Println("\n--- End of Results ---")
}

// queryAndStore performs a Prometheus range query and stores the results in the provided map
func queryAndStore(api v1.API, ctx context.Context, query string, start, end time.Time, step time.Duration, results map[time.Time]float64) {
	r := v1.Range{
		Start: start,
		End:   end,
		Step:  step,
	}

	value, warnings, err := api.QueryRange(ctx, query, r)
	if err != nil {
		fmt.Printf("Error querying Prometheus for '%s': %v\n", query, err)
		return
	}
	if len(warnings) > 0 {
		fmt.Printf("Prometheus warnings for '%s': %v\n", query, warnings)
	}

	if matrix, ok := value.(model.Matrix); ok {
		for _, series := range matrix {
			for _, sample := range series.Values {
				roundedTime := sample.Timestamp.Time().Round(step)
				results[roundedTime] = float64(sample.Value)
			}
		}
	} else {
		fmt.Printf("Unexpected query result format for '%s'. Expected a Matrix.\n", query)
	}
}

// printSortedResults prints the results map, sorted by timestamp
func printSortedResults(metricName string, results map[time.Time]float64) {
	if len(results) == 0 {
		fmt.Printf("\n--- %s ---\n", metricName)
		fmt.Println("No data found.")
		return
	}

	sortedTimestamps := make([]time.Time, 0, len(results))
	for ts := range results {
		sortedTimestamps = append(sortedTimestamps, ts)
	}
	// Sort the slice of timestamps
	for i := 0; i < len(sortedTimestamps); i++ {
		for j := i + 1; j < len(sortedTimestamps); j++ {
			if sortedTimestamps[j].Before(sortedTimestamps[i]) {
				sortedTimestamps[i], sortedTimestamps[j] = sortedTimistedTimestamps[j], sortedTimestamps[i]
			}
		}
	}

	fmt.Printf("\n--- %s ---\n", metricName)
	for _, ts := range sortedTimestamps {
		fmt.Printf("Time: %s, Value: %.4f\n", ts.Format("2006-01-02 15:04:05"), results[ts])
	}
}