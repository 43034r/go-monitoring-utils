package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv" // Добавлено для парсинга года и месяца
	"time"

	prometheusClient "github.com/prometheus/client_golang/api"
	prometheusV1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

const (
	defaultPrometheusURL     = "http://localhost:8428"
	defaultJobName           = "blackbox"
	defaultTargetValue       = "http://example.com"
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
	jobName := getEnv("JOB_NAME", defaultJobName)
	targetValue := getEnv("TARGET_VALUE", defaultTargetValue)

	scrapeIntervalStr := getEnv("SCRAPE_INTERVAL", "1m") // This will be the 'step' for our raw data query
	scrapeInterval := parseDuration(scrapeIntervalStr, defaultScrapeInterval, "SCRAPE_INTERVAL")

	queryStepStr := getEnv("QUERY_STEP", "12h") // Max range for a single API query
	queryStep := parseDuration(queryStepStr, defaultQueryStep, "QUERY_STEP")

	// --- НОВЫЕ ПЕРЕМЕННЫЕ ОКРУЖЕНИЯ ДЛЯ УКАЗАНИЯ МЕСЯЦА ---
	targetMonthStr := getEnv("TARGET_MONTH", "") // Например, "5" для мая
	targetYearStr := getEnv("TARGET_YEAR", "")   // Например, "2025"

	var startTime, endTime time.Time
	var err error

	if targetMonthStr != "" && targetYearStr != "" {
		monthInt, errMonth := strconv.Atoi(targetMonthStr)
		yearInt, errYear := strconv.Atoi(targetYearStr)

		if errMonth != nil || errYear != nil || monthInt < 1 || monthInt > 12 {
			fmt.Printf("Error: Invalid TARGET_MONTH ('%s') or TARGET_YEAR ('%s'). Using last month's data.\n", targetMonthStr, targetYearStr)
			// Если ошибка парсинга, используем логику "месяц назад"
			endTime = time.Now()
			startTime = endTime.AddDate(0, -1, 0)
		} else {
			// Расчет начала и конца указанного месяца
			// Начало месяца: 1-е число указанного месяца
			startTime = time.Date(yearInt, time.Month(monthInt), 1, 0, 0, 0, 0, time.Local)
			// Конец месяца: 1-е число СЛЕДУЮЩЕГО месяца минус одна наносекунда
			// Если это декабрь, то следующий месяц будет январь следующего года
			endTime = startTime.AddDate(0, 1, 0).Add(-time.Nanosecond)

			fmt.Printf("Calculating metrics for specified month: %s %d\n", time.Month(monthInt), yearInt)
		}
	} else {
		fmt.Println("TARGET_MONTH or TARGET_YEAR not set. Using last month's data.")
		endTime = time.Now()
		startTime = endTime.AddDate(0, -1, 0)
	}

	fmt.Printf("Connecting to VictoriaMetrics at: %s\n", prometheusURL)
	fmt.Printf("Monitoring target: job=\"%s\", target=\"%s\"\n", jobName, targetValue)
	fmt.Printf("Assumed scrape interval (query step for raw data): %s\n", scrapeInterval)
	fmt.Printf("Query chunk size: %s\n", queryStep)

	client, err := prometheusClient.NewClient(prometheusClient.Config{
		Address: prometheusURL,
	})
	if err != nil {
		fmt.Printf("Error creating client: %v\n", err)
		return
	}

	api := prometheusV1.NewAPI(client)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute) // Increased timeout for potentially large data fetches
	defer cancel()

	fmt.Printf("\nCollecting raw probe_success data for the period: %s to %s\n", startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))

	var rawProbeSuccessData []SamplePair

	currentQueryTime := startTime
	for currentQueryTime.Before(endTime) {
		rangeEnd := currentQueryTime.Add(queryStep)
		if rangeEnd.After(endTime) {
			rangeEnd = endTime
		}

		query := fmt.Sprintf(`probe_success{job="%s", target="%s"}`, jobName, targetValue)
		fmt.Printf("Fetching raw data chunk: %s to %s with step %s for query: %s\n",
			currentQueryTime.Format(time.RFC3339), rangeEnd.Format(time.RFC3339), scrapeInterval, query)

		r := prometheusV1.Range{
			Start: currentQueryTime,
			End:   rangeEnd,
			Step:  scrapeInterval,
		}

		value, warnings, err := api.QueryRange(ctx, query, r)
		if err != nil {
			fmt.Printf("Error querying Prometheus for raw data: %v\n", err)
			return
		}
		if len(warnings) > 0 {
			fmt.Printf("Prometheus warnings for raw data query: %v\n", warnings)
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
			fmt.Println("Unexpected raw data query result format. Expected a Matrix.")
			return
		}

		currentQueryTime = rangeEnd
	}

	if len(rawProbeSuccessData) == 0 {
		fmt.Println("\nNo raw probe_success data collected for the specified period and target. Cannot calculate metrics.")
		return
	}

	// Sort raw data by timestamp to ensure correct processing
	sort.Slice(rawProbeSuccessData, func(i, j int) bool {
		return rawProbeSuccessData[i].Timestamp.Before(rawProbeSuccessData[j].Timestamp)
	})

	fmt.Printf("\nCollected %d raw data points.\n", len(rawProbeSuccessData))

	fmt.Println("\n--- Calculating Aggregated Metrics for the Entire Period ---")

	// Calculate Availability
	availability := calculateAvailability(rawProbeSuccessData)
	fmt.Printf("1. Доступность (Availability): %.4f%%\n", availability)

	// Calculate Downtime
	downtimeSeconds := calculateDowntime(rawProbeSuccessData, scrapeInterval)
	fmt.Printf("2. Простой системы (Total Downtime): %.2f seconds (approx. %s)\n", downtimeSeconds, formatDuration(time.Duration(downtimeSeconds)*time.Second))

	// Calculate Number of Incidents
	numIncidents := calculateNumIncidents(rawProbeSuccessData)
	fmt.Printf("3. Количество простоев (Number of Incidents): %d\n", numIncidents)

	// Calculate MTTR (Mean Time To Recovery)
	mttrSeconds := calculateMTTR(rawProbeSuccessData, scrapeInterval)
	if mttrSeconds < 0 {
		fmt.Printf("4. Среднее время восстановления (MTTR): Not applicable (no incidents or recovery)\n")
	} else {
		fmt.Printf("4. Среднее время восстановления (MTTR): %.2f seconds (approx. %s)\n", mttrSeconds, formatDuration(time.Duration(mttrSeconds)*time.Second))
	}

	fmt.Println("\n--- End of Aggregated Results ---")

	// Optional: Print raw data for debugging/verification
	// fmt.Println("\n--- Raw Probe Success Data (first 100 points) ---")
	// for i, dp := range rawProbeSuccessData {
	// 	if i >= 100 {
	// 		break
	// 	}
	// 	fmt.Printf("Time: %s, Value: %.0f\n", dp.Timestamp.Format("2006-01-02 15:04:05"), dp.Value)
	// }
	// fmt.Println("\n--- End of Raw Data ---")
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