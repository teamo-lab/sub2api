package memlimit

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func withFakeSystem(t *testing.T, cgroupV2, cgroupV1, meminfo string) *int64 {
	t.Helper()
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return path
	}
	oldV2, oldV1, oldMeminfo, oldSet := cgroupV2MaxPath, cgroupV1LimitPath, procMeminfoPath, setMemoryLimit
	cgroupV2MaxPath = write("memory.max", cgroupV2)
	cgroupV1LimitPath = write("memory.limit_in_bytes", cgroupV1)
	procMeminfoPath = write("meminfo", meminfo)
	current := int64(math.MaxInt64)
	setMemoryLimit = func(limit int64) int64 {
		prev := current
		if limit >= 0 {
			current = limit
		}
		return prev
	}
	t.Cleanup(func() {
		cgroupV2MaxPath, cgroupV1LimitPath, procMeminfoPath, setMemoryLimit = oldV2, oldV1, oldMeminfo, oldSet
	})
	return &current
}

func TestConfigure_UsesCgroupLimitFirst(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	current := withFakeSystem(t, "1000000\n", "", "MemTotal:       16000000 kB\n")
	limit, source := Configure(0.5)
	if source != SourceCgroup || limit != 500000 || *current != 500000 {
		t.Fatalf("limit=%d source=%s current=%d", limit, source, *current)
	}
}

func TestConfigure_FallsBackToHostMemoryWhenCgroupUnlimited(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	current := withFakeSystem(t, "max\n", "", "MemFree: 1 kB\nMemTotal:       16000000 kB\n")
	limit, source := Configure(0.7)
	want := int64(float64(16000000*1024) * 0.7)
	if source != SourceHost || limit != want || *current != want {
		t.Fatalf("limit=%d source=%s current=%d want=%d", limit, source, *current, want)
	}
}

func TestConfigure_CgroupV1UnlimitedSentinelFallsBackToHost(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	withFakeSystem(t, "", "9223372036854771712\n", "MemTotal: 1000 kB\n")
	if _, source := Configure(0.5); source != SourceHost {
		t.Fatalf("source=%s, want host", source)
	}
}

func TestConfigure_RespectsExplicitGOMEMLIMIT(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "8GiB")
	current := withFakeSystem(t, "1000000\n", "", "MemTotal: 1000 kB\n")
	if _, source := Configure(0.5); source != SourceEnv || *current != math.MaxInt64 {
		t.Fatalf("source=%s current=%d: explicit GOMEMLIMIT must not be overridden", source, *current)
	}
}

func TestConfigure_ZeroRatioOrUnknownMemoryDisables(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "")
	current := withFakeSystem(t, "1000000\n", "", "")
	if _, source := Configure(0); source != SourceDisabled || *current != math.MaxInt64 {
		t.Fatalf("ratio 0: source=%s current=%d", source, *current)
	}
	withFakeSystem(t, "max\n", "", "")
	if limit, source := Configure(0.7); source != SourceDisabled || IsLimited(limit) {
		t.Fatalf("no memory info: limit=%d source=%s", limit, source)
	}
}
