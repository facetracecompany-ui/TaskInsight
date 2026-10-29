package main

import (
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/shirou/gopsutil/v4/process"
	"golang.org/x/sys/windows"
)

type DiskProc struct {
	PID   int32    `json:"pid"`
	Name  string   `json:"name"`
	Exe   string   `json:"exe"`
	User  string   `json:"user"`
	OnExe bool     `json:"onExe"` // сам .exe лежит на этом диске
	Files []string `json:"files"` // открытые файлы на этом диске
}

type DiskReport struct {
	Drive string     `json:"drive"`
	Procs []DiskProc `json:"procs"`
	Total int        `json:"total"` // всего открытых файлов
	Slow  bool       `json:"slow"`  // сканирование прервано по времени
}

func onDrive(path, drive string) bool {
	return len(path) >= 2 && strings.EqualFold(path[:2], drive)
}

// handleTable reads the system handle table once and returns, per PID, the
// handle values that belong to files. Calling it per process (what gopsutil's
// OpenFiles does) re-reads the whole table every time and is the slow part.
func handleTable() (map[uint32][]uintptr, error) {
	// open our own file first so it is present in the snapshot below
	f, err := os.Open(os.Args[0])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	proc := windows.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformation")
	buf := make([]byte, 4<<20)
	for {
		var ret uint32
		st, _, _ := proc.Call(64, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), uintptr(unsafe.Pointer(&ret)))
		if uint32(st) == 0xC0000004 { // STATUS_INFO_LENGTH_MISMATCH
			buf = make([]byte, len(buf)*2)
			continue
		}
		if st != 0 {
			return nil, windows.NTStatus(st)
		}
		break
	}
	type entry struct {
		Object, PID, Handle uintptr
		Access              uint32
		Trace, TypeIdx      uint16
		Attr, _             uint32
	}
	n := *(*uintptr)(unsafe.Pointer(&buf[0]))
	es := unsafe.Slice((*entry)(unsafe.Pointer(&buf[2*unsafe.Sizeof(uintptr(0))])), n)

	// the object type index of "File" differs between Windows builds: learn it from our own handle
	self, fileType := uintptr(windows.GetCurrentProcessId()), uint16(0xffff)
	for i := range es {
		if es[i].PID == self && es[i].Handle == f.Fd() {
			fileType = es[i].TypeIdx
			break
		}
	}
	if fileType == 0xffff {
		return nil, errors.New("не удалось определить тип файловых дескрипторов")
	}
	out := map[uint32][]uintptr{}
	for i := range es {
		if es[i].TypeIdx == fileType {
			pid := uint32(es[i].PID)
			out[pid] = append(out[pid], es[i].Handle)
		}
	}
	return out, nil
}

// filesOnDrive duplicates the process's file handles and returns paths on drive.
func filesOnDrive(pid uint32, handles []uintptr, drive string, deadline time.Time) []string {
	ph, err := windows.OpenProcess(windows.PROCESS_DUP_HANDLE, false, pid)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(ph)
	cur := windows.CurrentProcess()
	var (
		files []string
		seen  = map[string]bool{}
		buf   [windows.MAX_LONG_PATH]uint16
	)
	for _, h := range handles {
		if time.Now().After(deadline) {
			break
		}
		var d windows.Handle
		if windows.DuplicateHandle(ph, windows.Handle(h), cur, &d, 0, false, windows.DUPLICATE_SAME_ACCESS) != nil {
			continue
		}
		if t, err := windows.GetFileType(d); err == nil && t == windows.FILE_TYPE_DISK {
			if n, err := windows.GetFinalPathNameByHandle(d, &buf[0], uint32(len(buf)), 0); err == nil && n > 0 {
				path := strings.TrimPrefix(windows.UTF16ToString(buf[:n]), `\\?\`)
				if onDrive(path, drive) && !seen[path] {
					var fi windows.ByHandleFileInformation
					if windows.GetFileInformationByHandle(d, &fi) == nil && fi.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
						seen[path] = true
						files = append(files, path)
					}
				}
			}
		}
		windows.CloseHandle(d)
	}
	return files
}

// scanDrive finds processes that run from, or hold open files on, the given drive ("C:").
func scanDrive(drive string) DiskReport {
	drive = strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(drive, `\`), "/"))
	rep := DiskReport{Drive: drive, Procs: []DiskProc{}}
	if len(drive) != 2 || drive[1] != ':' {
		return rep
	}
	table, err := handleTable()
	if err != nil {
		return rep
	}
	ps, err := process.Processes()
	if err != nil {
		return rep
	}

	deadline := time.Now().Add(12 * time.Second)
	var (
		wg  sync.WaitGroup
		lk  sync.Mutex
		sem = make(chan struct{}, 16)
	)
	for _, p := range ps {
		if p.Pid <= 4 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(p *process.Process) {
			defer wg.Done()
			defer func() { <-sem }()
			exe, _ := p.Exe()
			dp := DiskProc{PID: p.Pid, Exe: exe, OnExe: onDrive(exe, drive), Files: []string{}}
			if hs := table[uint32(p.Pid)]; len(hs) > 0 {
				dp.Files = append(dp.Files, filesOnDrive(uint32(p.Pid), hs, drive, deadline)...)
			}
			if !dp.OnExe && len(dp.Files) == 0 {
				return
			}
			dp.Name, _ = p.Name()
			dp.User, _ = p.Username()
			sort.Strings(dp.Files)
			lk.Lock()
			rep.Procs = append(rep.Procs, dp)
			rep.Total += len(dp.Files)
			lk.Unlock()
		}(p)
	}
	wg.Wait()
	rep.Slow = time.Now().After(deadline)
	sort.Slice(rep.Procs, func(i, j int) bool {
		if len(rep.Procs[i].Files) != len(rep.Procs[j].Files) {
			return len(rep.Procs[i].Files) > len(rep.Procs[j].Files)
		}
		return strings.ToLower(rep.Procs[i].Name) < strings.ToLower(rep.Procs[j].Name)
	})
	return rep
}
