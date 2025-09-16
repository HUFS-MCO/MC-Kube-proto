package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	annUsageKey = "mckube.sdv.com/cpu-usage"
	annDurKey   = "mckube.sdv.com/cpu-over90-duration-s"
)

type cpuSample struct{ idle, total uint64 }

func readProcStat() (cpuSample, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return cpuSample{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return cpuSample{}, errors.New("empty /proc/stat")
	}
	fields := strings.Fields(sc.Text())
	if len(fields) == 0 || fields[0] != "cpu" {
		return cpuSample{}, errors.New("unexpected /proc/stat format")
	}
	var nums []uint64
	for _, s := range fields[1:] {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			return cpuSample{}, err
		}
		nums = append(nums, v)
	}
	var user, nice, system, idle, iowait, irq, softirq, steal uint64
	if len(nums) > 0 {
		user = nums[0]
	}
	if len(nums) > 1 {
		nice = nums[1]
	}
	if len(nums) > 2 {
		system = nums[2]
	}
	if len(nums) > 3 {
		idle = nums[3]
	}
	if len(nums) > 4 {
		iowait = nums[4]
	}
	if len(nums) > 5 {
		irq = nums[5]
	}
	if len(nums) > 6 {
		softirq = nums[6]
	}
	if len(nums) > 7 {
		steal = nums[7]
	}
	idleAll := idle + iowait
	nonIdle := user + nice + system + irq + softirq + steal
	return cpuSample{idle: idleAll, total: idleAll + nonIdle}, nil
}

func computeUsage(prev, cur cpuSample) int {
	if cur.total <= prev.total {
		return 0
	}
	idleDelta := float64(cur.idle - prev.idle)
	totalDelta := float64(cur.total - prev.total)
	usage := (1.0 - idleDelta/totalDelta) * 100.0
	if usage < 0 {
		usage = 0
	}
	if usage > 100 {
		usage = 100
	}
	return int(math.Round(usage))
}

func getNodeName() string {
	if v := strings.TrimSpace(os.Getenv("NODE_NAME")); v != "" {
		return strings.ToLower(v)
	}
	if out, err := exec.Command("hostname", "-s").Output(); err == nil && len(out) > 0 {
		return strings.ToLower(strings.TrimSpace(string(out)))
	}
	out2, _ := exec.Command("hostname").Output()
	return strings.ToLower(strings.TrimSpace(string(out2)))
}

func annotate(node string, usage int, over90time int64) error {
	kubectl := strings.TrimSpace(os.Getenv("KUBECTL"))
	if kubectl == "" {
		kubectl = "/usr/bin/kubectl"
	}
	node = strings.ToLower(node) // 안전 장치

	args := []string{
		"annotate", "node", node,
		fmt.Sprintf("%s=%d", annUsageKey, usage),
		fmt.Sprintf("%s=%d", annDurKey, over90time),
		"--overwrite",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, kubectl, args...)
	cmd.Env = os.Environ() // KUBECONFIG 등 전달
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		log.Printf("kubectl annotate failed: %v, stderr=%s", err, strings.TrimSpace(stderr.String()))
		return err
	}
	log.Printf("annotate success: node=%s usage=%d%% over90time=%ds", node, usage, over90time)
	return nil
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	interval := time.Second // 🔒 1초 고정
	node := getNodeName()
	if node == "" {
		log.Fatal("Cannot determine node name")
	}
	log.Printf("mckube-cpu-agent starting: node=%s interval=%s", node, interval)

	prev, err := readProcStat()
	if err != nil {
		log.Fatalf("read /proc/stat: %v", err)
	}
	time.Sleep(interval)

	var over90time int64
	var lastAnnUsage = -1
	var lastAnnTime time.Time

	t := time.NewTicker(interval)
	defer t.Stop()
	for range t.C {
		cur, err := readProcStat()
		if err != nil {
			log.Printf("read /proc/stat error: %v", err)
			continue
		}
		u := computeUsage(prev, cur)
		prev = cur

		if u > 90 {
			over90time += int64(interval / time.Second)
		} else {
			over90time = 0
		}

		log.Printf("publishing cpu usage: node=%s usage=%d%% over90time=%ds", node, u, over90time)

		if (u != lastAnnUsage) || time.Since(lastAnnTime) > 5*time.Second {
			if err := annotate(node, u, over90time); err == nil {
				lastAnnUsage = u
				lastAnnTime = time.Now()
			}
		}
	}
}
