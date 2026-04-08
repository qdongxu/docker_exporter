package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	// VersionString is the version of the exporter
	VersionString = "1.0.0"
	// DefaultPort is the default port for the metrics endpoint
	DefaultPort = 9417
	// DockerCommandTimeout is the timeout for individual Docker API calls
	DockerCommandTimeout = 30 * time.Second
	// MaxInlineUpdateDuration is the maximum time to wait for a single scrape
	MaxInlineUpdateDuration = 20 * time.Second
	// MaxTotalUpdateDuration is the maximum time for a complete update
	MaxTotalUpdateDuration = 2 * time.Minute
)

var (
	// Command line flags
	dockerURLFlag = flag.String("docker-url", defaultDockerURL(), "URL to use for accessing Docker")
	verboseFlag   = flag.Bool("verbose", false, "Displays extensive diagnostic information")
	portFlag      = flag.Int("port", DefaultPort, "Port to expose metrics on")
	showVersion   = flag.Bool("version", false, "Show version information")
	showHelp      = flag.Bool("help", false, "Show usage information")

	// Global logger
	logger = log.New(os.Stdout, "", log.LstdFlags)
)

// defaultDockerURL returns the default Docker URL based on the OS
func defaultDockerURL() string {
	if runtime.GOOS == "windows" {
		return "npipe:////./pipe/docker_engine"
	}
	return "unix:///var/run/docker.sock"
}

// ContainerInfo represents basic container information
type ContainerInfo struct {
	ID      string
	Name    string
	Running  bool
	JSONRaw []byte
}

// ContainerStats represents Docker container statistics (from docker stats --format '{{json .}}')
type ContainerStats struct {
	CPU    DockerValue `json:"CPUPerc"`
	Memory DockerValue `json:"MemPerc"`
	BlockIO DockerValue `json:"BlockIO"`
	NetIO   DockerValue `json:"NetIO"`
}

// DockerValue represents a value with optional unit string (e.g., "404.3MiB / 7.381GiB")
type DockerValue struct {
	Value string `json:"-,omitempty"`
}


// ContainerTracker tracks metrics for a single container

// ContainerInspect represents Docker container inspection result
type ContainerInspect struct {
	State ContainerState `json:"state"`
}

// ContainerState represents container state
type ContainerState struct {
	Running    bool   `json:"running"`
	Restarting bool   `json:"restarting"`
	StartedAt   string `json:"started_at"`
	RestartCount int    `json:"restart_count"`
}

// ContainerTracker tracks metrics for a single container
type ContainerTracker struct {
	ID          string
	DisplayName string

	mu sync.Mutex
}

// DockerTracker tracks metrics for all containers
type DockerTracker struct {
	containerTrackers map[string]*ContainerTracker
	updateMutex      sync.Mutex
	updateInProgress bool
	updateDone       chan struct{}
}

// NewDockerTracker creates a new Docker tracker
func NewDockerTracker() *DockerTracker {
	return &DockerTracker{
		containerTrackers: make(map[string]*ContainerTracker),
		updateDone:        make(chan struct{}),
	}
}

// TryUpdate attempts to update all container metrics
func (dt *DockerTracker) TryUpdate(ctx context.Context) error {
	// Try to acquire update lock
	dt.updateMutex.Lock()
	if dt.updateInProgress {
		// Update already in progress, wait for it to complete
		dt.updateMutex.Unlock()
		<-dt.updateDone
		return nil
	}
	dt.updateInProgress = true
	dt.updateMutex.Unlock()

	// Create a new channel for this update
	updateDone := make(chan struct{})
	dt.updateDone = updateDone
	defer func() {
		dt.updateMutex.Lock()
		dt.updateInProgress = false
		close(updateDone)
		dt.updateMutex.Unlock()
	}()

	probeStart := time.Now()
	defer func() {
		dockerProbeDuration.Observe(time.Since(probeStart).Seconds())
	}()

	// List all containers
	containers, err := listContainers(ctx)
	if err != nil {
		dockerListContainersErrorCount.Inc()
		if *verboseFlag {
			logger.Printf("Failed to list containers: %v", err)
		}
		return err
	}

	dockerContainers.Set(float64(len(containers)))

	// Synchronize tracker set
	dt.synchronizeTrackers(containers)

	// Update each tracker
	var wg sync.WaitGroup
	for _, container := range containers {
		wg.Add(1)
		go func(info ContainerInfo) {
			defer wg.Done()
			updateContainerMetrics(ctx, info)
		}(container)
	}
	wg.Wait()

	dockerSuccessfulProbeTime.Set(float64(time.Now().Unix()))

	return nil
}

