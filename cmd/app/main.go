package main

import (
	"context"
	"fmt"
	"os"
	"time"

<<<<<<< HEAD
	"github.com/prometheus/client_golang/api/prometheus"       // <--- Add this back for NewClient
	v1 "github.com/prometheus/client_golang/api/prometheus/v1" // This is for the API v1 client
=======
	prometheusClient "github.com/prometheus/client_golang/api"        // Correct import for NewClient and Config
	prometheusV1 "github.com/prometheus/client_golang/api/prometheus/v1" // Correct import for v1 API
>>>>>>> b63beca2a6a36e103c2e2278a7b21363f487131b
	"github.com/prometheus/common/model"
)

const (
	defaultPrometheusURL     = "http://localhost:8428" // Default VictoriaMetrics URL
	defaultJobName           = "blackbox"
	defaultTargetValue       = "http://example.com"
	defaultAggregationPeriod = 1 * time.Minute
	defaultQueryStep         = 2 * time.Hour
<<<<<<< HEAD
	// Default range for queries (e.g., for sum_over_time and increase)
	// This should cover the full month effectively.
	// We'll calculate it dynamically based on the start and end time of the month.
	defaultQueryRange = "24h"
=======
	defaultQueryRange        = "24h"
>>>>>>> b63beca2a6a36e103c2e2278a7b21363f487131b
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

	aggregationPeriodStr := getEnv("AGGREGATION_PERIOD", "1m")
	aggregationPeriod := parseDuration(aggregationPeriodStr, defaultAggregationPeriod, "AGGREGATION_PERIOD")

	queryStepStr := getEnv("QUERY_STEP", "2h")
	queryStep := parseDuration(queryStepStr, defaultQueryStep, "QUERY_STEP")

	queryRangeStr := getEnv("QUERY_RANGE", defaultQueryRange)
	queryRangeDuration := parseDuration(queryRangeStr, 24*time.Hour, "QUERY_RANGE")

	fmt.Printf("Connecting to VictoriaMetrics at: %s\n", prometheusURL)
	fmt.Printf("Monitoring target: job=\"%s\", target=\"%s\"\n", jobName, targetValue)
	fmt.Printf("Aggregation period: %s\n", aggregationPeriod)
	fmt.Printf("Query step (interval for each request): %s\n", queryStep)
	fmt.Printf("Query range for calculations (e.g., for sum_over_time): %s\n", queryRangeStr)

	// Correctly using aliased import for NewClient and Config
	client, err := prometheusClient.NewClient(prometheusClient.Config{
		Address: prometheusURL,
	})
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return
	}

	// Using the aliased v1 API client
	api := prometheusV1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	endTime := time.Now()
	startTime := endTime.AddDate(0, -1, 0)

	fmt.Printf("\nCalculating metrics for the period: %s to %s\n", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

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

		effectiveInterval := aggregationPeriod.String()
		effectiveRange := (rangeEnd.Sub(currentQueryTime)).String()
		if queryRangeDuration > 0 && queryRangeDuration < (rangeEnd.Sub(currentQueryTime)) {
			effectiveRange = queryRangeDuration.String()
		}

		fmt.Printf("\n--- Querying slice: %s to %s ---\n", currentQueryTime.Format(time.RFC3339), rangeEnd.Format(time.RFC3339))

		availabilityQuery := fmt.Sprintf(`avg_over_time(probe_success{job="%s", target="%s"}[%s]) * 100`, jobName, targetValue, effectiveInterval)
		fmt.Printf("Running Availability Query: %s\n", availabilityQuery)
		queryAndStore(api, ctx, availabilityQuery, currentQueryTime, rangeEnd, aggregationPeriod, availabilityResults)

		downtimeQuery := fmt.Sprintf(`sum_over_time((1 - probe_success{job="%s", target="%s"})[%s:])`, jobName, targetValue, effectiveRange)
		fmt.Printf("Running Downtime Query: %s\n", downtimeQuery)
		queryAndStore(api, ctx, downtimeQuery, currentQueryTime, rangeEnd, aggregationPeriod, downtimeSecondsResults)

		mttrQuery := fmt.Sprintf(`sum_over_time((1 - probe_success{job="%s", target="%s"})[%s:]) / (sum(increase((probe_success{job="%s", target="%s"} == bool 0)[%s:])) + 0.00000001)`,
			jobName, targetValue, effectiveRange, jobName, targetValue, effectiveRange)
		fmt.Printf("Running MTTR Query: %s\n", mttrQuery)
		queryAndStore(api, ctx, mttrQuery, currentQueryTime, rangeEnd, aggregationPeriod, mttrResults)

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
func queryAndStore(api prometheusV1.API, ctx context.Context, query string, start, end time.Time, step time.Duration, results map[time.Time]float64) {
	r := prometheusV1.Range{
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
				sortedTimestamps[i], sortedTimestamps[j] = sortedTimestamps[j], sortedTimestamps[i]
			}
		}
	}

	fmt.Printf("\n--- %s ---\n", metricName)
	for _, ts := range sortedTimestamps {
		fmt.Printf("Time: %s, Value: %.4f\n", ts.Format("2006-01-02 15:04:05"), results[ts])
	}
}
