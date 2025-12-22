package network

import (
	"fmt"
	"log"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type Degrader struct {
	mu      sync.Mutex
	os      string
	iface   string
	active  bool
	pipeNum int // For macOS dnctl pipe numbers
}

func NewDegrader() *Degrader {
	d := &Degrader{
		os:      runtime.GOOS,
		pipeNum: 1,
	}

	switch d.os {
	case "linux":
		d.iface = d.detectLinuxInterface()
	case "darwin":
		d.iface = d.detectDarwinInterface()
	default:
		d.iface = "lo"
	}

	log.Printf("[Network] Initialized degrader for %s on interface %s", d.os, d.iface)
	return d
}

// Finds the default network interface on Linux
func (d *Degrader) detectLinuxInterface() string {
	// Try to find the default route interface
	out, err := exec.Command("ip", "route", "show", "default").Output()
	if err == nil {
		parts := strings.Fields(string(out))
		for i, p := range parts {
			if p == "dev" && i+1 < len(parts) {
				return parts[i+1]
			}
		}
	}

	// Fallback to common interfaces
	for _, iface := range []string{"eth0", "ens0", "enp0s3", "docker0", "lo"} {
		if _, err := exec.Command("ip", "link", "show", iface).Output(); err == nil {
			return iface
		}
	}

	return "lo"
}

// Finds the default network interface on macOS
func (d *Degrader) detectDarwinInterface() string {
	out, err := exec.Command("route", "-n", "get", "default").Output()
	if err == nil {
		for line := range strings.SplitSeq(string(out), "\n") {
			if strings.Contains(line, "interface:") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					return parts[1]
				}
			}
		}
	}

	return "lo0"
}

// Sets the network interface to use
func (d *Degrader) SetInterface(iface string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.iface = iface
}

// Applies packet loss using OS-specific tools
func (d *Degrader) ApplyPacketLoss(lossPct int, port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if lossPct < 0 || lossPct > 100 {
		return fmt.Errorf("loss percentage must be 0-100, got %d", lossPct)
	}

	switch d.os {
	case "linux":
		return d.applyLinuxPacketLoss(lossPct, port)
	case "darwin":
		return d.applyDarwinPacketLoss(lossPct, port)
	default:
		return fmt.Errorf("unsupported OS: %s", d.os)
	}
}

// Applies tc netem for packet loss on Linux
func (d *Degrader) applyLinuxPacketLoss(lossPct int, port int) error {
	// First, remove any existing qdisc
	exec.Command("tc", "qdisc", "del", "dev", d.iface, "root").Run()

	// Add root qdisc with handle
	cmd := exec.Command("tc", "qdisc", "add", "dev", d.iface, "root", "handle", "1:", "prio")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add root qdisc: %v, output: %s", err, out)
	}

	// Add netem qdisc with packet loss
	cmd = exec.Command("tc", "qdisc", "add", "dev", d.iface, "parent", "1:3", "handle", "30:",
		"netem", "loss", fmt.Sprintf("%d%%", lossPct))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add netem qdisc: %v, output: %s", err, out)
	}

	// Add filter for specific port if specified
	if port > 0 {
		cmd = exec.Command("tc", "filter", "add", "dev", d.iface, "protocol", "ip", "parent", "1:",
			"prio", "3", "u32", "match", "ip", "dport", strconv.Itoa(port), "0xffff", "flowid", "1:3")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("[Network] Warning: failed to add port filter: %v, output: %s", err, out)
		}
	}

	d.active = true
	log.Printf("[Network] Applied %d%% packet loss on %s (port %d)", lossPct, d.iface, port)
	return nil
}

