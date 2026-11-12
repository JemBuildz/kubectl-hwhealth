package health

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeTestNode(name, hostname string) kubeNode {
	var node kubeNode
	node.Metadata.Name = name
	if hostname != "" {
		node.Metadata.Labels = map[string]string{"kubernetes.io/hostname": hostname}
	}
	node.Status.NodeInfo.Architecture = "amd64"
	return node
}

func TestParseMdstatAggregatesWorstArrayState(t *testing.T) {
	sample := `Personalities : [raid1]
md0 : active raid1 sdb1[1] sda1[0]
      1047552 blocks super 1.2 [2/2] [UU]
      [==>..................]  resync = 12.4% (130432/1047552) finish=0.8min speed=18633K/sec

md1 : active raid1 sdd1[1] sdc1[0]
      20971520 blocks super 1.2 [2/1] [U_]

unused devices: <none>
`
	summary, state := parseMdstat(sample)
	if state != RAIDDegraded {
		t.Fatalf("expected degraded worst state, got %v (%s)", state, summary)
	}
	if !strings.Contains(summary, "md1 [U_] DEGRADED") {
		t.Fatalf("degraded array missing from summary: %q", summary)
	}
}

func TestParseMdstatHealthyEmptyAndUnknown(t *testing.T) {
	healthy := "Personalities : [raid1]\nmd0 : active raid1 sda1[0] sdb1[1]\n 1047552 blocks super 1.2 [2/2] [UU]\n"
	_, state := parseMdstat(healthy)
	if state != RAIDHealthy {
		t.Fatalf("expected healthy state, got %v", state)
	}

	_, state = parseMdstat("Personalities : \nunused devices: <none>\n")
	if state != RAIDNone {
		t.Fatalf("expected no arrays, got %v", state)
	}

	_, state = parseMdstat("md0 : active raid1 sda1[0] sdb1[1]\n 1000 blocks super 1.2\n")
	if state != RAIDUnknown {
		t.Fatalf("array without a member-state field must be unknown, got %v", state)
	}
}

func TestReadMaxThermalCUsesCPUZoneAndRejectsAmbientOnly(t *testing.T) {
	root := t.TempDir()
	writeZone := func(name, kind, temp string) {
		t.Helper()
		zone := filepath.Join(root, name)
		if err := os.MkdirAll(zone, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zone, "type"), []byte(kind+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zone, "temp"), []byte(temp+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeZone("thermal_zone0", "acpitz", "65000")
	writeZone("thermal_zone1", "x86_pkg_temp", "58000")

	temp, sensor, ok := readMaxThermalC(root)
	if !ok || temp != 58 || sensor != "x86_pkg_temp" {
		t.Fatalf("expected CPU package reading 58C, got %.1f %q, ok=%v", temp, sensor, ok)
	}

	if err := os.RemoveAll(filepath.Join(root, "thermal_zone1")); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := readMaxThermalC(root); ok {
		t.Fatal("ambient-only reading must not be treated as a CPU temperature")
	}
}

func TestUnknownTelemetryNeverDisplaysHealthy(t *testing.T) {
	cases := []struct {
		name   string
		tempOK bool
		raid   RAIDState
		temp   float64
	}{
		{name: "no temperature with no RAID", tempOK: false, raid: RAIDNone},
		{name: "temperature with unknown RAID", tempOK: true, raid: RAIDUnknown, temp: 40},
		{name: "nothing inspected", tempOK: false, raid: RAIDUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := evaluateNodeTelemetry("node-a", "amd64", tc.temp, "", tc.tempOK, "n/a", tc.raid, 75, false, true)
			if strings.Contains(report.StatusDisplay, "HEALTHY") || !strings.Contains(report.StatusDisplay, "UNKNOWN") {
				t.Fatalf("partial/unknown telemetry must not report healthy: %q", report.StatusDisplay)
			}
			if report.Critical {
				t.Fatalf("unknown telemetry must not report critical: %+v", report)
			}
		})
	}
}

func TestClusterEvaluationOnlyTrustsUniqueLocalMatch(t *testing.T) {
	items := []kubeNode{makeTestNode("node-local", "node-local"), makeTestNode("node-remote", "node-remote")}
	reports, count := evaluateClusterNodes(items, "node-local", false, 90, "x86_pkg_temp", true, "md0 [U_] DEGRADED", RAIDDegraded, 75)
	if count != 1 || !reports[0].Critical || !reports[0].Local {
		t.Fatalf("expected a critical report tied only to the unique local match: count=%d reports=%+v", count, reports)
	}
	if reports[1].Critical || reports[1].Local || !strings.Contains(reports[1].StatusDisplay, "UNKNOWN") {
		t.Fatalf("remote node must remain unknown and unassociated with local readings: %+v", reports[1])
	}

	ambiguous := []kubeNode{makeTestNode("node-a", "host-x"), makeTestNode("node-b", "host-x")}
	reports, count = evaluateClusterNodes(ambiguous, "host-x", false, 90, "cpu", true, "md0 [U_]", RAIDDegraded, 75)
	if count != 2 {
		t.Fatalf("expected ambiguous host identity, got %d matches", count)
	}
	for _, report := range reports {
		if report.Local || report.Critical || !strings.Contains(report.StatusDisplay, "ambiguous") {
			t.Fatalf("ambiguous identity must fail closed: %+v", report)
		}
	}
}

func TestExplicitNodeNameMatchingIsExact(t *testing.T) {
	items := []kubeNode{makeTestNode("Node-A", "host-a")}
	reports, count := evaluateClusterNodes(items, "node-a", true, 90, "cpu", true, "md0 [U_]", RAIDDegraded, 75)
	if count != 0 || reports[0].Local || reports[0].Critical {
		t.Fatalf("an explicit name must match exactly and not attach host telemetry on mismatch: count=%d reports=%+v", count, reports)
	}
}

func TestScanRejectsInvalidThresholdBeforeHostOrClusterAccess(t *testing.T) {
	_, err := Scan(context.Background(), Options{WarnTempC: math.NaN()})
	if err == nil {
		t.Fatal("NaN threshold must be rejected")
	}
}

func TestScanUsesOnlyReadOnlyKubectlQuery(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "kubectl-args")
	stub := filepath.Join(dir, "kubectl")
	const nodeJSON = `{"items":[{"metadata":{"name":"node-a","labels":{}},"status":{"nodeInfo":{"architecture":"amd64"}}}]}`
	script := "#!/bin/sh\nprintf '%s' \"$*\" > \"$KUBECTL_CALL_LOG\"\nprintf '%s' '" + nodeJSON + "'\n"
	if err := os.WriteFile(stub, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECTL_CALL_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := Scan(context.Background(), Options{KubernetesContext: "staging", NodeName: "host-no-match", SimulateRAIDFail: true})
	if err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(args), "--context staging get nodes -o json"; got != want {
		t.Fatalf("expected only a read-only node query, got kubectl args %q", got)
	}
}
