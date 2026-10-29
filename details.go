package main

import (
	"errors"
	"sort"

	"github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

type ConnInfo struct {
	Proto  string `json:"proto"`
	Local  string `json:"local"`
	Remote string `json:"remote"`
	Status string `json:"status"`
}

type ProcDetails struct {
	PID      int32      `json:"pid"`
	Exe      string     `json:"exe"`
	Cmdline  string     `json:"cmdline"`
	Cwd      string     `json:"cwd"`
	Parent   string     `json:"parent"`
	Children []string   `json:"children"`
	Priority int32      `json:"priority"`
	Status   []string   `json:"status"`
	Conns    []ConnInfo `json:"conns"`
	Files    []string   `json:"files"`
	Error    string     `json:"error"`
}

func procDetails(pid int32) ProcDetails {
	d := ProcDetails{PID: pid, Children: []string{}, Conns: []ConnInfo{}, Files: []string{}}
	p, err := process.NewProcess(pid)
	if err != nil {
		d.Error = "Процесс уже завершён"
		return d
	}
	d.Exe, _ = p.Exe()
	d.Cmdline, _ = p.Cmdline()
	d.Cwd, _ = p.Cwd()
	d.Status, _ = p.Status()
	d.Priority, _ = p.Nice()
	if pp, err := p.Parent(); err == nil {
		n, _ := pp.Name()
		d.Parent = n
	}
	if ch, err := p.Children(); err == nil {
		for _, c := range ch {
			n, _ := c.Name()
			d.Children = append(d.Children, n+" ("+itoa(c.Pid)+")")
		}
	}
	if cs, err := net.ConnectionsPid("inet", pid); err == nil {
		for _, c := range cs {
			proto := "TCP"
			if c.Type == 2 {
				proto = "UDP"
			}
			ci := ConnInfo{Proto: proto, Local: hostPort(c.Laddr), Status: c.Status}
			if c.Raddr.Port != 0 {
				ci.Remote = hostPort(c.Raddr)
			}
			d.Conns = append(d.Conns, ci)
		}
		sort.Slice(d.Conns, func(i, j int) bool { return d.Conns[i].Remote > d.Conns[j].Remote })
	}
	if fs, err := p.OpenFiles(); err == nil {
		for _, f := range fs {
			d.Files = append(d.Files, f.Path)
		}
		if len(d.Files) > 200 {
			d.Files = d.Files[:200]
		}
	}
	return d
}

func hostPort(a net.Addr) string { return a.IP + ":" + itoa(int32(a.Port)) }

func itoa(n int32) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func doAction(what string, pid int32) error {
	if pid <= 4 {
		return errors.New("системный процесс трогать нельзя")
	}
	p, err := process.NewProcess(pid)
	if err != nil {
		return err
	}
	switch what {
	case "kill":
		return p.Kill()
	case "tree":
		ch, _ := p.Children()
		for _, c := range ch {
			_ = doAction("tree", c.Pid)
		}
		return p.Kill()
	case "suspend":
		return p.Suspend()
	case "resume":
		return p.Resume()
	}
	return errors.New("неизвестное действие")
}
