package main

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	prometheusClient "github.com/prometheus/client_golang/api"
	prometheusV1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const (
	defaultPrometheusURL     = "http://localhost:8428"
	defaultPrometheusURLIn   = "" // Default empty, meaning no output
	defaultJobName           = "blackbox"
	defaultMetricPrefix      = "calc_be_"
	defaultScrapeInterval    = 1 * time.Minute // Assumed blackbox_exporter scrape interval
	defaultQueryStep         = 12 * time.Hour  // Chunk size for fetching data from Prometheus/VictoriaMetrics
)

// SamplePair represents a single time series data point (timestamp and value)
type SamplePair struct {
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
	prometheusURLIn := getEnv("VM_PROM_URL_IN", defaultPrometheusURLIn)
	jobName := getEnv("JOB_NAME", defaultJobName)
	targetValueFilter := getEnv("TARGET_VALUE", "") // Filter for a specific target, empty for all
	metricPrefix := getEnv("METRIC_PREFIX", defaultMetricPrefix)

	scrapeIntervalStr := getEnv("SCRAPE_INTERVAL", "1m")
	scrapeInterval := parseDuration(scrapeIntervalStr, defaultScrapeInterval, "SCRAPE_INTERVAL")

	queryStepStr := getEnv("QUERY_STEP", "12h")
	queryStep := parseDuration(queryStepStr, defaultQueryStep, "QUERY_STEP")

	// --- Определение периода расчета (аналогично main2.go) ---
	targetMonthStr := getEnv("TARGET_MONTH", "")
	targetYearStr := getEnv("TARGET_YEAR", "")

	var startTime, endTime time.Time
	if targetMonthStr != "" && targetYearStr != "" {
		monthInt, errMonth := strconv.Atoi(targetMonthStr)
		yearInt, errYear := strconv.Atoi(targetYearStr)

		if errMonth != nil || errYear != nil || monthInt < 1 || monthInt > 12 {
			fmt.Printf("Error: Invalid TARGET_MONTH ('%s') or TARGET_YEAR ('%s'). Using last month's data.\n", targetMonthStr, targetYearStr)
			endTime = time.Now()
			startTime = endTime.AddDate(0, -1, 0)
		} else {
			startTime = time.Date(yearInt, time.Month(monthInt), 1, 0, 0, 0, 0, time.Local)
			endTime = startTime.AddDate(0, 1, 0).Add(-time.Nanosecond)
			fmt.Printf("Calculating metrics for specified month: %s %d\n", time.Month(monthInt), yearInt)
		}
	} else {
		fmt.Println("TARGET_MONTH or TARGET_YEAR not set. Using last month's data.")
		endTime = time.Now()
		startTime = endTime.AddDate(0, -1, 0)
	}

	fmt.Printf("Connecting to VictoriaMetrics at: %s\n", prometheusURL)
	fmt.Printf("Monitoring job: \"%s\"\n", jobName)
	if targetValueFilter != "" {
		fmt.Printf("Filtering for target: \"%s\"\n", targetValueFilter)
	} else {
		fmt.Println("Calculating metrics for ALL targets under this job.")
	}
	fmt.Printf("Assumed scrape interval (query step for raw data): %s\n", scrapeInterval)
	fmt.Printf("Query chunk size: %s\n", queryStep)
	if prometheusURLIn != "" {
		fmt.Printf("Sending calculated metrics to: %s with prefix '%s'\n", prometheusURLIn, metricPrefix)
	} else {
		fmt.Println("No VM_PROM_URL_IN defined, metrics will not be pushed.")
	}

	client, err := prometheusClient.NewClient(prometheusClient.Config{
		Address: prometheusURL,
	})
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return
	}

	api := prometheusV1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	// --- Получение списка таргетов ---
	targets, err := getTargetsForJob(ctx, api, jobName, targetValueFilter)
	if err != nil {
		fmt.Printf("Error getting targets for job '%s': %v\n", jobName, err)
		return
	}
	if len(targets) == 0 {
		fmt.Printf("No targets found for job '%s' (or filtered target '%s'). Exiting.\n", jobName, targetValueFilter)
		return
	}

	fmt.Printf("\nFound %d target(s) for job '%s' (after filter): %v\n", len(targets), jobName, targets)

	// --- Перебор таргетов и расчет метрик для каждого ---
	for _, target := range targets {
		fmt.Printf("\n--- Processing metrics for target: \"%s\" ---\n", target)

		var rawProbeSuccessData []SamplePair
		currentQueryTime := startTime

		for currentQueryTime.Before(endTime) {
			rangeEnd := currentQueryTime.Add(queryStep)
			if rangeEnd.After(endTime) {
				rangeEnd = endTime
			}

			query := fmt.Sprintf(`probe_success{job="%s", target="%s"}`, jobName, target)
			// fmt.Printf("Fetching raw data chunk: %s to %s with step %s for query: %s\n",
			// 	currentQueryTime.Format(time.RFC3339), rangeEnd.Format(time.RFC3339), scrapeInterval, query)

			r := prometheusV1.Range{
				Start: currentQueryTime,
				End:   rangeEnd,
				Step:  scrapeInterval,
			}

			value, warnings, err := api.QueryRange(ctx, query, r)
			if err != nil {
				fmt.Printf("Error querying Prometheus for raw data for target '%s': %v\n", target, err)
				currentQueryTime = rangeEnd // Move to next chunk to try and continue
				continue
			}
			if len(warnings) > 0 {
				fmt.Printf("Prometheus warnings for raw data query for target '%s': %v\n", target, warnings)
			}

			if matrix, ok := value.(model.Matrix); ok {
				for _, series := range matrix {
					for _, sample := range series.Values {
						rawProbeSuccessData = append(rawProbeSuccessData, SamplePair{
							Timestamp: sample.Timestamp.Time(),
							Value:     float64(sample.Value),
						})
					}
				}
			} else {
				fmt.Printf("Unexpected raw data query result format for target '%s'. Expected a Matrix.\n", target)
				currentQueryTime = rangeEnd // Move to next chunk
				continue
			}
			currentQueryTime = rangeEnd
		}

		if len(rawProbeSuccessData) == 0 {
			fmt.Printf("No raw probe_success data collected for target '%s'. Cannot calculate metrics.\n", target)
			continue // Move to next target
		}

		sort.Slice(rawProbeSuccessData, func(i, j int) bool {
			return rawProbeSuccessData[i].Timestamp.Before(rawProbeSuccessData[j].Timestamp)
		})

		fmt.Printf("Collected %d raw data points for target '%s'.\n", len(rawProbeSuccessData), target)

		// --- Расчет и вывод агрегированных метрик ---
		fmt.Println("\n--- Calculated Aggregated Metrics ---")

		availability := calculateAvailability(rawProbeSuccessData)
		fmt.Printf("  1. Доступность (Availability): %.4f%%\n", availability)

		downtimeSeconds := calculateDowntime(rawProbeSuccessData, scrapeInterval)
		fmt.Printf("  2. Простой системы (Total Downtime): %.2f seconds (approx. %s)\n", downtimeSeconds, formatDuration(time.Duration(downtimeSeconds)*time.Second))

		numIncidents := calculateNumIncidents(rawProbeSuccessData)
		fmt.Printf("  3. Количество простоев (Number of Incidents): %d\n", numIncidents)

		mttrSeconds := calculateMTTR(rawProbeSuccessData, scrapeInterval)
		if mttrSeconds < 0 {
			fmt.Printf("  4. Среднее время восстановления (MTTR): Not applicable (no incidents or recovery)\n")
		} else {
			fmt.Printf("  4. Среднее время восстановления (MTTR): %.2f seconds (approx. %s)\n", mttrSeconds, formatDuration(time.Duration(mttrSeconds)*time.Second))
		}

		// --- Отправка метрик в VM_PROM_URL_IN ---
		if prometheusURLIn != "" {
			fmt.Printf("  Pushing metrics to %s...\n", prometheusURLIn)
			labels := map[string]string{
				"job":    jobName,
				"target": target,
			}
			sendMetrics(prometheusURLIn, metricPrefix, labels, map[string]float64{
				"availability_percent":          availability,
				"downtime_seconds_total":        downtimeSeconds,
				"number_of_incidents_total":     float64(numIncidents),
				"mean_time_to_recovery_seconds": mttrSeconds,
			})
		}
	}

	fmt.Println("\n--- All targets processed. End of report. ---")
}