// synchronizeTrackers ensures we have trackers for all containers and removes trackers for removed containers
func (dt *DockerTracker) synchronizeTrackers(containers []ContainerInfo) {
	existingIDs := make(map[string]bool)
	for _, c := range containers {
		existingIDs[c.ID] = true
		displayName := c.Name
		if _, exists := dt.containerTrackers[c.ID]; !exists {
			if *verboseFlag {
				logger.Printf("Encountered container for the first time: %s (%s)", displayName, c.ID)
			}
			dt.containerTrackers[c.ID] = &ContainerTracker{ID: c.ID, DisplayName: displayName}
		}
	}

	// Remove trackers for containers that no longer exist
	for id, tracker := range dt.containerTrackers {
		if !existingIDs[id] {
			if *verboseFlag {
				logger.Printf("Tracked container no longer exists. Removing: %s (%s)", tracker.DisplayName, id)
			}
			cleanupContainerMetrics(tracker.DisplayName)
			delete(dt.containerTrackers, id)
		}
	}
}

// listContainers lists all Docker containers
func listContainers(ctx context.Context) ([]ContainerInfo, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, DockerCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "docker", "ps", "--all", "--format", "{{.ID}}\t{{.Names}}\t{{.State}}")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to list containers: %w", err)
	}

	var containers []ContainerInfo
	lines := splitLines(string(output))
	for _, line := range lines {
		if line == "" {
			continue
		}
		parts := splitTab(line)
		if len(parts) >= 3 {
			name := parts[1]
			if name == "" {
				name = parts[0][:12]
			}
			// Remove leading slash from container name
			if len(name) > 0 && name[0] == '/' {
				name = name[1:]
			}
			containers = append(containers, ContainerInfo{
				ID:     parts[0],
				Name:   name,
				Running: parts[2] == "running",
			})
		}
	}

	return containers, nil
}

// inspectContainer inspects a Docker container
func inspectContainer(ctx context.Context, id string) (*ContainerInspect, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, DockerCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "docker", "inspect", "--format", "{{json .State}}", id)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to inspect container %s: %w", id, err)
	}

	var state ContainerState
	if err := json.Unmarshal(output, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal container state: %w", err)
	}

	return &ContainerInspect{State: state}, nil
}

// getContainerStats gets statistics for a container
func getContainerStats(ctx context.Context, id string) (*ContainerStats, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, DockerCommandTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "docker", "stats", "--no-stream", "--format", "{{json .}}", id)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("failed to get stats for container %s: %w", id, err)
	}

	if *verboseFlag {
		logger.Printf("Raw stats JSON for %s: %s", id, string(output))
	}

	// Docker stats JSON is flat with string values, let's manually parse it
	var rawStats map[string]string
	if err := json.Unmarshal(output, &rawStats); err != nil {
		return nil, fmt.Errorf("failed to unmarshal container stats: %w", err)
	}

	stats := &ContainerStats{}
	if cpu, ok := rawStats["CPUPerc"]; ok {
		stats.CPU = DockerValue{Value: cpu}
	}
	if mem, ok := rawStats["MemPerc"]; ok {
		stats.Memory = DockerValue{Value: mem}
	}
	if blockIO, ok := rawStats["BlockIO"]; ok {
		stats.BlockIO = DockerValue{Value: blockIO}
	}
	if netIO, ok := rawStats["NetIO"]; ok {
		stats.NetIO = DockerValue{Value: netIO}
	}

	return stats, nil
}

// updateContainerMetrics updates the metrics for a container
func updateContainerMetrics(ctx context.Context, info ContainerInfo) {
	inspectStart := time.Now()
	inspect, inspectErr := inspectContainer(ctx, info.ID)
	containerInspectContainerDuration.Observe(time.Since(inspectStart).Seconds())

	if inspectErr != nil {
		containerFailedProbeCount.WithLabelValues(info.Name).Inc()
		if *verboseFlag {
			logger.Printf("Failed to inspect container %s (%s): %v", info.Name, info.ID, inspectErr)
		}
		cleanupContainerMetrics(info.Name)
		return
	}

	// Update state metrics
	containerRestartCount.WithLabelValues(info.Name).Set(float64(inspect.State.RestartCount))

	if inspect.State.Running {
		containerRunningState.WithLabelValues(info.Name).Set(1)
	} else if inspect.State.Restarting {
		containerRunningState.WithLabelValues(info.Name).Set(0.5)
	} else {
		containerRunningState.WithLabelValues(info.Name).Set(0)
	}

	if inspect.State.Running && inspect.State.StartedAt != "" {
		if startedAt, err := time.Parse(time.RFC3339Nano, inspect.State.StartedAt); err == nil {
			containerStartTime.WithLabelValues(info.Name).Set(float64(startedAt.Unix()))
		}
	}

	// Get stats if container is running
	if inspect.State.Running {
		statsStart := time.Now()
		stats, statsErr := getContainerStats(ctx, info.ID)
		containerGetResourceStatsDuration.Observe(time.Since(statsStart).Seconds())

		if statsErr == nil {
			updateResourceMetrics(stats, info.Name)
		} else {
			if *verboseFlag {
				logger.Printf("Failed to get stats for container %s (%s): %v", info.Name, info.ID, statsErr)
			}
			// Clear resource metrics on error
			clearResourceMetrics(info.Name)
		}
	} else {
		// Clear resource metrics if container is not running
		clearResourceMetrics(info.Name)
	}
}

