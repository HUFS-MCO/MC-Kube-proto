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
	annUsageKey   = "mckube.sdv.com/cpu-usage"
	annDurKey     = "mckube.sdv.com/cpu-over90-duration-s"
	annCpuBusyKey = "mckube.sdv.com/isCpuBusy"
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

func annotate(node string, usage int, over90time int64, isCpuBusy *bool) error {
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
	
	// isCpuBusy 값이 제공된 경우에만 추가
	if isCpuBusy != nil {
		args = append(args, fmt.Sprintf("%s=%t", annCpuBusyKey, *isCpuBusy))
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
	
	if isCpuBusy != nil {
		log.Printf("annotate success: node=%s usage=%d%% over90time=%ds isCpuBusy=%t", node, usage, over90time, *isCpuBusy)
	} else {
		log.Printf("annotate success: node=%s usage=%d%% over90time=%ds", node, usage, over90time)
	}
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
	var dropBelowTime time.Time // 90% 미만으로 떨어진 시점
	var waitingForBusyFalse bool // isCpuBusy=false 전송 대기 상태

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
			// 90% 이상에서 다시 돌아온 경우 대기 상태 리셋
			if waitingForBusyFalse {
				waitingForBusyFalse = false
				log.Printf("CPU went back above 90%%, canceling isCpuBusy=false timer")
			}
		} else {
			// 90% 미만으로 떨어진 최초 시점 기록
			if over90time > 0 && dropBelowTime.IsZero() {
				dropBelowTime = time.Now()
				log.Printf("CPU dropped below 90%% for the first time, starting 5s timer")
			}
			over90time = 0
		}

		log.Printf("publishing cpu usage: node=%s usage=%d%% over90time=%ds", node, u, over90time)

		// 이벤트 기반: 의미있는 변화가 있을 때만 annotation 갱신
		shouldUpdate := false
		var isCpuBusy *bool
		
		if u > 90 {
			// CPU 90% 이상: 사용률 변화가 있거나 5초마다 갱신
			if (u != lastAnnUsage) || time.Since(lastAnnTime) > 5*time.Second {
				shouldUpdate = true
				busyVal := true
				isCpuBusy = &busyVal
				// 90% 이상에서 다시 돌아온 경우 대기 상태 리셋
				if waitingForBusyFalse {
					waitingForBusyFalse = false
					log.Printf("CPU went back above 90%%, canceling isCpuBusy=false timer")
				}
			}
		} else {
			// CPU 90% 미만: 상태 변화가 있을 때만 갱신
			if lastAnnUsage > 90 {
				// 최초로 90% 미만으로 떨어진 경우
				log.Printf("CPU dropped below 90%%, sending reset annotation: node=%s usage=%d%%", node, u)
				shouldUpdate = true
				waitingForBusyFalse = true
			} else if waitingForBusyFalse && !dropBelowTime.IsZero() && time.Since(dropBelowTime) >= 5*time.Second {
				// 5초 후 isCpuBusy=false 전송
				log.Printf("5 seconds passed since CPU dropped below 90%%, sending isCpuBusy=false")
				shouldUpdate = true
				busyVal := false
				isCpuBusy = &busyVal
				waitingForBusyFalse = false
				dropBelowTime = time.Time{} // 시간 리셋
			}
		}
		
		// annotation 업데이트 (필요한 경우에만)
		if shouldUpdate {
			if err := annotate(node, u, over90time, isCpuBusy); err == nil {
				lastAnnUsage = u
				lastAnnTime = time.Now()
			}
		}
	}
}