// getTargetsForJob fetches all unique 'target' labels for a given 'job' from Prometheus/VictoriaMetrics.
// If targetFilter is not empty, it returns only that target if found.
func getTargetsForJob(ctx context.Context, api prometheusV1.API, job string, targetFilter string) ([]string, error) {
	// Query for unique target labels under the given job
	// Using a regex match to find any probe_success metrics with the job label
	query := fmt.Sprintf(`label_values(probe_success{job="%s"}, target)`, job)
	result, warnings, err := api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("error querying labels: %w", err)
	}
	if len(warnings) > 0 {
		fmt.Printf("Warnings from label query: %v\n", warnings)
	}

	targets := []string{}
	if vector, ok := result.(model.Vector); ok {
		for _, sample := range vector {
			targetLabel := string(sample.Metric["__name__"]) // In label_values result, the value is in __name__
			if targetLabel != "" {
				targets = append(targets, targetLabel)
			}
		}
	} else if matrix, ok := result.(model.Matrix); ok {
		// This can happen if label_values returns a matrix with one-element vectors.
		// Iterate through the matrix, then through the vector within.
		for _, series := range matrix {
			if len(series.Values) > 0 {
				targetLabel := string(series.Metric["__name__"])
				if targetLabel != "" {
					targets = append(targets, targetLabel)
				}
			}
		}
	} else if scalar, ok := result.(*model.Scalar); ok {
        // Handle cases where label_values might return a scalar if only one value exists.
        // This is less common for label_values but good to be robust.
        targets = append(targets, scalar.String())
    } else {
		return nil, fmt.Errorf("unexpected result type for label_values query: %T", result)
	}

	// Filter targets if a specific target is requested
	if targetFilter != "" {
		filteredTargets := []string{}
		found := false
		for _, t := range targets {
			if t == targetFilter {
				filteredTargets = append(filteredTargets, t)
				found = true
				break
			}
		}
		if !found && len(targets) > 0 {
			return nil, fmt.Errorf("target '%s' not found for job '%s' among available targets: %v", targetFilter, job, targets)
		}
		return filteredTargets, nil
	}

	// If no filter, return all unique targets
	sort.Strings(targets) // Sort for consistent output
	uniqueTargets := make([]string, 0, len(targets))
	seen := make(map[string]bool)
	for _, t := range targets {
		if _, ok := seen[t]; !ok {
			seen[t] = true
			uniqueTargets = append(uniqueTargets, t)
		}
	}
	return uniqueTargets, nil
}


