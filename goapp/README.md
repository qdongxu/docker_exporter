# Docker Exporter (Go)

A Prometheus exporter for Docker container statistics, ported from C# to Go. This version uses the Docker CLI instead of the Docker API, making it simpler and more portable.

## Features

- Exports Docker container metrics to Prometheus
- Tracks container state (running, restarting, stopped), restart count, and start time
- Tracks resource metrics: CPU usage, memory usage, network I/O, disk I/O
- Rate limiting to avoid overloading the Docker CLI
- Cross-platform support (Linux, Windows, macOS)
- Single executable binary - no Dockerfile required
- Uses Docker CLI commands instead of Docker API for simpler dependency management

## Prerequisites

- Go 1.23 or later (for building)
- Docker (for running the exporter - requires Docker CLI to be installed and available)
- Git

## Building

### Build Instructions

1. Navigate to `goapp` directory:

```bash
cd goapp
```

2. Download dependencies:

```bash
go mod download
```

3. Build the binary:

```bash
go build -o docker_exporter .
```

This will create a `docker_exporter` binary in the current directory.

### Building for Different Platforms

#### Linux

```bash
GOOS=linux GOARCH=amd64 go build -o docker_exporter-linux-amd64 .
```

#### Windows

```bash
GOOS=windows GOARCH=amd64 go build -o docker_exporter-windows-amd64.exe .
```

#### macOS (Intel)

```bash
GOOS=darwin GOARCH=amd64 go build -o docker_exporter-darwin-amd64 .
```

#### macOS (Apple Silicon)

```bash
GOOS=darwin GOARCH=arm64 go build -o docker_exporter-darwin-arm64 .
```

## Usage

### Command Line Options

```
Usage:
  docker_exporter [options]

General options:
  -docker-url string
        URL to use for accessing Docker (default "unix:///var/run/docker.sock" on Unix, "npipe:////./pipe/docker_engine" on Windows)
  -help
        Show usage information
  -port int
        Port to expose metrics on (default 9417)
  -verbose
        Displays extensive diagnostic information
  -version
        Show version information
```

### Running

#### On Linux/Unix

```bash
./docker_exporter
```

#### On Windows

```powershell
.\docker_exporter.exe
```

#### With Custom Docker URL

```bash
./docker_exporter --docker-url unix:///var/run/docker.sock
```

#### With Custom Port

```bash
./docker_exporter --port 9090
```

### Docker CLI Requirements

The exporter requires Docker CLI to be installed and accessible on the system PATH. It uses the following Docker CLI commands:

- `docker ps --all --format` - to list containers
- `docker inspect --format` - to get container state
- `docker stats --no-stream --format` - to get container statistics

Make sure the user running the exporter has permissions to execute these Docker CLI commands (e.g., add user to `docker` group on Linux).

## Metrics

The exporter exposes the following metrics on `/metrics`:

### Container State Metrics

- `docker_container_restart_count` - Number of times the runtime has restarted this container
- `docker_container_running_state` - Whether the container is running (1), restarting (0.5) or stopped (0)
- `docker_container_start_time_seconds` - Timestamp indicating when the container was started

### Container Resource Metrics

- `docker_container_cpu_used_total` - Accumulated CPU usage of a container
- `docker_container_cpu_capacity_total` - All potential CPU usage available to a container
- `docker_container_memory_used_bytes` - Memory usage of a container
- `docker_container_network_in_bytes` - Total bytes received by the container's network interfaces
- `docker_container_network_out_bytes` - Total bytes sent by the container's network interfaces
- `docker_container_disk_read_bytes` - Total bytes read from disk by a container
- `docker_container_disk_write_bytes` - Total bytes written to disk by a container

### Docker Probe Metrics

- `docker_containers` - Number of containers that exist
- `docker_probe_container_failed_total` - Number of times the exporter failed to collect information about a specific container
- `docker_probe_inspect_duration_seconds` - How long it takes to query Docker for basic information about a single container
- `docker_probe_stats_duration_seconds` - How long it takes to query Docker for resource usage of a single container
- `docker_probe_list_containers_failed_total` - How many times the attempt to list all containers has failed
- `docker_probe_duration_seconds` - How long it takes to query Docker for the complete data set
- `docker_probe_list_containers_duration_seconds` - How long it takes to query Docker for the list of containers
- `docker_probe_successfully_completed_time` - When the last Docker probe was successfully completed
- `docker_probe_inline_timeouts_total` - Number of times a scrape returned stale data because the update took too long

### Example Prometheus Configuration

Add this to your `prometheus.yml`:

```yaml
scrape_configs:
  - job_name: 'docker'
    static_configs:
      - targets: ['localhost:9417']
```

## Development

### Running in Development

```bash
go run .
```

## License

This is a port of the original C# docker_exporter by prometheus-net. See the original repository for license information.

## Differences from C# Version

- Uses Docker CLI instead of Docker API library
- Simpler dependency management (no Docker Go SDK)
- Uses Go 1.23+ features like `any` type
- Single binary deployment without Dockerfile
- Cross-platform support through standard library