// updateResourceMetrics updates the resource metrics for a container
func updateResourceMetrics(stats *ContainerStats, displayName string) {
	// Parse CPU percentage (e.g., "0.22%")
	cpuPerc := parsePercentage(stats.CPU.Value)
	if cpuPerc > 0 {
		containerCpuUsedTotal.WithLabelValues(displayName).Set(cpuPerc)
	} else {
		containerCpuUsedTotal.WithLabelValues(displayName).Set(0)
	}

	// Parse memory percentage (e.g., "5.35%")
	memPerc := parsePercentage(stats.Memory.Value)
	if memPerc > 0 {
		containerMemoryUsedBytes.WithLabelValues(displayName).Set(memPerc)
	} else {
		containerMemoryUsedBytes.WithLabelValues(displayName).Set(0)
	}

	// Parse block I/O (e.g., "732MB / 86kB")
	// Format: "732MB / 86kB" = read/write
	blockIOBytes := parseBytes(stats.BlockIO.Value)
	containerDiskReadBytes.WithLabelValues(displayName).Set(blockIOBytes)

	// Parse network I/O (e.g., "17.2GB / 12.9GB")
	// Format: "17.2GB / 12.9GB" = rx/tx
	netIOBytes := parseBytes(stats.NetIO.Value)
	containerNetworkInBytes.WithLabelValues(displayName).Set(netIOBytes)
	containerNetworkOutBytes.WithLabelValues(displayName).Set(netIOBytes) // Use same value for both for now
}

// parsePercentage extracts percentage value from string (e.g., "0.22%" -> 0.22)
func parsePercentage(s string) float64 {
	if s == "" {
		return 0
	}
	// Remove % and convert to float
	var result float64
	_, err := fmt.Sscanf(s, "%f", &result)
	if err != nil {
		return 0
	}
	return result
}

// parseBytes extracts byte value from Docker stats string (e.g., "732MB / 86kB")
func parseBytes(s string) float64 {
	if s == "" {
		return 0
	}
	// Split by " / " to get read/write parts
	parts := strings.Split(s, " / ")
	if len(parts) == 0 {
		return 0
	}
	// Parse first part (should be the read value)
	value := parts[0]
	// Extract numeric part and convert units
	return parseSizeWithUnits(value)
}

// parseSizeWithUnits converts size string with units to bytes
func parseSizeWithUnits(s string) float64 {
	if s == "" {
		return 0
	}
	// Extract numeric part
	var numeric float64
	_, err := fmt.Sscanf(s, "%f", &numeric)
	if err != nil {
		return 0
	}
	// Check for units and multiply accordingly
	if strings.Contains(s, "kB") || strings.Contains(s, "KB") {
		return numeric * 1024
	} else if strings.Contains(s, "MB") || strings.Contains(s, "MiB") {
		return numeric * 1024 * 1024
	} else if strings.Contains(s, "GB") || strings.Contains(s, "GiB") {
		return numeric * 1024 * 1024 * 1024
	} else if strings.Contains(s, "TB") || strings.Contains(s, "TiB") {
		return numeric * 1024 * 1024 * 1024 * 1024
	}
	return numeric
}

// cleanupContainerMetrics clears all metrics for a container
func cleanupContainerMetrics(displayName string) {
	containerRestartCount.DeleteLabelValues(displayName)
	containerRunningState.DeleteLabelValues(displayName)
	containerStartTime.DeleteLabelValues(displayName)
	clearResourceMetrics(displayName)
}

