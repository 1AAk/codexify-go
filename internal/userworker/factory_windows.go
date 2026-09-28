//go:build windows

package userworker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/benice2me11/codexify-go/internal/supervisor"
	"golang.org/x/sys/windows"
)

type Factory struct {
	Executable string
	ConfigPath string
	Env        map[string]string
	Log        *slog.Logger
}

func (f *Factory) Start(context.Context) (supervisor.Process, error) {
	sessionID, token, username, err := activeUserToken()
	if err != nil {
		return nil, err
	}
	defer token.Close()

	env, err := environmentBlock(token, f.Env)
	if err != nil {
		return nil, err
	}

	executable, err := filepath.Abs(f.Executable)
	if err != nil {
		return nil, err
	}
	configPath, err := filepath.Abs(f.ConfigPath)
	if err != nil {
		return nil, err
	}
	cmdline := strings.Join([]string{
		windows.EscapeArg(executable),
		"worker",
		"run",
		"--config",
		windows.EscapeArg(configPath),
	}, " ")
	appName, err := windows.UTF16PtrFromString(executable)
	if err != nil {
		return nil, err
	}
	cmdPtr, err := windows.UTF16PtrFromString(cmdline)
	if err != nil {
		return nil, err
	}
	currentDir, err := token.GetUserProfileDirectory()
	if err != nil {
		currentDir = filepath.Dir(executable)
	}
	currentDirPtr, err := windows.UTF16PtrFromString(currentDir)
	if err != nil {
		return nil, err
	}
	desktop, _ := windows.UTF16PtrFromString("winsta0\\default")

	startup := windows.StartupInfo{
		Cb:      uint32(unsafe.Sizeof(windows.StartupInfo{})),
		Desktop: desktop,
	}
	var proc windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcessAsUser(
		token,
		appName,
		cmdPtr,
		nil,
		nil,
		false,
		flags,
		&env[0],
		currentDirPtr,
		&startup,
		&proc,
	); err != nil {
		return nil, fmt.Errorf("CreateProcessAsUser session %d user %s: %w", sessionID, username, err)
	}

	job, err := createKillOnCloseJob()
	if err != nil {
		_ = windows.TerminateProcess(proc.Process, 1)
		windows.CloseHandle(proc.Thread)
		windows.CloseHandle(proc.Process)
		return nil, err
	}
	if err := windows.AssignProcessToJobObject(job, proc.Process); err != nil {
		windows.CloseHandle(job)
		_ = windows.TerminateProcess(proc.Process, 1)
		windows.CloseHandle(proc.Thread)
		windows.CloseHandle(proc.Process)
		return nil, fmt.Errorf("assign worker to job: %w", err)
	}
	if _, err := windows.ResumeThread(proc.Thread); err != nil {
		_ = windows.TerminateJobObject(job, 1)
		windows.CloseHandle(job)
		windows.CloseHandle(proc.Thread)
		windows.CloseHandle(proc.Process)
		return nil, fmt.Errorf("resume worker: %w", err)
	}
	windows.CloseHandle(proc.Thread)

	p := &process{
		pid:     int(proc.ProcessId),
		process: proc.Process,
		job:     job,
		done:    make(chan error, 1),
	}
	if f.Log != nil {
		f.Log.Info("user worker started", "pid", p.pid, "session_id", sessionID, "user", username)
	}
	go p.wait()
	return p, nil
}

type process struct {
	pid     int
	process windows.Handle
	job     windows.Handle
	done    chan error
	once    sync.Once
}

func (p *process) PID() int { return p.pid }

func (p *process) Done() <-chan error { return p.done }

func (p *process) Stop(ctx context.Context) error {
	p.once.Do(func() {
		_ = windows.TerminateJobObject(p.job, 0)
	})
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *process) wait() {
	event, err := windows.WaitForSingleObject(p.process, windows.INFINITE)
	var result error
	if err != nil {
		result = err
	} else if event != windows.WAIT_OBJECT_0 {
		result = fmt.Errorf("unexpected wait result %#x", event)
	} else {
		var code uint32
		if err := windows.GetExitCodeProcess(p.process, &code); err != nil {
			result = err
		} else if code != 0 {
			result = fmt.Errorf("worker exited with code %d", code)
		}
	}
	windows.CloseHandle(p.process)
	windows.CloseHandle(p.job)
	p.done <- result
	close(p.done)
}

func activeUserToken() (uint32, windows.Token, string, error) {
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return 0, 0, "", fmt.Errorf("enumerate WTS sessions: %w", err)
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))

	list := unsafe.Slice(sessions, count)
	console := windows.WTSGetActiveConsoleSessionId()
	var candidates []uint32
	for _, s := range list {
		if s.State != windows.WTSActive {
			continue
		}
		if s.SessionID == console {
			candidates = append([]uint32{s.SessionID}, candidates...)
		} else {
			candidates = append(candidates, s.SessionID)
		}
	}
	if len(candidates) == 0 {
		return 0, 0, "", errors.New("no active interactive Windows session")
	}

	var lastErr error
	for _, sessionID := range candidates {
		var token windows.Token
		if err := windows.WTSQueryUserToken(sessionID, &token); err != nil {
			lastErr = err
			continue
		}
		username := tokenUsername(token)
		return sessionID, token, username, nil
	}
	return 0, 0, "", fmt.Errorf("query active user token: %w", lastErr)
}

func tokenUsername(token windows.Token) string {
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return "unknown"
	}
	account, domain, _, err := user.User.Sid.LookupAccount("")
	if err != nil {
		return user.User.Sid.String()
	}
	if domain == "" {
		return account
	}
	return domain + "\\" + account
}

func createKillOnCloseJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ret, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil || ret == 0 {
		windows.CloseHandle(job)
		if err != nil {
			return 0, err
		}
		return 0, errors.New("SetInformationJobObject failed")
	}
	return job, nil
}

func environmentBlock(token windows.Token, additions map[string]string) ([]uint16, error) {
	var raw *uint16
	if err := windows.CreateEnvironmentBlock(&raw, token, false); err != nil {
		return nil, fmt.Errorf("create user environment: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(raw)

	entries := readEnvironmentBlock(raw)
	for key, value := range additions {
		prefix := strings.ToUpper(key) + "="
		filtered := entries[:0]
		for _, entry := range entries {
			if strings.HasPrefix(strings.ToUpper(entry), prefix) {
				continue
			}
			filtered = append(filtered, entry)
		}
		entries = append(filtered, key+"="+value)
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToUpper(entries[i]) < strings.ToUpper(entries[j])
	})

	var block []uint16
	for _, entry := range entries {
		u, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, u...)
	}
	block = append(block, 0)
	return block, nil
}

func readEnvironmentBlock(block *uint16) []string {
	if block == nil {
		return nil
	}
	var entries []string
	ptr := unsafe.Pointer(block)
	for {
		cur := (*uint16)(ptr)
		if *cur == 0 {
			break
		}
		s := windows.UTF16PtrToString(cur)
		entries = append(entries, s)
		units := len(utf16.Encode([]rune(s))) + 1
		ptr = unsafe.Add(ptr, units*2)
	}
	return entries
}
