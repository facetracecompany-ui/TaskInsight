package main

import (
	"encoding/csv"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type ProcInfo struct {
	PID     int32   `json:"pid"`
	PPID    int32   `json:"ppid"`
	Name    string  `json:"name"`
	User    string  `json:"user"`
	CPU     float64 `json:"cpu"`   // % of the whole machine
	RSS     uint64  `json:"rss"`   // working set, bytes
	Private uint64  `json:"vms"`   // committed, bytes
	ReadBps float64 `json:"rbps"`  // disk read, bytes/s
	WritBps float64 `json:"wbps"`  // disk write, bytes/s
	Threads int32   `json:"thr"`
	Conns   int     `json:"conns"` // open TCP/UDP endpoints
	GPU     float64 `json:"gpu"`   // % of the GPU (any engine)
	GPUMem  uint64  `json:"gpumem"`
	Started int64   `json:"started"` // unix ms
}

type GPUInfo struct {
	Name    string  `json:"name"`
	Util    float64 `json:"util"`
	MemUsed float64 `json:"memUsed"` // MiB
	MemTot  float64 `json:"memTotal"`
	Temp    float64 `json:"temp"`
	Power   float64 `json:"power"`
	Clock   float64 `json:"clock"`
}

type DiskInfo struct {
	Mount string  `json:"mount"`
	Total uint64  `json:"total"`
	Used  uint64  `json:"used"`
	Pct   float64 `json:"pct"`
}

type Snapshot struct {
	Time     int64      `json:"time"`
	Host     string     `json:"host"`
	Uptime   uint64     `json:"uptime"`
	Cores    int        `json:"cores"`
	CPU      float64    `json:"cpu"`
	PerCore  []float64  `json:"perCore"`
	MemTotal uint64     `json:"memTotal"`
	MemUsed  uint64     `json:"memUsed"`
	MemAvail uint64     `json:"memAvail"`
	SwapUsed uint64     `json:"swapUsed"`
	SwapTot  uint64     `json:"swapTotal"`
	DiskR    float64    `json:"diskR"`
	DiskW    float64    `json:"diskW"`
	NetUp    float64    `json:"netUp"`
	NetDown  float64    `json:"netDown"`
	GPU      *GPUInfo   `json:"gpu"`
	Disks    []DiskInfo `json:"disks"`
	Procs    []ProcInfo `json:"procs"`
	Threads  int        `json:"threads"`
}

var snap Snapshot

type ioPrev struct {
	r, w uint64
	t    time.Time
}

var (
	gpuUtil = map[int32]float64{}
	gpuMem  = map[int32]uint64{}
	gpuSys  *GPUInfo
)

func sampleLoop() {
	procs := map[int32]*process.Process{}
	prevIO := map[int32]ioPrev{}
	type staticInfo struct {
		name, user string
		ppid       int32
		started    int64
	}
	static := map[int32]staticInfo{}
	cores := runtime.NumCPU()
	hostName, _ := host.Info()

	var pDisk map[string]disk.IOCountersStat
	var pNet []net.IOCountersStat
	var pT = time.Now()
	pDisk, _ = disk.IOCounters()
	pNet, _ = net.IOCounters(false)
	_, _ = cpu.Percent(0, true)

	diskTick := 0
	var disks []DiskInfo
	lastRun := time.Time{}

	for {
		time.Sleep(2 * time.Second)
		// In the tray nobody watches live: sample only as often as the journal needs.
		if !windowOpen() && time.Since(lastRun) < logEvery-300*time.Millisecond {
			continue
		}
		lastRun = time.Now()
		now := lastRun
		dt := now.Sub(pT).Seconds()
		pT = now

		s := Snapshot{Time: now.UnixMilli(), Cores: cores}
		if hostName != nil {
			s.Host, s.Uptime = hostName.Hostname, hostName.Uptime
		}
		if pc, err := cpu.Percent(0, true); err == nil {
			s.PerCore = pc
			var sum float64
			for _, v := range pc {
				sum += v
			}
			if len(pc) > 0 {
				s.CPU = sum / float64(len(pc))
			}
		}
		if vm, err := mem.VirtualMemory(); err == nil {
			s.MemTotal, s.MemUsed, s.MemAvail = vm.Total, vm.Used, vm.Available
		}
		if sw, err := mem.SwapMemory(); err == nil {
			s.SwapTot, s.SwapUsed = sw.Total, sw.Used
		}
		if cur, err := disk.IOCounters(); err == nil {
			var r, w uint64
			for k, v := range cur {
				if p, ok := pDisk[k]; ok {
					r += v.ReadBytes - p.ReadBytes
					w += v.WriteBytes - p.WriteBytes
				}
			}
			s.DiskR, s.DiskW = float64(r)/dt, float64(w)/dt
			pDisk = cur
		}
		if cur, err := net.IOCounters(false); err == nil && len(cur) > 0 && len(pNet) > 0 {
			s.NetDown = float64(cur[0].BytesRecv-pNet[0].BytesRecv) / dt
			s.NetUp = float64(cur[0].BytesSent-pNet[0].BytesSent) / dt
			pNet = cur
		}
		if diskTick%10 == 0 {
			disks = disks[:0]
			if parts, err := disk.Partitions(false); err == nil {
				for _, p := range parts {
					if u, err := disk.Usage(p.Mountpoint); err == nil && u.Total > 0 {
						disks = append(disks, DiskInfo{Mount: p.Mountpoint, Total: u.Total, Used: u.Used, Pct: u.UsedPercent})
					}
				}
			}
		}
		diskTick++
		s.Disks = append([]DiskInfo(nil), disks...)

		conns := map[int32]int{}
		if cs, err := net.Connections("inet"); err == nil {
			for _, c := range cs {
				if c.Pid > 0 {
					conns[c.Pid]++
				}
			}
		}

		list, _ := process.Processes()
		alive := map[int32]bool{}
		for _, p := range list {
			pid := p.Pid
			alive[pid] = true
			cached, ok := procs[pid]
			if !ok {
				procs[pid] = p
				cached = p
			}
			pi := ProcInfo{PID: pid, Conns: conns[pid]}
			st, known := static[pid]
			if !known {
				st.name, _ = cached.Name()
				if pid == 0 {
					st.name = "System Idle Process"
				}
				st.ppid, _ = cached.Ppid()
				st.user, _ = cached.Username()
				st.started, _ = cached.CreateTime()
				static[pid] = st
			}
			pi.Name, pi.PPID, pi.User, pi.Started = st.name, st.ppid, st.user, st.started
			if pct, err := cached.Percent(0); err == nil && pid != 0 {
				pi.CPU = pct / float64(cores)
			}
			if mi, err := cached.MemoryInfo(); err == nil && mi != nil {
				pi.RSS, pi.Private = mi.RSS, mi.VMS
			}
			if n, err := cached.NumThreads(); err == nil {
				pi.Threads = n
				s.Threads += int(n)
			}
			if io, err := cached.IOCounters(); err == nil && io != nil {
				if pv, ok := prevIO[pid]; ok {
					d := now.Sub(pv.t).Seconds()
					if d > 0 {
						pi.ReadBps = float64(io.ReadBytes-pv.r) / d
						pi.WritBps = float64(io.WriteBytes-pv.w) / d
					}
				}
				prevIO[pid] = ioPrev{io.ReadBytes, io.WriteBytes, now}
			}
			pi.GPU, pi.GPUMem = gpuUtil[pid], gpuMem[pid]
			s.Procs = append(s.Procs, pi)
		}
		for pid := range procs {
			if !alive[pid] {
				delete(procs, pid)
				delete(prevIO, pid)
				delete(static, pid)
			}
		}
		sort.Slice(s.Procs, func(i, j int) bool { return s.Procs[i].CPU > s.Procs[j].CPU })
		s.GPU = gpuSys

		mu.Lock()
		snap = s
		mu.Unlock()

		recordSample(s, dt, func(pid int32) (string, string) {
			p := procs[pid]
			if p == nil {
				return "", ""
			}
			exe, _ := p.Exe()
			cmd, _ := p.Cmdline()
			return exe, cmd
		})
	}
}

// hidden runs a console program without flashing a window.
func hidden(name string, args ...string) *exec.Cmd {
	c := exec.Command(name, args...)
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return c
}

func gpuLoop() {
	for {
		pollGPU()
		if windowOpen() {
			time.Sleep(2 * time.Second)
		} else {
			time.Sleep(logEvery)
		}
	}
}

func pollGPU() {
	// Whole-card numbers from nvidia-smi (when there is an NVIDIA card).
	if out, err := hidden("nvidia-smi",
		"--query-gpu=name,utilization.gpu,memory.used,memory.total,temperature.gpu,power.draw,clocks.gr",
		"--format=csv,noheader,nounits").Output(); err == nil {
		f := strings.Split(strings.TrimSpace(strings.Split(string(out), "\n")[0]), ",")
		if len(f) >= 7 {
			num := func(i int) float64 {
				v, _ := strconv.ParseFloat(strings.TrimSpace(f[i]), 64)
				return v
			}
			gpuSys = &GPUInfo{Name: strings.TrimSpace(f[0]), Util: num(1), MemUsed: num(2), MemTot: num(3), Temp: num(4), Power: num(5), Clock: num(6)}
		}
	}

	// Per-process load from the Windows GPU performance counters.
	out, err := hidden("typeperf",
		`\GPU Engine(*)\Utilization Percentage`, `\GPU Process Memory(*)\Dedicated Usage`, "-sc", "1").Output()
	if err != nil {
		return
	}
	var lines []string
	for _, l := range strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n") {
		if strings.HasPrefix(l, `"`) {
			lines = append(lines, l)
		}
	}
	if len(lines) < 2 {
		return
	}
	rd := csv.NewReader(strings.NewReader(lines[0] + "\n" + lines[1]))
	rd.FieldsPerRecord = -1
	rec, err := rd.ReadAll()
	if err != nil || len(rec) < 2 {
		return
	}
	util := map[int32]float64{}
	gm := map[int32]uint64{}
	for i, name := range rec[0] {
		if i == 0 || i >= len(rec[1]) {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(rec[1][i]), 64)
		if err != nil || v == 0 {
			continue
		}
		var pid int
		if k := strings.Index(name, "pid_"); k >= 0 {
			_, _ = fmt.Sscanf(name[k+4:], "%d", &pid)
		}
		if pid == 0 {
			continue
		}
		if strings.Contains(name, "GPU Engine") {
			util[int32(pid)] += v
		} else {
			gm[int32(pid)] += uint64(v)
		}
	}
	for k, v := range util {
		if v > 100 {
			util[k] = 100
		}
	}
	gpuUtil, gpuMem = util, gm
}