// sendMetrics pushes calculated metrics to a Prometheus Pushgateway-like endpoint (e.g., VictoriaMetrics /write).
// This uses the InfluxDB line protocol, which VictoriaMetrics supports via /write endpoint.
func sendMetrics(url, prefix string, commonLabels map[string]string, metrics map[string]float64) {
	var sb strings.Builder
	timestamp := time.Now().UnixNano() // Use current time for the push

	// Construct common labels string
	var commonLabelsBuilder strings.Builder
	for k, v := range commonLabels {
		if commonLabelsBuilder.Len() > 0 {
			commonLabelsBuilder.WriteString(",")
		}
		commonLabelsBuilder.WriteString(fmt.Sprintf("%s=%s", k, escapeInfluxQL(v)))
	}
	commonLabelsStr := commonLabelsBuilder.String()

	for name, value := range metrics {
		metricName := prefix + name
		// InfluxDB line protocol format: <measurement>,<tag_key>=<tag_value> <field_key>=<field_value> <timestamp>
		// For VictoriaMetrics /write, it prefers Prometheus remote_write format or InfluxDB line protocol.
		// Using InfluxDB line protocol for simplicity here.
		sb.WriteString(fmt.Sprintf("%s", escapeInfluxQL(metricName))) // Measurement
		if commonLabelsStr != "" {
			sb.WriteString(fmt.Sprintf(",%s", commonLabelsStr)) // Tags
		}
		sb.WriteString(fmt.Sprintf(" value=%.4f %d\n", value, timestamp)) // Field and timestamp
	}

	payload := sb.String()
	req, err := http.NewRequest("POST", url, strings.NewReader(payload))
	if err != nil {
		fmt.Printf("Error creating HTTP request to send metrics: %v\n", err)
		return
	}
	req.Header.Set("Content-Type", "text/plain") // Content-Type for InfluxDB line protocol

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Error sending metrics to %s: %v\n", url, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := ioutil.ReadAll(resp.Body)
		fmt.Printf("Failed to send metrics to %s. Status: %s, Body: %s\n", url, resp.Status, string(bodyBytes))
	} else {
		fmt.Println("  Metrics successfully pushed.")
	}
}

// escapeInfluxQL escapes special characters for InfluxDB Line Protocol.
func escapeInfluxQL(s string) string {
    // For tag values: comma, space, and double quotes must be escaped.
    // For measurement/field keys: comma, space, equals sign must be escaped.
    // We are using it for measurement (metricName) and tag values.
    // Replace commas and spaces with backslash-escaped versions.
    s = strings.ReplaceAll(s, ",", "\\,")
    s = strings.ReplaceAll(s, " ", "\\ ")
    // Double quotes are not typically escaped in tag values if they don't contain other special chars,
    // but better to be safe. For metric names, they are not usually present.
    // If you expect double quotes inside your target labels, you might need:
    // s = strings.ReplaceAll(s, "\"", "\\\"")
    return s
}


