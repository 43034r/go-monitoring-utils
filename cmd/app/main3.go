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

	// НОВЫЕ ИМПОРТЫ для Prometheus remote_write
	"github.com/golang/snappy"                // Для Snappy-сжатия
	"github.com/prometheus/prometheus/prompb" // Для структур Prometheus remote_write (protobuf)
	"google.golang.org/protobuf/proto"        // Для сериализации protobuf
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

	// --- Определение периода расчета ---
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
	targets, err := getTargetsForJob(ctx, api, jobName, targetValueFilter, startTime, endTime)
	if err != nil {
		fmt.Printf("Error getting targets for job '%s': %v\n", jobName, err)
		return
	}
	if len(targets) == 0 {
		fmt.Printf("No targets found for job '%s' (or filtered target '%s') within the period %s to %s. Exiting.\n", jobName, targetValueFilter, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339))
		return
	}

	fmt.Printf("\nFound %d target(s) for job '%s' (after filter) within the period: %v\n", len(targets), jobName, targets)

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
			currentQueryTime = rangeQueryTime
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
			fmt.Printf("  Pushing metrics to %s...\n", prometetheusURLIn)
			labels := map[string]string{
				"job":    jobName,
				"target": target,
			}
			// Теперь вызываем sendMetrics с новыми параметрами
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
// This function now uses api.LabelValues which queries the /api/v1/label/<label_name>/values endpoint,
// AND includes the time range for the query.
func getTargetsForJob(ctx context.Context, api prometheusV1.API, job string, targetFilter string, startTime, endTime time.Time) ([]string, error) {
	// Selector to filter probe_success metrics belonging to the specific job
	selector := fmt.Sprintf(`{__name__="probe_success", job="%s"}`, job)

	// Call the LabelValues method for the "target" label
	// NOW INCLUDING startTime and endTime
	values, warnings, err := api.LabelValues(ctx, "target", []string{selector}, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("error querying label values for 'target' with selector '%s' in range %s to %s: %w", selector, startTime.Format(time.RFC3339), endTime.Format(time.RFC3339), err)
	}
	if len(warnings) > 0 {
		fmt.Printf("Warnings from LabelValues query: %v\n", warnings)
	}

	var targets []string
	for _, val := range values {
		targets = append(targets, string(val))
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

	// If no filter, return all unique targets.
	// LabelValues should typically return unique values already, but sorting is good practice.
	sort.Strings(targets)
	return targets, nil
}


// sendMetrics pushes calculated metrics to a Prometheus remote_write compatible endpoint
// using Prometheus remote_write protocol (Protobuf + Snappy compression).
func sendMetrics(url, prefix string, commonLabels map[string]string, metrics map[string]float64) {
	writeRequest := &prompb.WriteRequest{}

	for name, value := range metrics {
		metricName := prefix + name
		
		// Создаем лейблы для TimeSeries
		// "__name__" - это специальный лейбл для имени метрики в Prometheus
		labels := []prompb.Label{
			{Name: "__name__", Value: metricName},
		}
		// Добавляем общие лейблы (job, target)
		for k, v := range commonLabels {
			labels = append(labels, prompb.Label{Name: k, Value: v})
		}
		
		// Создаем сэмпл
		// Timestamp должен быть в миллисекундах Unix
		sample := prompb.Sample{
			Value:     value,
			Timestamp: time.Now().UnixNano() / int64(time.Millisecond),
		}

		// Создаем TimeSeries, содержащий лейблы и сэмплы
		ts := prompb.TimeSeries{
			Labels:  labels,
			Samples: []prompb.Sample{sample},
		}

		writeRequest.Timeseries = append(writeRequest.Timeseries, ts)
	}

	// Сериализуем WriteRequest в Protobuf
	data, err := proto.Marshal(writeRequest)
	if err != nil {
		fmt.Printf("Error marshaling Protobuf data: %v\n", err)
		return
	}

	// Сжимаем данные с помощью Snappy
	compressedData := snappy.Encode(nil, data)

	// Создаем HTTP POST запрос
	req, err := http.NewRequest("POST", url, strings.NewReader(string(compressedData)))
	if err != nil {
		fmt.Printf("Error creating HTTP request to send metrics: %v\n", err)
		return
	}

	// Устанавливаем правильные заголовки для Prometheus remote_write
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0") // Рекомендуемый заголовок

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

// Вспомогательные функции для форматирования метрик и времени (без изменений)
// ... (оставшиеся функции calculateAvailability, calculateDowntime, calculateNumIncidents, calculateMTTR, formatDuration, join остаются БЕЗ ИЗМЕНЕНИЙ)

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