// Applies pfctl and dnctl for packet loss on macOS
func (d *Degrader) applyDarwinPacketLoss(lossPct int, port int) error {
	pipeNum := d.pipeNum
	d.pipeNum++

	// Configure dnctl pipe with packet loss
	cmd := exec.Command("dnctl", "pipe", strconv.Itoa(pipeNum), "config", "plr", fmt.Sprintf("%.2f", float64(lossPct)/100.0))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure dnctl pipe: %v, output: %s", err, out)
	}

	// Create pf rules file
	rules := fmt.Sprintf("dummynet out proto udp from any to any port %d pipe %d\n", port, pipeNum)
	if port == 0 {
		rules = fmt.Sprintf("dummynet out proto udp from any to any pipe %d\n", pipeNum)
	}

	// Write rules to file
	rulesFile := fmt.Sprintf("/tmp/chaos_pf_rules_%d.conf", pipeNum)
	if err := exec.Command("sh", "-c", fmt.Sprintf("echo '%s' > %s", rules, rulesFile)).Run(); err != nil {
		return fmt.Errorf("failed to write pf rules: %v", err)
	}

	// Load rules into pf
	cmd = exec.Command("pfctl", "-f", rulesFile)
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[Network] Warning: pfctl load failed: %v, output: %s", err, out)
	}

	// Enable pf
	exec.Command("pfctl", "-e").Run()

	d.active = true
	log.Printf("[Network] Applied %d%% packet loss on port %d using dnctl pipe %d", lossPct, port, pipeNum)
	return nil
}

// Applies network latency
func (d *Degrader) ApplyLatency(delayMs int, jitterMs int, port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch d.os {
	case "linux":
		return d.applyLinuxLatency(delayMs, jitterMs, port)
	case "darwin":
		return d.applyDarwinLatency(delayMs, jitterMs, port)
	default:
		return fmt.Errorf("unsupported OS: %s", d.os)
	}
}

// Applies tc netem for latency on Linux
func (d *Degrader) applyLinuxLatency(delayMs int, jitterMs int, port int) error {
	// First, remove any existing qdisc
	exec.Command("tc", "qdisc", "del", "dev", d.iface, "root").Run()

	// Add root qdisc
	cmd := exec.Command("tc", "qdisc", "add", "dev", d.iface, "root", "handle", "1:", "prio")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add root qdisc: %v, output: %s", err, out)
	}

	// Adds netem qdisc with delay
	args := []string{"qdisc", "add", "dev", d.iface, "parent", "1:3", "handle", "30:", "netem",
		"delay", fmt.Sprintf("%dms", delayMs)}
	if jitterMs > 0 {
		args = append(args, fmt.Sprintf("%dms", jitterMs), "distribution", "normal")
	}

	cmd = exec.Command("tc", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add netem delay: %v, output: %s", err, out)
	}

	// Adds filter for specific port if specified
	if port > 0 {
		cmd = exec.Command("tc", "filter", "add", "dev", d.iface, "protocol", "ip", "parent", "1:",
			"prio", "3", "u32", "match", "ip", "dport", strconv.Itoa(port), "0xffff", "flowid", "1:3")
		cmd.Run()
	}

	d.active = true
	log.Printf("[Network] Applied %dms delay (±%dms jitter) on %s", delayMs, jitterMs, d.iface)
	return nil
}

// Applies dnctl for latency on macOS
func (d *Degrader) applyDarwinLatency(delayMs int, _ int, port int) error {
	pipeNum := d.pipeNum
	d.pipeNum++

	// Configure dnctl pipe with delay
	cmd := exec.Command("dnctl", "pipe", strconv.Itoa(pipeNum), "config", "delay", fmt.Sprintf("%dms", delayMs))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure dnctl pipe: %v, output: %s", err, out)
	}

	// Create pf rules
	rules := fmt.Sprintf("dummynet out proto udp from any to any port %d pipe %d\n", port, pipeNum)
	if port == 0 {
		rules = fmt.Sprintf("dummynet out proto udp from any to any pipe %d\n", pipeNum)
	}

	rulesFile := fmt.Sprintf("/tmp/chaos_pf_rules_%d.conf", pipeNum)
	exec.Command("sh", "-c", fmt.Sprintf("echo '%s' > %s", rules, rulesFile)).Run()

	exec.Command("pfctl", "-f", rulesFile).Run()
	exec.Command("pfctl", "-e").Run()

	d.active = true
	log.Printf("[Network] Applied %dms delay on port %d using dnctl pipe %d", delayMs, port, pipeNum)
	return nil
}

