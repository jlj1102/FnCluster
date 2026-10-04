package collector

import "testing"

func TestProcParsers(t *testing.T) {
	total, idle, err := cpuCounters("cpu  10 2 3 4 1 0 0 0\n")
	if err != nil || total != 20 || idle != 5 {
		t.Fatalf("cpu: %d %d %v", total, idle, err)
	}
	memoryTotal, used, err := memory("MemTotal: 100 kB\nMemAvailable: 40 kB\n")
	if err != nil || memoryTotal != 102400 || used != 61440 {
		t.Fatalf("memory: %d %d %v", memoryTotal, used, err)
	}
	load, err := loadAverage("1.2 2.3 3.4 1/100 123\n")
	if err != nil || load != [3]float64{1.2, 2.3, 3.4} {
		t.Fatalf("load: %v %v", load, err)
	}
}

func TestDiskTotals(t *testing.T) {
	input := "   8       0 sda 100 0 2048 0 50 0 1024 0 0 0 0\n" +
		"   8       1 sda1 10 0 1000 0 5 0 500 0 0 0 0\n" +
		" 259       0 nvme0n1 10 0 4096 0 20 0 8192 0 0 0 0\n" +
		"   7       0 loop0 1 0 9999 0 1 0 9999 0 0 0 0\n"
	read, write := diskTotals(input)
	if read != uint64(2048+4096)*512 || write != uint64(1024+8192)*512 {
		t.Fatalf("disk totals = %d %d", read, write)
	}
}

func TestPerSecondHandlesReset(t *testing.T) {
	if got := perSecond(100, 200, 2); got != 50 {
		t.Fatalf("rate = %v", got)
	}
	if got := perSecond(200, 100, 2); got != 0 {
		t.Fatalf("counter reset rate = %v", got)
	}
}

func TestFnOSVolumes(t *testing.T) {
	mounts := fnOSVolumes("/dev/sda / ext4 rw 0 0\n/dev/sdb /vol1 btrfs rw 0 0\n/dev/sdb /vol1/app btrfs rw 0 0\n/dev/sdc /vol2 ext4 rw 0 0\n/dev/sdc /vol2 ext4 rw 0 0\n")
	if len(mounts) != 2 || mounts[0] != "/vol1" || mounts[1] != "/vol2" {
		t.Fatalf("unexpected volume mounts: %v", mounts)
	}
}