// calculateAvailability calculates the overall availability percentage.
// It counts successful probes and divides by total probes.
func calculateAvailability(data []SamplePair) float64 {
	if len(data) == 0 {
		return 0.0
	}

	totalProbes := len(data)
	successfulProbes := 0
	for _, dp := range data {
		if dp.Value == 1.0 { // probe_success == 1 means success
			successfulProbes++
		}
	}
	return (float64(successfulProbes) / float64(totalProbes)) * 100.0
}

// calculateDowntime calculates the total time in seconds the system was down.
// It sums up the duration of each "0" (failure) probe_success sample.
func calculateDowntime(data []SamplePair, scrapeInterval time.Duration) float64 {
	if len(data) == 0 {
		return 0.0
	}

	totalDowntimeSeconds := 0.0
	for _, dp := range data {
		if dp.Value == 0.0 { // probe_success == 0 means failure/downtime
			totalDowntimeSeconds += scrapeInterval.Seconds()
		}
	}
	return totalDowntimeSeconds
}

// calculateNumIncidents counts the number of distinct incidents (transitions from 1 to 0).
func calculateNumIncidents(data []SamplePair) int {
	if len(data) < 2 {
		// Cannot determine transitions with less than 2 data points
		return 0
	}

	incidents := 0
	// Assume initial state is up if first probe is 1, otherwise it might be already down.
	// For incident counting, we care about 'falling'
	wasUp := data[0].Value == 1.0

	for i := 1; i < len(data); i++ {
		// If it was up and now it's down, it's a new incident
		if wasUp && data[i].Value == 0.0 {
			incidents++
		}
		// Update wasUp for the next iteration
		wasUp = data[i].Value == 1.0
	}
	return incidents
}

// calculateMTTR calculates Mean Time To Recovery (MTTR).
// It identifies downtime periods and averages their durations.
// Returns -1 if no incidents occurred.
func calculateMTTR(data []SamplePair, scrapeInterval time.Duration) float64 {
	if len(data) == 0 {
		return -1.0
	}

	var downtimeDurations []float64 // Stores duration of each incident in seconds
	inDowntime := false
	downtimeStart := time.Time{}

	for i, dp := range data {
		if dp.Value == 0.0 { // Currently down
			if !inDowntime {
				// Start of a new downtime period
				inDowntime = true
				downtimeStart = dp.Timestamp // Mark the start of this downtime incident
			}
			// If already in downtime, continue counting
		} else { // dp.Value == 1.0 (Currently up)
			if inDowntime {
				// End of a downtime period, calculate duration
				downtimeEnd := dp.Timestamp // Recovery happened at this point
				duration := downtimeEnd.Sub(downtimeStart).Seconds()
				downtimeDurations = append(downtimeDurations, duration)
				inDowntime = false
			}
			// If already up, continue being up
		}

		// Handle case where downtime extends to the very end of the data
		// This ensures that if the service is down at the end of the monitored period,
		// that downtime segment is still counted.
		// Only add if it was an actual downtime segment, not if it was already up.
		if inDowntime && i == len(data)-1 {
			duration := data[i].Timestamp.Sub(downtimeStart).Seconds() + scrapeInterval.Seconds() // Add one interval for the last down probe
			downtimeDurations = append(downtimeDurations, duration)
		}
	}

	if len(downtimeDurations) == 0 {
		return -1.0 // No incidents found or recovered
	}

	totalDowntimeDuration := 0.0
	for _, d := range downtimeDurations {
		totalDowntimeDuration += d
	}

	return totalDowntimeDuration / float64(len(downtimeDurations))
}

// formatDuration converts a time.Duration to a human-readable string.
func formatDuration(d time.Duration) string {
	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	parts := []string{}
	if days > 0 {
		parts = append(parts, fmt.Sprintf("%dd", days))
	}
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%dh", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%dm", minutes))
	}
	if seconds > 0 || len(parts) == 0 { // Always show seconds if duration is less than a minute
		parts = append(parts, fmt.Sprintf("%ds", seconds))
	}
	return fmt.Sprintf("%s", join(parts, " "))
}

// Helper to join string slice with a separator
func join(elems []string, sep string) string {
	switch len(elems) {
	case 0:
		return ""
	case 1:
		return elems[0]
	}
	n := len(sep) * (len(elems) - 1)
	for i := 0; i < len(elems); i++ {
		n += len(elems[i])
	}

	var b []byte
	b = make([]byte, 0, n)
	b = append(b, elems[0]...)
	for _, s := range elems[1:] {
		b = append(b, sep...)
		b = append(b, s...)
	}
	return string(b)
}