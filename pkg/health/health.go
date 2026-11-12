package health

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const DefaultWarnTempC = 75.0

type RAIDState int

const (
	RAIDUnknown RAIDState = iota
	RAIDNone
	RAIDHealthy
	RAIDResyncing
	RAIDDegraded
)

// Options configures a single, read-only scan of the host running the caller.
type Options struct {
	KubernetesContext string
	NodeName          string
	WarnTempC         float64
	SimulateRAIDFail  bool
}

// Report contains observed or explicitly unknown hardware telemetry for one node.
// Local is true only when this host matched exactly one Kubernetes node.
type Report struct {
	Name          string
	Arch          string
	TempDisplay   string
	RaidSummary   string
	StatusDisplay string
	Critical      bool
	Inspected     bool
	Local         bool
	Cordoned      bool
}

// Result describes one scan. A Kubernetes query failure still returns a local-host
// report and is exposed in KubernetesError; callers must not treat that report as
// matched to a cluster node.
type Result struct {
	Reports          []Report
	LocalHost        string
	LocalMatchCount  int
	ClusterAvailable bool
	KubernetesError  error
}

type kubeNode struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		NodeInfo struct {
			Architecture string `json:"architecture"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type kubeNodeList struct {
	Items []kubeNode `json:"items"`
}

var (
	mdDeviceHeader = regexp.MustCompile(`^(md\d+)\s*:\s*(\S+)\s+(\S+)`)
	mdMemberState  = regexp.MustCompile(`\[([U_]+)\]`)
)

// Scan reads RAID and thermal data from the local Linux host and, when available,
// performs a read-only `kubectl get nodes` query to associate that data with the
// cluster. It never mutates Kubernetes resources or sends network alerts.
func Scan(ctx context.Context, options Options) (Result, error) {
	if options.WarnTempC == 0 {
		options.WarnTempC = DefaultWarnTempC
	}
	if math.IsNaN(options.WarnTempC) || math.IsInf(options.WarnTempC, 0) || options.WarnTempC <= 0 || options.WarnTempC >= 135 {
		return Result{}, fmt.Errorf("warn temperature must be a finite value greater than 0 and less than 135 Celsius")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	localHost := resolveLocalNodeName(options.NodeName)
	tempC, sensorType, tempOK := readMaxThermalC("/sys/class/thermal")
	raidSummary, raidState := checkLinuxRAID("/proc/mdstat", options.SimulateRAIDFail, runtime.GOOS)
	result := Result{LocalHost: localHost}

	nodes, err := fetchKubeNodes(ctx, options.KubernetesContext)
	if err != nil {
		result.KubernetesError = err
		result.Reports = []Report{evaluateNodeTelemetry(localHost, runtime.GOARCH, tempC, sensorType, tempOK, raidSummary, raidState, options.WarnTempC, false, false)}
		return result, nil
	}
	if len(nodes.Items) == 0 {
		result.KubernetesError = fmt.Errorf("Kubernetes returned no nodes")
		result.Reports = []Report{evaluateNodeTelemetry(localHost, runtime.GOARCH, tempC, sensorType, tempOK, raidSummary, raidState, options.WarnTempC, false, false)}
		return result, nil
	}

	result.ClusterAvailable = true
	result.Reports, result.LocalMatchCount = evaluateClusterNodes(
		nodes.Items, localHost, options.NodeName != "", tempC, sensorType, tempOK, raidSummary, raidState, options.WarnTempC,
	)
	return result, nil
}

func resolveLocalNodeName(override string) string {
	if strings.TrimSpace(override) != "" {
		return strings.TrimSpace(override)
	}
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		return "local-host"
	}
	return strings.TrimSpace(host)
}

func evaluateClusterNodes(items []kubeNode, localHost string, explicitName bool, localTemp float64, sensorType string, tempOK bool, localRaid string, localRaidState RAIDState, warnTemp float64) ([]Report, int) {
	matches := make([]bool, len(items))
	matchCount := 0
	for i, node := range items {
		match := false
		if explicitName {
			match = node.Metadata.Name == localHost
		} else {
			match = strings.EqualFold(node.Metadata.Name, localHost)
			if node.Metadata.Labels["kubernetes.io/hostname"] != "" {
				match = match || strings.EqualFold(node.Metadata.Labels["kubernetes.io/hostname"], localHost)
			}
		}
		if match {
			matches[i] = true
			matchCount++
		}
	}

	reports := make([]Report, 0, len(items)+1)
	for i, node := range items {
		if matches[i] && matchCount == 1 {
			reports = append(reports, evaluateNodeTelemetry(node.Metadata.Name, node.Status.NodeInfo.Architecture, localTemp, sensorType, tempOK, localRaid, localRaidState, warnTemp, node.Spec.Unschedulable, true))
			continue
		}
		unknownReason := "UNKNOWN (uninspected remote node)"
		if matches[i] && matchCount > 1 {
			unknownReason = "UNKNOWN (ambiguous local identity)"
		}
		reports = append(reports, unknownNodeReport(node.Metadata.Name, node.Status.NodeInfo.Architecture, unknownReason, node.Spec.Unschedulable))
	}
	if matchCount == 0 {
		reports = append(reports, evaluateNodeTelemetry(localHost, runtime.GOARCH, localTemp, sensorType, tempOK, localRaid, localRaidState, warnTemp, false, false))
	}
	return reports, matchCount
}

func unknownNodeReport(name, arch, reason string, cordoned bool) Report {
	status := reason
	if cordoned {
		status += " (CORDONED)"
	}
	return Report{
		Name: name, Arch: arch, TempDisplay: "UNKNOWN", RaidSummary: "UNKNOWN (no authenticated telemetry)",
		StatusDisplay: status, Critical: false, Inspected: false, Local: false, Cordoned: cordoned,
	}
}

func evaluateNodeTelemetry(name, arch string, tempC float64, sensorType string, tempOK bool, raidSummary string, raidState RAIDState, warnTemp float64, cordoned, local bool) Report {
	tempDisplay := "UNKNOWN (CPU sensor unavailable)"
	if tempOK {
		if sensorType != "" {
			tempDisplay = fmt.Sprintf("%.1f°C (%s)", tempC, sensorType)
		} else {
			tempDisplay = fmt.Sprintf("%.1f°C", tempC)
		}
	}

	var status string
	critical := false
	switch {
	case raidState == RAIDDegraded:
		status, critical = "CRITICAL: RAID DEGRADED", true
	case tempOK && tempC >= warnTemp:
		status, critical = "CRITICAL: THERMAL THRESHOLD", true
	case raidState == RAIDResyncing:
		status = "WARNING: RAID RESYNCING"
	case !tempOK:
		status = "UNKNOWN (CPU sensor unavailable)"
	case raidState == RAIDUnknown:
		status = "UNKNOWN (RAID state unavailable)"
	default:
		status = "HEALTHY"
	}
	if cordoned {
		status += " (CORDONED)"
	}
	return Report{
		Name: name, Arch: arch, TempDisplay: tempDisplay, RaidSummary: raidSummary,
		StatusDisplay: status, Critical: critical, Inspected: tempOK || raidState != RAIDUnknown,
		Local: local, Cordoned: cordoned,
	}
}

func checkLinuxRAID(mdstatPath string, simulateFail bool, goos string) (string, RAIDState) {
	if simulateFail {
		return "md0 [U_] (SIMULATED DEGRADED)", RAIDDegraded
	}
	if goos != "linux" {
		return fmt.Sprintf("UNKNOWN (%s host)", goos), RAIDUnknown
	}
	data, err := os.ReadFile(mdstatPath)
	if err != nil {
		return "UNKNOWN (cannot read /proc/mdstat)", RAIDUnknown
	}
	return parseMdstat(string(data))
}

func parseMdstat(content string) (string, RAIDState) {
	type arrayInfo struct {
		name       string
		detail     string
		state      RAIDState
		active     bool
		hasMembers bool
	}
	var arrays []arrayInfo
	var current *arrayInfo
	flush := func() {
		if current == nil {
			return
		}
		if !current.active && current.state != RAIDDegraded {
			current.state = RAIDUnknown
			current.detail = current.name + " (inactive/unknown)"
		} else if !current.hasMembers && current.state != RAIDResyncing && current.state != RAIDDegraded {
			current.state = RAIDUnknown
			current.detail = current.name + " (member state unavailable)"
		}
		arrays = append(arrays, *current)
		current = nil
	}

	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimSpace(raw)
		if match := mdDeviceHeader.FindStringSubmatch(line); match != nil {
			flush()
			current = &arrayInfo{name: match[1], active: match[2] == "active", state: RAIDUnknown, detail: match[1] + " (unknown)"}
		}
		if current == nil {
			continue
		}
		if match := mdMemberState.FindStringSubmatch(line); match != nil {
			members := match[1]
			current.hasMembers = true
			if strings.Contains(members, "_") {
				current.state = RAIDDegraded
				current.detail = fmt.Sprintf("%s [%s] DEGRADED", current.name, members)
			} else if current.active {
				current.state = RAIDHealthy
				current.detail = fmt.Sprintf("%s [%s]", current.name, members)
			}
		}
		if strings.Contains(line, "recovery") || strings.Contains(line, "resync") {
			if current.state != RAIDDegraded {
				current.state = RAIDResyncing
				current.detail = fmt.Sprintf("%s (RESYNCING)", current.name)
			}
		}
	}
	flush()
	if len(arrays) == 0 {
		return "NONE (0 active md arrays)", RAIDNone
	}

	worst := RAIDHealthy
	summaries := make([]string, 0, len(arrays))
	for _, array := range arrays {
		summaries = append(summaries, array.detail)
		if raidSeverity(array.state) > raidSeverity(worst) {
			worst = array.state
		}
	}
	return strings.Join(summaries, ", "), worst
}

func raidSeverity(state RAIDState) int {
	switch state {
	case RAIDDegraded:
		return 4
	case RAIDUnknown:
		return 3
	case RAIDResyncing:
		return 2
	case RAIDHealthy:
		return 1
	default:
		return 0
	}
}

func readMaxThermalC(thermalRoot string) (float64, string, bool) {
	files, err := filepath.Glob(filepath.Join(thermalRoot, "thermal_zone*", "temp"))
	if err != nil || len(files) == 0 {
		return 0, "", false
	}
	maxTemp := -1.0
	var sensorType string
	for _, tempFile := range files {
		zoneDir := filepath.Dir(tempFile)
		typeBytes, err := os.ReadFile(filepath.Join(zoneDir, "type"))
		if err != nil {
			continue
		}
		zoneType := strings.TrimSpace(string(typeBytes))
		if !isCPUSensorType(zoneType) {
			continue
		}
		raw, err := os.ReadFile(tempFile)
		if err != nil {
			continue
		}
		milliC, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			continue
		}
		tempC := float64(milliC) / 1000
		if tempC <= 0 || tempC >= 135 {
			continue
		}
		if tempC > maxTemp {
			maxTemp, sensorType = tempC, zoneType
		}
	}
	if maxTemp < 0 {
		return 0, "", false
	}
	return maxTemp, sensorType, true
}

func isCPUSensorType(zoneType string) bool {
	lower := strings.ToLower(zoneType)
	for _, keyword := range []string{"x86_pkg", "cpu", "coretemp", "k10temp", "soc", "package", "tctl"} {
		if strings.Contains(lower, keyword) {
			return true
		}
	}
	return false
}

func fetchKubeNodes(parent context.Context, kubeContext string) (kubeNodeList, error) {
	var list kubeNodeList
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	args := []string{"get", "nodes", "-o", "json"}
	if strings.TrimSpace(kubeContext) != "" {
		args = append([]string{"--context", strings.TrimSpace(kubeContext)}, args...)
	}
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.Output()
	if err != nil {
		return list, err
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return list, fmt.Errorf("invalid kubectl JSON output: %w", err)
	}
	return list, nil
}
