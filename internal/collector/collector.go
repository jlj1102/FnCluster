package collector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"fncluster/internal/cluster"
)

type Collector struct {
	mu        sync.Mutex
	mounts    []string
	lastTotal uint64
	lastIdle  uint64

	lastNetRx     uint64
	lastNetTx     uint64
	lastDiskRead  uint64
	lastDiskWrite uint64
	lastRateAt    time.Time
}

func New(mounts []string) *Collector {
	return &Collector{mounts: append([]string(nil), mounts...)}
}

func (c *Collector) Collect() (cluster.Metrics, error) {
	if runtime.GOOS != "linux" {
		return cluster.Metrics{}, errors.New("host metrics require Linux")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var result cluster.Metrics
	stat, err := os.ReadFile("/proc/stat")
	if err != nil {
		return result, err
	}
	total, idle, err := cpuCounters(string(stat))
	if err != nil {
		return result, err
	}
	if total > c.lastTotal && idle >= c.lastIdle {
		totalDelta, idleDelta := total-c.lastTotal, idle-c.lastIdle
		if idleDelta <= totalDelta {
			value := 100 * float64(totalDelta-idleDelta) / float64(totalDelta)
			result.CPUPercent = &value
		}
	}
	c.lastTotal, c.lastIdle = total, idle
	mem, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return result, err
	}
	result.MemoryTotal, result.MemoryUsed, err = memory(string(mem))
	if err != nil {
		return result, err
	}
	load, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return result, err
	}
	result.Load, err = loadAverage(string(load))
	if err != nil {
		return result, err
	}
	uptime, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return result, err
	}
	uptimeFields := strings.Fields(string(uptime))
	if len(uptimeFields) == 0 {
		return result, errors.New("invalid /proc/uptime")
	}
	result.UptimeSeconds, err = strconv.ParseFloat(uptimeFields[0], 64)
	if err != nil {
		return result, err
	}
	net, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return result, err
	}
	result.Network, err = network(string(net))
	if err != nil {
		return result, err
	}
	mounts := c.mounts
	if len(mounts) == 0 {
		if procMounts, readErr := os.ReadFile("/proc/mounts"); readErr == nil {
			mounts = fnOSVolumes(string(procMounts))
		}
	}
	seen := make(map[string]bool)
	for _, mount := range mounts {
		path, err := filepath.EvalSymlinks(mount)
		if err != nil {
			return result, fmt.Errorf("storage mount %q: %w", mount, err)
		}
		fs, device, err := filesystem(path)
		if err != nil {
			return result, fmt.Errorf("storage mount %q: %w", mount, err)
		}
		if seen[device] {
			continue
		}
		seen[device] = true
		result.Filesystems = append(result.Filesystems, fs)
	}
	c.rates(&result, time.Now())
	result.CollectedAt = time.Now().UTC()
	return result, nil
}

// rates turns cumulative counters into per-second values between collections.
func (c *Collector) rates(result *cluster.Metrics, now time.Time) {
	var netRx, netTx uint64
	for name, counter := range result.Network {
		if name == "lo" {
			continue
		}
		netRx += counter.ReceiveBytes
		netTx += counter.TransmitBytes
	}
	var diskRead, diskWrite uint64
	if data, err := os.ReadFile("/proc/diskstats"); err == nil {
		diskRead, diskWrite = diskTotals(string(data))
	}
	if !c.lastRateAt.IsZero() {
		seconds := now.Sub(c.lastRateAt).Seconds()
		if seconds > 0 {
			result.Rates = &cluster.Rates{
				NetworkReceiveBytesPerSecond:  perSecond(c.lastNetRx, netRx, seconds),
				NetworkTransmitBytesPerSecond: perSecond(c.lastNetTx, netTx, seconds),
				DiskReadBytesPerSecond:        perSecond(c.lastDiskRead, diskRead, seconds),
				DiskWriteBytesPerSecond:       perSecond(c.lastDiskWrite, diskWrite, seconds),
			}
		}
	}
	c.lastNetRx, c.lastNetTx = netRx, netTx
	c.lastDiskRead, c.lastDiskWrite = diskRead, diskWrite
	c.lastRateAt = now
}

func perSecond(previous, current uint64, seconds float64) float64 {
	if current < previous {
		return 0
	}
	return float64(current-previous) / seconds
}

// wholeDisk matches whole block devices, excluding partitions and virtual disks.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme\d+n\d+|mmcblk\d+)$`)

// diskTotals sums read/write bytes from /proc/diskstats (sectors are 512 bytes).
func diskTotals(input string) (uint64, uint64) {
	var read, write uint64
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !wholeDisk.MatchString(fields[2]) {
			continue
		}
		readSectors, err := strconv.ParseUint(fields[5], 10, 64)
		if err != nil {
			continue
		}
		writeSectors, err := strconv.ParseUint(fields[9], 10, 64)
		if err != nil {
			continue
		}
		read += readSectors * 512
		write += writeSectors * 512
	}
	return read, write
}

// fnOS data volumes are mounted at /volN. Ignore system, bind and nested mounts.
func fnOSVolumes(input string) []string {
	var volumes []string
	seen := map[string]bool{}
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		mount := fields[1]
		if !strings.HasPrefix(mount, "/vol") || len(mount) <= 4 || seen[mount] {
			continue
		}
		valid := true
		for _, ch := range mount[4:] {
			if ch < '0' || ch > '9' {
				valid = false
				break
			}
		}
		if valid {
			seen[mount] = true
			volumes = append(volumes, mount)
		}
	}
	return volumes
}

func cpuCounters(input string) (uint64, uint64, error) {
	fields := strings.Fields(strings.SplitN(input, "\n", 2)[0])
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, errors.New("invalid /proc/stat")
	}
	var total, idle uint64
	for i, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, err
		}
		total += value
		if i == 3 || i == 4 {
			idle += value
		}
	}
	return total, idle, nil
}

func memory(input string) (uint64, uint64, error) {
	values := map[string]uint64{}
	for _, line := range strings.Split(input, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if fields[0] == "MemTotal:" || fields[0] == "MemAvailable:" {
			value, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, 0, err
			}
			values[fields[0]] = value * 1024
		}
	}
	total, available := values["MemTotal:"], values["MemAvailable:"]
	if total == 0 || available > total {
		return 0, 0, errors.New("invalid /proc/meminfo")
	}
	return total, total - available, nil
}

func loadAverage(input string) ([3]float64, error) {
	var result [3]float64
	fields := strings.Fields(input)
	if len(fields) < 3 {
		return result, errors.New("invalid /proc/loadavg")
	}
	for i := range result {
		value, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return result, err
		}
		result[i] = value
	}
	return result, nil
}

func network(input string) (map[string]cluster.NetworkCounter, error) {
	result := make(map[string]cluster.NetworkCounter)
	for _, line := range strings.Split(input, "\n") {
		name, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		name = strings.TrimSpace(name)
		fields := strings.Fields(rest)
		if len(fields) < 16 {
			return nil, errors.New("invalid /proc/net/dev")
		}
		receive, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return nil, err
		}
		transmit, err := strconv.ParseUint(fields[8], 10, 64)
		if err != nil {
			return nil, err
		}
		result[name] = cluster.NetworkCounter{ReceiveBytes: receive, TransmitBytes: transmit}
	}
	return result, nil
}
