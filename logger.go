package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The journal is one JSON-lines file per day in %APPDATA%\TaskInsight\logs.
// "proc" lines describe a process once (path, command line, user); "s" lines
// are samples that mention only the processes that were actually busy.

const (
	logEvery  = 5 * time.Second
	logKeep   = 14 // days
	spikeGap  = 60 * 1000
	maxPoints = 240
)

type logProc struct {
	Key   string  `json:"k"`
	CPU   float64 `json:"c,omitempty"`
	RSS   uint64  `json:"m,omitempty"`
	GPU   float64 `json:"g,omitempty"`
	Read  float64 `json:"r,omitempty"`
	Write float64 `json:"w,omitempty"`
	Conns int     `json:"n,omitempty"`
}

type logLine struct {
	K    string  `json:"k"` // "proc" | "s"
	Key  string  `json:"key,omitempty"`
	PID  int32   `json:"pid,omitempty"`
	PPID int32   `json:"ppid,omitempty"`
	Name string  `json:"name,omitempty"`
	Exe  string  `json:"exe,omitempty"`
	Cmd  string  `json:"cmd,omitempty"`
	User string  `json:"user,omitempty"`
	Born int64   `json:"born,omitempty"`
	T    int64   `json:"t,omitempty"`
	DT   float64 `json:"dt,omitempty"`
	CPU  float64 `json:"cpu,omitempty"`
	Mem  float64 `json:"mem,omitempty"` // % used
	GPU  float64 `json:"gpu,omitempty"`
	DR   float64 `json:"dr,omitempty"`
	DW   float64 `json:"dw,omitempty"`
	NU   float64 `json:"nu,omitempty"`
	ND   float64 `json:"nd,omitempty"`
	P    []logProc `json:"p,omitempty"`
}

var (
	logMu     sync.Mutex
	logFile   *os.File
	logDay    string
	logged    = map[string]bool{}
	lastLogAt time.Time
)

func logDir() string {
	dir, _ := os.UserConfigDir()
	return filepath.Join(dir, "TaskInsight", "logs")
}

func procKey(pid int32, born int64) string {
	return itoa(pid) + "-" + strconv.FormatInt(born, 10)
}

