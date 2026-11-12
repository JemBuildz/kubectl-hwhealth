# kubectl-hwhealth 🛡️

**Catch failing bare-metal RAID drives and overheating nodes before Kubernetes loses your data.**

Standard Kubernetes (`kubelet`) only schedules workloads based on free CPU and RAM. It is completely blind when a physical Linux software RAID array (`mdadm`) drops a drive or a bare-metal server starts thermally throttling.

`kubectl-hwhealth` is a zero-dependency Go plugin that inspects physical RAID status (`/proc/mdstat`) and thermal sensors across your bare-metal Kubernetes, K3s, or homelab nodes—and automatically cordons failing hardware before your pods crash.

---

## ⚡ 10-Second Install (Free CLI)

Download the pre-compiled binary for Linux (`amd64` or Raspberry Pi `arm64`) from [Releases](../../releases/latest):

```bash
curl -sSL [https://github.com/jembuildz/kubectl-hwhealth/releases/latest/download/kubectl-hwhealth-linux-amd64](https://github.com/jembuildz/kubectl-hwhealth/releases/latest/download/kubectl-hwhealth-linux-amd64) -o kubectl-hwhealth
chmod +x kubectl-hwhealth
sudo mv kubectl-hwhealth /usr/local/bin/
```

Run a hardware check immediately:

```bash
kubectl hwhealth
```

### Example Terminal Output

```text
=== kubectl-hwhealth: Bare-Metal RAID & Thermal Scanner ===

NODE           ARCH    CPU TEMP   RAID ARRAY            HARDWARE STATUS
k3s-master-1   amd64   46.0°C     HEALTHY [UU]          HEALTHY
k3s-worker-2   amd64   81.5°C     md0 [U_] (DEGRADED)   CRITICAL: RAID DRIVE FAILED

[!] HARDWARE RISK DETECTED on 'k3s-worker-2'.
-> Upgrade to Pro to automatically cordon failing nodes & send Slack/Discord alerts:
   kubectl hwhealth --auto-drain --webhook-url=https://... --license-key=YOUR_KEY
```

---

## 🧪 Test Failure Simulation Right Now

Want to see how `kubectl-hwhealth` reacts when a physical RAID drive dies? Run:

```bash
kubectl hwhealth --simulate-raid-fail
```

Test the Pro auto-cordon and webhook engine using the built-in evaluation key:

```bash
kubectl hwhealth --simulate-raid-fail --auto-drain --license-key=PRO-DEMO-2026
```

---

## 💼 Free vs. Pro License

* **Community Edition (Free Forever):**
  * Manual CLI diagnostics (`kubectl hwhealth`)
  * Non-root Linux `/proc/mdstat` RAID health verification (`[UU]` vs `[U_]`)
  * Bare-metal CPU thermal zone scanning (`/sys/class/thermal`)
* **Pro Edition ($79 Lifetime Node License):**
  * **Automated Node Cordoning (`--auto-drain`):** Evacuate workloads the second a RAID drive degrades or CPU temps cross your threshold
  * **Instant Slack & Discord Webhooks (`--webhook-url`):** Real-time alerts before a second RAID drive fails
  * **Unattended Cron / Daemon Mode:** Run silently every 60 seconds in production clusters

👉 **[Get an Instant Pro License Key Here](#)**