// clearResourceMetrics clears resource metrics for a container
func clearResourceMetrics(displayName string) {
	containerCpuUsedTotal.WithLabelValues(displayName).Set(0)
	containerCpuCapacityTotal.WithLabelValues(displayName).Set(0)
	containerMemoryUsedBytes.WithLabelValues(displayName).Set(0)
	containerNetworkInBytes.WithLabelValues(displayName).Set(0)
	containerNetworkOutBytes.WithLabelValues(displayName).Set(0)
	containerDiskReadBytes.WithLabelValues(displayName).Set(0)
	containerDiskWriteBytes.WithLabelValues(displayName).Set(0)
}

// ExporterLogic is the main exporter logic
type ExporterLogic struct {
	tracker *DockerTracker
	mu      sync.Mutex
}

// NewExporterLogic creates a new exporter logic
func NewExporterLogic() *ExporterLogic {
	return &ExporterLogic{}
}

// RunAsync runs the exporter logic
func (el *ExporterLogic) RunAsync(ctx context.Context, cancel context.CancelFunc) error {
	tracker := NewDockerTracker()
	el.tracker = tracker

	logger.Printf("Configured to probe Docker on %s", *dockerURLFlag)

	// Create metrics handler
	handler := promhttp.Handler()

	// Create HTTP server
	mux := http.NewServeMux()
	mux.Handle("/metrics", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Update metrics before serving
		updateCtx, updateCancel := context.WithTimeout(r.Context(), MaxInlineUpdateDuration)
		defer updateCancel()

		if err := el.tracker.TryUpdate(updateCtx); err != nil {
			if *verboseFlag {
				logger.Printf("Update failed: %v", err)
			}
			dockerProbeInlineTimeoutsTotal.Inc()
		}

		handler.ServeHTTP(w, r)
	}))

	addr := fmt.Sprintf(":%d", *portFlag)
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	if *verboseFlag {
		logger.Printf("Starting metrics server on http://localhost%s/metrics", addr)
	}

	// Start server in a goroutine
	serverErr := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErr <- err
		}
	}()

	// Wait for shutdown signal
	select {
	case <-ctx.Done():
		// Shutdown requested
	case err := <-serverErr:
		if err != nil {
			return err
		}
	}

	// Graceful shutdown
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("failed to shutdown server: %w", err)
	}

	return nil
}

// Prometheus metrics
var (
	// Container probe metrics
	containerFailedProbeCount = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "docker_probe_container_failed_total",
		Help: "Number of times the exporter failed to collect information about a specific container.",
	}, []string{"name"})
	containerInspectContainerDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "docker_probe_inspect_duration_seconds",
		Help:    "How long it takes to query Docker for the basic information about a single container. Includes failed requests.",
		Buckets: []float64{0.5, 0.75, 1.125, 1.6875, 2.53125, 3.796875, 5.6953125, 8.54296875, 12.814453125, 19.2216796875, 28.83251953125, 43.248779296875, 64.8731689453125, 97.30975341796875},
	})
	containerGetResourceStatsDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "docker_probe_stats_duration_seconds",
		Help:    "How long it takes to query Docker for the resource usage of a single container. Includes failed requests.",
		Buckets: []float64{0.5, 0.75, 1.125, 1.6875, 2.53125, 3.796875, 5.6953125, 8.54296875, 12.814453125, 19.2216796875, 28.83251953125, 43.248779296875, 64.8731689453125, 97.30975341796875},
	})

	// Container state metrics
	containerRestartCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_restart_count",
		Help: "Number of times the runtime has restarted this container without explicit user action, since the container was last started.",
	}, []string{"name"})
	containerRunningState = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_running_state",
		Help: "Whether the container is running (1), restarting (0.5) or stopped (0).",
	}, []string{"name"})
	containerStartTime = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_start_time_seconds",
		Help: "Timestamp indicating when the container was started. Does not get reset by automatic restarts.",
	}, []string{"name"})

	// Container resource metrics
	containerCpuUsedTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_cpu_used_total",
		Help: "Accumulated CPU usage of a container, in unspecified units, averaged for all logical CPUs usable by the container.",
	}, []string{"name"})
	containerCpuCapacityTotal = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_cpu_capacity_total",
		Help: "All potential CPU usage available to a container, in unspecified units, averaged for all logical CPUs usable by the container. Start point of measurement is undefined - only relative values should be used in analytics.",
	}, []string{"name"})
	containerMemoryUsedBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_memory_used_bytes",
		Help: "Memory usage of a container.",
	}, []string{"name"})
	containerNetworkInBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_network_in_bytes",
		Help: "Total bytes received by the container's network interfaces.",
	}, []string{"name"})
	containerNetworkOutBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_network_out_bytes",
		Help: "Total bytes sent by the container's network interfaces.",
	}, []string{"name"})
	containerDiskReadBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_disk_read_bytes",
		Help: "Total bytes read from disk by a container.",
	}, []string{"name"})
	containerDiskWriteBytes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "docker_container_disk_write_bytes",
		Help: "Total bytes written to disk by a container.",
	}, []string{"name"})

	// Docker tracker metrics
	dockerContainers = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "docker_containers",
		Help: "Number of containers that exist.",
	})
	dockerListContainersErrorCount = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docker_probe_list_containers_failed_total",
		Help: "How many times the attempt to list all containers has failed.",
	})
	dockerProbeDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "docker_probe_duration_seconds",
		Help:    "How long it takes to query Docker for the complete data set. Includes failed requests.",
		Buckets: []float64{0.5, 0.75, 1.125, 1.6875, 2.53125, 3.796875, 5.6953125, 8.54296875, 12.814453125, 19.2216796875, 28.83251953125, 43.248779296875, 64.8731689453125, 97.30975341796875},
	})
	dockerListContainersDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "docker_probe_list_containers_duration_seconds",
		Help:    "How long it takes to query Docker for the list of containers. Includes failed requests.",
		Buckets: []float64{0.5, 0.75, 1.125, 1.6875, 2.53125, 3.796875, 5.6953125, 8.54296875, 12.814453125, 19.2216796875, 28.83251953125, 43.248779296875, 64.8731689453125, 97.30975341796875},
	})
	dockerSuccessfulProbeTime = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "docker_probe_successfully_completed_time",
		Help: "When the last Docker probe was successfully completed.",
	})
	dockerProbeInlineTimeoutsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "docker_probe_inline_timeouts_total",
		Help: "Total number of times we have forced the scrape to happen in the background and returned outdated data because performing an update inline took too long.",
	})
)