// openLog switches to today's file; call with logMu held.
func openLog(now time.Time) {
	day := now.Format("2006-01-02")
	if logFile != nil && day == logDay {
		return
	}
	if logFile != nil {
		logFile.Close()
	}
	_ = os.MkdirAll(logDir(), 0o755)
	f, err := os.OpenFile(filepath.Join(logDir(), day+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		logFile = nil
		return
	}
	logFile, logDay = f, day
	clear(logged) // every file must be readable on its own
	pruneLogs(now)
}

func pruneLogs(now time.Time) {
	old := now.AddDate(0, 0, -logKeep).Format("2006-01-02")
	ents, _ := os.ReadDir(logDir())
	for _, e := range ents {
		if n := strings.TrimSuffix(e.Name(), ".jsonl"); n < old && strings.HasSuffix(e.Name(), ".jsonl") {
			_ = os.Remove(filepath.Join(logDir(), e.Name()))
		}
	}
}

func writeLine(l logLine) {
	b, _ := json.Marshal(l)
	_, _ = logFile.Write(append(b, '\n'))
}

// recordSample appends one sample if it is time to. info supplies the path and
// command line of a process the first time it shows up in the journal.
func recordSample(s Snapshot, dt float64, info func(pid int32) (exe, cmd string)) {
	now := time.UnixMilli(s.Time)
	logMu.Lock()
	defer logMu.Unlock()
	if now.Sub(lastLogAt) < logEvery-200*time.Millisecond {
		return
	}
	lastLogAt = now
	openLog(now)
	if logFile == nil {
		return
	}

	rec := logLine{K: "s", T: s.Time, DT: dt, CPU: round1(s.CPU), DR: s.DiskR, DW: s.DiskW, NU: s.NetUp, ND: s.NetDown}
	if s.MemTotal > 0 {
		rec.Mem = round1(float64(s.MemUsed) / float64(s.MemTotal) * 100)
	}
	if s.GPU != nil {
		rec.GPU = s.GPU.Util
	}

	// the busy ones, plus the three biggest in memory
	byMem := append([]ProcInfo(nil), s.Procs...)
	sort.Slice(byMem, func(i, j int) bool { return byMem[i].RSS > byMem[j].RSS })
	top := map[int32]bool{}
	for i := 0; i < 3 && i < len(byMem); i++ {
		top[byMem[i].PID] = true
	}
	for _, p := range s.Procs {
		if p.PID == 0 {
			continue
		}
		if !(p.CPU >= 1 || p.GPU >= 2 || p.ReadBps+p.WritBps >= 100*1024 || p.Conns >= 20 || top[p.PID]) {
			continue
		}
		key := procKey(p.PID, p.Started)
		if !logged[key] {
			logged[key] = true
			exe, cmd := info(p.PID)
			writeLine(logLine{K: "proc", Key: key, PID: p.PID, PPID: p.PPID, Name: p.Name, Exe: exe, Cmd: cmd, User: p.User, Born: p.Started})
		}
		rec.P = append(rec.P, logProc{Key: key, CPU: round1(p.CPU), RSS: p.RSS, GPU: round1(p.GPU), Read: round0(p.ReadBps), Write: round0(p.WritBps), Conns: p.Conns})
	}
	writeLine(rec)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }
func round0(v float64) float64 { return float64(int(v + 0.5)) }

// ---------------------------------------------------------------- reading

type ProcAgg struct {
	Key      string  `json:"key"`
	PID      int32   `json:"pid"`
	Name     string  `json:"name"`
	Exe      string  `json:"exe"`
	Cmd      string  `json:"cmd"`
	User     string  `json:"user"`
	Samples  int     `json:"samples"`
	CPUAvg   float64 `json:"cpuAvg"`
	CPUPeak  float64 `json:"cpuPeak"`
	CPUSum   float64 `json:"cpuSum"` // %·sec of the whole machine
	GPUAvg   float64 `json:"gpuAvg"`
	GPUPeak  float64 `json:"gpuPeak"`
	RSSPeak  uint64  `json:"rssPeak"`
	RSSAvg   uint64  `json:"rssAvg"`
	ReadSum  float64 `json:"readSum"` // bytes
	WriteSum float64 `json:"writeSum"`
	IOPeak   float64 `json:"ioPeak"` // bytes/s
	Conns    int     `json:"connsPeak"`
	First    int64   `json:"first"`
	Last     int64   `json:"last"`
	Seconds  float64 `json:"seconds"`

	cpuS, gpuS, rssS float64
}

type Point struct {
	T    int64               `json:"t"`
	CPU  float64             `json:"cpu"`
	Mem  float64             `json:"mem"`
	GPU  float64             `json:"gpu"`
	Disk float64             `json:"disk"`
	Net  float64             `json:"net"`
	Top  map[string][]string `json:"top"` // metric -> culprits at the busiest moment
}

type Spike struct {
	T      int64    `json:"t"`
	Value  float64  `json:"value"`
	Who    []string `json:"who"`
	Metric string   `json:"metric"`
}

type LogReport struct {
	From, To  int64             `json:"-"`
	Samples   int               `json:"samples"`
	Start     int64             `json:"start"`
	End       int64             `json:"end"`
	Points    []Point           `json:"points"`
	Procs     []*ProcAgg        `json:"procs"`
	Spikes    map[string][]Spike `json:"spikes"`
	Days      []string          `json:"days"`
	LogFolder string            `json:"logFolder"`
}

func metricVal(m string, l *logLine) float64 {
	switch m {
	case "cpu":
		return l.CPU
	case "mem":
		return l.Mem
	case "gpu":
		return l.GPU
	case "disk":
		return l.DR + l.DW
	case "net":
		return l.NU + l.ND
	}
	return 0
}

func procMetric(m string, p logProc) float64 {
	switch m {
	case "cpu":
		return p.CPU
	case "mem":
		return float64(p.RSS)
	case "gpu":
		return p.GPU
	case "disk":
		return p.Read + p.Write
	case "net":
		return float64(p.Conns)
	}
	return 0
}

var metricNames = []string{"cpu", "mem", "gpu", "disk", "net"}

func buildReport(from, to int64) LogReport {
	rep := LogReport{From: from, To: to, Spikes: map[string][]Spike{}, LogFolder: logDir(), Points: []Point{}, Procs: []*ProcAgg{}}
	info := map[string]*ProcAgg{}
	var samples []*logLine

	ents, _ := os.ReadDir(logDir())
	fromDay, toDay := time.UnixMilli(from).Format("2006-01-02"), time.UnixMilli(to).Format("2006-01-02")
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		day := strings.TrimSuffix(e.Name(), ".jsonl")
		rep.Days = append(rep.Days, day)
		if day < fromDay || day > toDay {
			continue
		}
		f, err := os.Open(filepath.Join(logDir(), e.Name()))
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var l logLine
			if json.Unmarshal(sc.Bytes(), &l) != nil {
				continue
			}
			switch l.K {
			case "proc":
				if _, ok := info[l.Key]; !ok {
					info[l.Key] = &ProcAgg{Key: l.Key, PID: l.PID, Name: l.Name, Exe: l.Exe, Cmd: l.Cmd, User: l.User}
				}
			case "s":
				if l.T >= from && l.T <= to {
					ll := l
					samples = append(samples, &ll)
				}
			}
		}
		f.Close()
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].T < samples[j].T })
	rep.Samples = len(samples)
	if len(samples) == 0 {
		return rep
	}
	rep.Start, rep.End = samples[0].T, samples[len(samples)-1].T

	nameOf := func(key string) string {
		if a := info[key]; a != nil {
			return a.Name
		}
		return key
	}
	culprits := func(m string, l *logLine) []string {
		ps := append([]logProc(nil), l.P...)
		sort.Slice(ps, func(i, j int) bool { return procMetric(m, ps[i]) > procMetric(m, ps[j]) })
		var out []string
		for _, p := range ps {
			v := procMetric(m, p)
			if v <= 0 || len(out) == 3 {
				break
			}
			out = append(out, nameOf(p.Key)+" "+fmtMetric(m, v))
		}
		return out
	}

	// per-process totals
	for _, l := range samples {
		for _, p := range l.P {
			a := info[p.Key]
			if a == nil {
				a = &ProcAgg{Key: p.Key, Name: p.Key}
				info[p.Key] = a
			}
			a.Samples++
			a.cpuS += p.CPU
			a.gpuS += p.GPU
			a.rssS += float64(p.RSS)
			a.CPUSum += p.CPU * l.DT
			a.Seconds += l.DT
			a.ReadSum += p.Read * l.DT
			a.WriteSum += p.Write * l.DT
			a.CPUPeak = maxf(a.CPUPeak, p.CPU)
			a.GPUPeak = maxf(a.GPUPeak, p.GPU)
			a.IOPeak = maxf(a.IOPeak, p.Read+p.Write)
			if p.RSS > a.RSSPeak {
				a.RSSPeak = p.RSS
			}
			if p.Conns > a.Conns {
				a.Conns = p.Conns
			}
			if a.First == 0 || l.T < a.First {
				a.First = l.T
			}
			if l.T > a.Last {
				a.Last = l.T
			}
		}
	}
	for _, a := range info {
		if a.Samples == 0 {
			continue
		}
		n := float64(a.Samples)
		a.CPUAvg, a.GPUAvg, a.RSSAvg = a.cpuS/n, a.gpuS/n, uint64(a.rssS/n)
		rep.Procs = append(rep.Procs, a)
	}
	sort.Slice(rep.Procs, func(i, j int) bool { return rep.Procs[i].CPUSum > rep.Procs[j].CPUSum })

	// timeline
	nb := maxPoints
	if len(samples) < nb {
		nb = len(samples)
	}
	span := float64(rep.End-rep.Start) + 1
	type bucket struct {
		n    int
		sum  [5]float64
		best [5]*logLine
		bv   [5]float64
	}
	bs := make([]bucket, nb)
	for _, l := range samples {
		i := int(float64(l.T-rep.Start) / span * float64(nb))
		b := &bs[i]
		b.n++
		for k, m := range metricNames {
			v := metricVal(m, l)
			b.sum[k] += v
			if b.best[k] == nil || v > b.bv[k] {
				b.best[k], b.bv[k] = l, v
			}
		}
	}
	for i, b := range bs {
		if b.n == 0 {
			continue
		}
		p := Point{T: rep.Start + int64(float64(i)/float64(nb)*span), Top: map[string][]string{}}
		n := float64(b.n)
		p.CPU, p.Mem, p.GPU, p.Disk, p.Net = b.sum[0]/n, b.sum[1]/n, b.sum[2]/n, b.sum[3]/n, b.sum[4]/n
		for k, m := range metricNames {
			if b.best[k] != nil {
				p.Top[m] = culprits(m, b.best[k])
			}
		}
		rep.Points = append(rep.Points, p)
	}

	// the worst moments of each metric
	for _, m := range metricNames {
		idx := make([]int, len(samples))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(a, b int) bool { return metricVal(m, samples[idx[a]]) > metricVal(m, samples[idx[b]]) })
		var out []Spike
		for _, i := range idx {
			l := samples[i]
			v := metricVal(m, l)
			if v <= 0 || len(out) >= 15 {
				break
			}
			near := false
			for _, s := range out {
				if abs64(s.T-l.T) < spikeGap {
					near = true
					break
				}
			}
			if near {
				continue
			}
			out = append(out, Spike{T: l.T, Value: v, Who: culprits(m, l), Metric: m})
		}
		rep.Spikes[m] = out
	}
	return rep
}

func fmtMetric(m string, v float64) string {
	switch m {
	case "cpu", "gpu":
		return ftoa(v) + "%"
	case "mem":
		return ftoa(v/1048576) + " МБ"
	case "disk":
		return ftoa(v/1048576) + " МБ/с"
	}
	return ftoa(v) + " подкл."
}

func ftoa(v float64) string {
	b, _ := json.Marshal(float64(int(v*10+0.5)) / 10)
	return string(b)
}

func maxf(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
