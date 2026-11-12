package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseMdstatMultiArray(t *testing.T) {
	mdstatSample := `Personalities : [raid1] 
md0 : active raid1 sdb1[1] sda1[0]
      1047552 blocks super 1.2 [2/2] [UU]
      [==>..................]  resync = 12.4% (130432/1047552) finish=0.8min speed=18633K/sec

md1 : active raid1 sdd1[1] sdc1[0]
      20971520 blocks super 1.2 [2/1] [U_]

unused devices: <none>
`
	summary, state := parseMdstat(mdstatSample)
	if state != RaidDegraded {
		t.Fatalf("expected RaidDegraded when md1 is [U_], got state=%v (summary=%q)", state, summary)
	}
	if !strings.Contains(summary, "md1 [U_] DEGRADED") {
		t.Errorf("expected summary to report md1 [U_] DEGRADED, got %q", summary)
	}
}

func TestReadMaxThermalCPrioritizesCPUSensor(t *testing.T) {
	tmpDir := t.TempDir()
	zone0 := filepath.Join(tmpDir, "thermal_zone0")
	zone1 := filepath.Join(tmpDir, "thermal_zone1")
	_ = os.MkdirAll(zone0, 0o755)
	_ = os.MkdirAll(zone1, 0o755)

	_ = os.WriteFile(filepath.Join(zone0, "type"), []byte("acpitz\n"), 0o644)
	_ = os.WriteFile(filepath.Join(zone0, "temp"), []byte("65000\n"), 0o644)
	_ = os.WriteFile(filepath.Join(zone1, "type"), []byte("x86_pkg_temp\n"), 0o644)
	_ = os.WriteFile(filepath.Join(zone1, "temp"), []byte("58000\n"), 0o644)

	temp, sensorType, ok := readMaxThermalC(tmpDir)
	if !ok {
		t.Fatal("expected thermal reading to succeed")
	}
	if sensorType != "x86_pkg_temp" || temp != 58.0 {
		t.Fatalf("expected CPU sensor x86_pkg_temp (58.0C), got %s (%.1fC)", sensorType, temp)
	}
}

func TestRemoteNodeAnnotationsRequireFreshnessAndEd25519Signature(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	now := time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)
	freshTime := now.Add(-2 * time.Minute).Format(time.RFC3339)
	staleTime := now.Add(-15 * time.Minute).Format(time.RFC3339)

	// Sign valid payload for node-remote-valid
	validPayload := canonicalNodeAnnotationPayload("node-remote-valid", "DEGRADED", "md0 [U_] DEGRADED", "82.0", freshTime)
	validSig := hex.EncodeToString(ed25519.Sign(priv, validPayload))

	// Sign payload for stale node (signature is valid, but timestamp is expired)
	stalePayload := canonicalNodeAnnotationPayload("node-remote-stale", "DEGRADED", "md0 [U_] DEGRADED", "82.0", staleTime)
	staleSig := hex.EncodeToString(ed25519.Sign(priv, stalePayload))

	nodes := []kubeNode{
		{
			Metadata: struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			}{
				Name: "node-remote-unsigned",
				Annotations: map[string]string{
					"hwhealth.io/raid-state":   "DEGRADED",
					"hwhealth.io/raid-summary": "md0 [U_] DEGRADED",
					"hwhealth.io/cpu-temp-c":   "84.0",
					"hwhealth.io/updated-at":   freshTime,
				},
			},
		},
		{
			Metadata: struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			}{
				Name: "node-remote-stale",
				Annotations: map[string]string{
					"hwhealth.io/raid-state":   "DEGRADED",
					"hwhealth.io/raid-summary": "md0 [U_] DEGRADED",
					"hwhealth.io/cpu-temp-c":   "82.0",
					"hwhealth.io/updated-at":   staleTime,
					"hwhealth.io/signature":    staleSig,
				},
			},
		},
		{
			Metadata: struct {
				Name        string            `json:"name"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			}{
				Name: "node-remote-valid",
				Annotations: map[string]string{
					"hwhealth.io/raid-state":   "DEGRADED",
					"hwhealth.io/raid-summary": "md0 [U_] DEGRADED",
					"hwhealth.io/cpu-temp-c":   "82.0",
					"hwhealth.io/updated-at":   freshTime,
					"hwhealth.io/signature":    validSig,
				},
			},
		},
	}

	reports := evaluateClusterNodes(nodes, "node-local", 45.0, "x86_pkg_temp", true, "md0 [UU]", RaidHealthy, 75.0, now, 5*time.Minute, pubHex)
	if len(reports) != 3 {
		t.Fatalf("expected 3 reports, got %d", len(reports))
	}

	// 1. Unsigned remote node must be rejected
	if reports[0].Inspected || reports[0].Critical {
		t.Errorf("expected unsigned remote node to be rejected as uninspected, got %+v", reports[0])
	}
	// 2. Stale remote node must be rejected
	if reports[1].Inspected || reports[1].Critical {
		t.Errorf("expected stale remote node to be rejected as uninspected, got %+v", reports[1])
	}
	// 3. Fresh + validly signed remote node must be accepted and marked critical
	if !reports[2].Inspected || !reports[2].Critical {
		t.Errorf("expected fresh Ed25519-signed remote node to be inspected and critical, got %+v", reports[2])
	}
}

func TestPureEd25519LicenseAndSimulationIsolation(t *testing.T) {
	// 1. Legacy symmetric HWPRO- keys must always fail
	if valid, _ := verifyLicense("HWPRO-JEM2026A-94AE9B", false); valid {
		t.Fatal("expected legacy symmetric HWPRO- key to be rejected")
	}

	// 2. Demo key must fail outside simulation mode
	if valid, _ := verifyLicense("PRO-DEMO-2026", false); valid {
		t.Fatal("expected PRO-DEMO-2026 to be rejected when simulateMode is false")
	}

	// 3. Valid Ed25519 production key verification
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	origPub := ed25519PubKeyHex
	ed25519PubKeyHex = hex.EncodeToString(pub)
	defer func() { ed25519PubKeyHex = origPub }()

	msg := "HWPRO1.ACMECORP"
	sig := ed25519.Sign(priv, []byte(msg))
	prodToken := msg + "." + hex.EncodeToString(sig)

	valid, dryRun := verifyLicense(prodToken, false)
	if !valid || dryRun {
		t.Fatalf("expected valid Ed25519 token to succeed with dryRun=false, got valid=%v dryRun=%v", valid, dryRun)
	}

	// 4. Simulation mode MUST force dryRun=true even with a valid Ed25519 production key
	validSim, dryRunSim := verifyLicense(prodToken, true)
	if !validSim || !dryRunSim {
		t.Fatalf("expected simulateMode=true with production key to force dryRun=true, got valid=%v dryRun=%v", validSim, dryRunSim)
	}

	// 5. Simulation mode combined with --publish-node-status must be rejected
	if err := validateFlagCombinations(true, true); err == nil {
		t.Fatal("expected --simulate-raid-fail + --publish-node-status to return an error")
	}
}

func TestValidateWebhookURL(t *testing.T) {
	if _, err := validateWebhookURL("http://hooks.slack.com/services/T00/B00/X00"); err == nil {
		t.Error("expected http:// webhook URL to be rejected")
	}
	if _, err := validateWebhookURL("https://127.0.0.1/webhook"); err == nil {
		t.Error("expected loopback IP webhook to be rejected")
	}
	if _, err := validateWebhookURL("https://hooks.slack.com/services/T00/B00/X00"); err != nil {
		t.Errorf("expected valid HTTPS Slack URL to pass, got %v", err)
	}
}