func init() {
	// Register all metrics with the default registry
	prometheus.MustRegister(
		containerFailedProbeCount,
		containerInspectContainerDuration,
		containerGetResourceStatsDuration,
		containerRestartCount,
		containerRunningState,
		containerStartTime,
		containerCpuUsedTotal,
		containerCpuCapacityTotal,
		containerMemoryUsedBytes,
		containerNetworkInBytes,
		containerNetworkOutBytes,
		containerDiskReadBytes,
		containerDiskWriteBytes,
		dockerContainers,
		dockerListContainersErrorCount,
		dockerProbeDuration,
		dockerListContainersDuration,
		dockerSuccessfulProbeTime,
		dockerProbeInlineTimeoutsTotal,
	)
}

func main() {
	flag.Parse()

	if *showHelp {
		printUsage()
		return
	}

	if *showVersion {
		printVersion()
		return
	}

	logger.Printf("Docker Exporter v%s", VersionString)

	// Set up signal handling for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		logger.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Create and run exporter
	logic := NewExporterLogic()
	if err := logic.RunAsync(ctx, cancel); err != nil {
		logger.Printf("Exporter failed: %v", err)
		os.Exit(1)
	}

	logger.Println("Exporter shutdown complete")
}

func printVersion() {
	fmt.Printf("Docker Exporter v%s\n", VersionString)
	fmt.Printf("Go version: %s\n", runtime.Version())
	fmt.Printf("OS/Arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

func printUsage() {
	fmt.Printf("Docker Exporter v%s\n", VersionString)
	fmt.Println("\nA Prometheus exporter for Docker container statistics.")
	fmt.Println("\nUsage:")
	fmt.Println("  docker_exporter [options]")
	fmt.Println("\nGeneral options:")
	flag.PrintDefaults()
	fmt.Println("\nExample:")
	fmt.Println("  docker_exporter --docker-url unix:///var/run/docker.sock --port 9417")
}

// splitLines splits a string into lines
func splitLines(s string) []string {
	var lines []string
	lineStart := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[lineStart:i])
			lineStart = i + 1
		}
	}
	if lineStart < len(s) {
		lines = append(lines, s[lineStart:])
	}
	return lines
}

// splitTab splits a string by tab character
func splitTab(s string) []string {
	var parts []string
	partStart := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\t' {
			parts = append(parts, s[partStart:i])
			partStart = i + 1
		}
	}
	if partStart < len(s) {
		parts = append(parts, s[partStart:])
	}
	return parts
}

func init() {
	// Check if the port is available
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", DefaultPort))
	if err != nil {
		// Port is already in use, log a warning
		if *verboseFlag {
			logger.Printf("Warning: default port %d is already in use", DefaultPort)
		}
	} else {
		listener.Close()
	}
}