// Applies bandwidth limiting
func (d *Degrader) ApplyBandwidthLimit(kbps int, port int) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch d.os {
	case "linux":
		return d.applyLinuxBandwidth(kbps, port)
	case "darwin":
		return d.applyDarwinBandwidth(kbps, port)
	default:
		return fmt.Errorf("unsupported OS: %s", d.os)
	}
}

// Applies bandwidth limiting using tc tbf for Linux
func (d *Degrader) applyLinuxBandwidth(kbps int, _ int) error {
	// Remove existing qdisc
	exec.Command("tc", "qdisc", "del", "dev", d.iface, "root").Run()

	// Add tbf (token bucket filter) for rate limiting
	rate := fmt.Sprintf("%dkbit", kbps)
	burst := fmt.Sprintf("%dk", kbps/8) // burst in bytes

	cmd := exec.Command("tc", "qdisc", "add", "dev", d.iface, "root", "tbf",
		"rate", rate, "burst", burst, "latency", "50ms")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add tbf qdisc: %v, output: %s", err, out)
	}

	d.active = true
	log.Printf("[Network] Applied %d kbps bandwidth limit on %s", kbps, d.iface)
	return nil
}

// Applies bandwidth limiting using dnctl for macOS
func (d *Degrader) applyDarwinBandwidth(kbps int, port int) error {
	pipeNum := d.pipeNum
	d.pipeNum++

	// Configure dnctl pipe with bandwidth
	cmd := exec.Command("dnctl", "pipe", strconv.Itoa(pipeNum), "config", "bw", fmt.Sprintf("%dKbit/s", kbps))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to configure dnctl pipe: %v, output: %s", err, out)
	}

	rules := fmt.Sprintf("dummynet out proto udp from any to any port %d pipe %d\n", port, pipeNum)
	if port == 0 {
		rules = fmt.Sprintf("dummynet out proto udp from any to any pipe %d\n", pipeNum)
	}

	rulesFile := fmt.Sprintf("/tmp/chaos_pf_rules_%d.conf", pipeNum)
	exec.Command("sh", "-c", fmt.Sprintf("echo '%s' > %s", rules, rulesFile)).Run()

	exec.Command("pfctl", "-f", rulesFile).Run()
	exec.Command("pfctl", "-e").Run()

	d.active = true
	log.Printf("[Network] Applied %d kbps bandwidth limit on port %d", kbps, port)
	return nil
}

// Removes all network degradation rules
func (d *Degrader) RemoveAll() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	switch d.os {
	case "linux":
		return d.removeLinuxRules()
	case "darwin":
		return d.removeDarwinRules()
	default:
		return nil
	}
}

// Removes all tc rules on Linux
func (d *Degrader) removeLinuxRules() error {
	cmd := exec.Command("tc", "qdisc", "del", "dev", d.iface, "root")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("[Network] Warning: failed to remove tc rules: %v, output: %s", err, out)
	}

	d.active = false
	log.Printf("[Network] Removed all tc rules on %s", d.iface)
	return nil
}

// Removes all pf/dnctl rules on macOS
func (d *Degrader) removeDarwinRules() error {
	// Flush all dnctl pipes
	exec.Command("dnctl", "-q", "flush").Run()

	// Disable pf dummynet
	exec.Command("pfctl", "-f", "/etc/pf.conf").Run()

	// Remove temp rule files
	exec.Command("sh", "-c", "rm -f /tmp/chaos_pf_rules_*.conf").Run()

	d.pipeNum = 1
	d.active = false
	log.Printf("[Network] Removed all pf/dnctl rules")
	return nil
}

// Returns whether degradation is currently active
func (d *Degrader) IsActive() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.active
}

// Checks if we have the necessary privileges
func (d *Degrader) CheckPrivileges() error {
	switch d.os {
	case "linux":
		if out, err := exec.Command("tc", "qdisc", "show").CombinedOutput(); err != nil {
			return fmt.Errorf("tc not available or insufficient privileges: %v, output: %s", err, out)
		}
	case "darwin":
		if out, err := exec.Command("dnctl", "list").CombinedOutput(); err != nil {
			return fmt.Errorf("dnctl not available or insufficient privileges: %v, output: %s", err, out)
		}
	}
	return nil
}
