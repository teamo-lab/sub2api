// Package memlimit 在启动时给 Go 运行时设软内存上限（GOMEMLIMIT）。
//
// 网关进程常和 PostgreSQL、Redis、另一个蓝绿槽位跑在同一台机器上，容器一般也没配内存上限。
// 不设 GOMEMLIMIT 时，GC 只按 GOGC 把堆放大到存活对象的两倍，大请求体一多就会直接撑到
// 内核 OOM。设了上限，堆接近上限时 GC 会提前回收；它是软上限，不会因为超过而崩溃。
package memlimit

import (
	"bufio"
	"bytes"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// Source 说明当前内存上限从哪来。
type Source string

const (
	SourceEnv      Source = "env"      // 用户设置了 GOMEMLIMIT，原样保留
	SourceCgroup   Source = "cgroup"   // 按容器内存上限 × 比例
	SourceHost     Source = "host"     // 按整机内存 × 比例
	SourceDisabled Source = "disabled" // 比例为 0，或读不到内存总量
)

var (
	cgroupV2MaxPath   = "/sys/fs/cgroup/memory.max"
	cgroupV1LimitPath = "/sys/fs/cgroup/memory/memory.limit_in_bytes"
	procMeminfoPath   = "/proc/meminfo"
	setMemoryLimit    = debug.SetMemoryLimit
)

// Configure 在没有设置 GOMEMLIMIT 时按 ratio 设内存上限，返回生效的上限和来源。
// 返回的上限为 math.MaxInt64 表示没有上限。
func Configure(ratio float64) (int64, Source) {
	if strings.TrimSpace(os.Getenv("GOMEMLIMIT")) != "" {
		return Current(), SourceEnv
	}
	if ratio <= 0 || ratio >= 1 {
		return Current(), SourceDisabled
	}
	total, source := cgroupLimitBytes(), SourceCgroup
	if total <= 0 {
		total, source = hostMemoryBytes(), SourceHost
	}
	if total <= 0 {
		return Current(), SourceDisabled
	}
	limit := int64(float64(total) * ratio)
	setMemoryLimit(limit)
	return limit, source
}

// Current 返回 Go 运行时当前的内存上限，math.MaxInt64 表示没有上限。
func Current() int64 {
	return setMemoryLimit(-1)
}

// IsLimited 判断 limit 是否是一个真实的上限。
func IsLimited(limit int64) bool {
	return limit > 0 && limit < math.MaxInt64
}

func cgroupLimitBytes() int64 {
	for _, path := range []string{cgroupV2MaxPath, cgroupV1LimitPath} {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(raw))
		if value == "" || value == "max" {
			return 0
		}
		n, err := strconv.ParseInt(value, 10, 64)
		// cgroup v1 没有上限时是一个接近 int64 上限的数（按页对齐的 2^63-1）。
		if err != nil || n <= 0 || n >= 1<<62 {
			return 0
		}
		return n
	}
	return 0
}

func hostMemoryBytes() int64 {
	raw, err := os.ReadFile(procMeminfoPath)
	if err != nil {
		return 0
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb <= 0 {
			return 0
		}
		return kb * 1024
	}
	return 0
}
