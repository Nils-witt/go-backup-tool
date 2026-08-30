//go:build windows

package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

// serviceName is the Windows service go-backup-tool installs itself as and
// looks itself up by for -service=start/stop/uninstall, and the Event Log
// source it logs under while running as that service.
const serviceName = "GoBackupTool"

// serviceFlagPrefix marks a -service=<command> argument. It's matched by a
// literal prefix (rather than a flag.FlagSet, which would need to know
// about every other flag Main is passed through, like -config) so any other
// flags in args pass through untouched to install or, once installed, to
// the service itself.
const serviceFlagPrefix = "-service="

// Main runs go-backup-tool, dispatching to Windows service management or
// service-mode execution as appropriate:
//
//   - "-service=install|uninstall|start|stop": manages the Windows service
//     registration, then exits. install registers the currently running
//     executable, with the rest of args (e.g. -config), as the service's
//     command line, so the service starts with the same configuration as
//     the console invocation that installed it.
//   - Running under the Service Control Manager (detected automatically,
//     no flag needed): serves as the service itself, logging to the
//     Windows Event Log (there's no console to write to) and driving
//     shutdown from SCM stop/shutdown requests instead of OS signals,
//     which SCM doesn't deliver to services.
//   - Anything else: an ordinary interactive console run, same as Run.
func Main(args []string, stderr io.Writer) int {
	if cmd, rest, ok := extractServiceCommand(args); ok {
		if err := controlService(cmd, rest); err != nil {
			_, _ = fmt.Fprintln(stderr, "error:", err)

			return 2
		}

		return 0
	}

	isService, err := svc.IsWindowsService()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "error: checking Windows service session:", err)

		return 2
	}

	if !isService {
		return Run(args, stderr)
	}

	return runAsService(args)
}

// extractServiceCommand pulls a -service=<command> argument out of args, if
// present, returning the command and the remaining arguments with it
// removed.
func extractServiceCommand(args []string) (cmd string, rest []string, ok bool) {
	rest = make([]string, 0, len(args))

	for _, a := range args {
		if v, found := strings.CutPrefix(a, serviceFlagPrefix); found {
			cmd, ok = v, true
			continue
		}

		rest = append(rest, a)
	}

	return cmd, rest, ok
}

// controlService dispatches a -service=<cmd> command, passing rest through
// to installService as the service's future command-line arguments.
func controlService(cmd string, rest []string) error {
	switch cmd {
	case "install":
		return installService(rest)
	case "uninstall", "remove":
		return uninstallService()
	case "start":
		return startService()
	case "stop":
		return stopService()
	default:
		return fmt.Errorf("-service=%s: want install, uninstall, start, or stop", cmd)
	}
}

// installService registers the currently running executable as the
// GoBackupTool Windows service, passed args (with its -config value
// resolved to an absolute path, see resolveConfigArgAbs) as its command
// line, and registers it as an Event Log source for runAsService's log
// output. The service is created stopped (mgr.CreateService doesn't start
// it); start it with -service=start or `sc start GoBackupTool`.
func installService(args []string) error {
	exePath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	exePath, err = filepath.Abs(exePath)
	if err != nil {
		return fmt.Errorf("resolving executable path: %w", err)
	}

	args, err = resolveConfigArgAbs(args)
	if err != nil {
		return err
	}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to Windows service manager: %w", err)
	}
	defer m.Disconnect()

	if s, err := m.OpenService(serviceName); err == nil {
		_ = s.Close()

		return fmt.Errorf("service %q is already installed", serviceName)
	}

	s, err := m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: "Go Backup Tool",
		Description: "Runs go-backup-tool's configured backup jobs.",
		StartType:   mgr.StartAutomatic,
	}, args...)
	if err != nil {
		return fmt.Errorf("creating service: %w", err)
	}
	defer s.Close()

	// Best-effort: without an Event Log source registration, Event Viewer
	// shows the service's log entries with a "message not found" notice
	// instead of readable text, but the service still runs and logs fine.
	_ = eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info)

	return nil
}

// resolveConfigArgAbs rewrites args' -config (or --config) value to an
// absolute path, resolved against the current working directory, appending
// one (pointing at defaultConfigPath) if args doesn't set -config at all.
//
// The Windows Service Control Manager starts services with
// %SystemRoot%\System32 as their working directory, not the directory the
// operator ran -service=install from. Without this, a relative -config
// path — or the relative defaultConfigPath a bare install falls back to —
// would resolve against the wrong directory once the service actually
// starts, even though it worked fine from the console at install time.
func resolveConfigArgAbs(args []string) ([]string, error) {
	out := make([]string, len(args))
	copy(out, args)

	for i := 0; i < len(out); i++ {
		a := out[i]
		if !strings.HasPrefix(a, "-") {
			continue
		}

		name, value, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name != "config" {
			continue
		}

		valueIdx := i
		if !hasValue {
			if i+1 >= len(out) {
				return nil, errors.New("-config: missing value")
			}

			valueIdx = i + 1
			value = out[valueIdx]
		}

		abs, err := filepath.Abs(value)
		if err != nil {
			return nil, fmt.Errorf("resolving -config path: %w", err)
		}

		if hasValue {
			out[i] = "-config=" + abs
		} else {
			out[valueIdx] = abs
		}

		return out, nil
	}

	abs, err := filepath.Abs(defaultConfigPath)
	if err != nil {
		return nil, fmt.Errorf("resolving default config path: %w", err)
	}

	return append(out, "-config="+abs), nil
}

// uninstallService removes the GoBackupTool service and its Event Log
// source registration. The service should be stopped first (-service=stop);
// Delete only marks a running service for deletion once it stops.
func uninstallService() error {
	return withInstalledService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if err := s.Delete(); err != nil {
			return fmt.Errorf("deleting service: %w", err)
		}

		_ = eventlog.Remove(serviceName)

		return nil
	})
}

// startService starts the already-installed GoBackupTool service.
func startService() error {
	return withInstalledService(func(_ *mgr.Mgr, s *mgr.Service) error {
		if err := s.Start(); err != nil {
			return fmt.Errorf("starting service: %w", err)
		}

		return nil
	})
}

// withInstalledService connects to the Windows service manager and opens the
// already-installed GoBackupTool service, running fn with both and closing
// both regardless of fn's outcome. Shared by uninstallService, startService,
// and stopService, which otherwise repeat this connect/open/defer-close
// boilerplate identically before their own distinct logic. installService
// doesn't use it since it wants OpenService to fail (there being no service
// yet to open).
func withInstalledService(fn func(m *mgr.Mgr, s *mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connecting to Windows service manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("service %q is not installed", serviceName)
	}
	defer s.Close()

	return fn(m, s)
}

// stopService requests the GoBackupTool service stop and waits (up to 20s)
// for it to actually do so, so a caller scripting -service=stop followed by
// e.g. an upgrade or -service=uninstall doesn't race the service's own
// shutdown.
func stopService() error {
	return withInstalledService(func(_ *mgr.Mgr, s *mgr.Service) error {
		status, err := s.Control(svc.Stop)
		if err != nil {
			return fmt.Errorf("stopping service: %w", err)
		}

		deadline := time.Now().Add(20 * time.Second)

		for status.State != svc.Stopped {
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for service %q to stop", serviceName)
			}

			time.Sleep(300 * time.Millisecond)

			if status, err = s.Query(); err != nil {
				return fmt.Errorf("querying service status: %w", err)
			}
		}

		return nil
	})
}

// runAsService runs go-backup-tool's jobs under the Service Control
// Manager, logging to the Windows Event Log in place of the stderr an
// interactive run would use (a service has no console to write to).
func runAsService(args []string) int {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		// Nowhere to report this to: no console, and the Event Log source
		// is exactly what failed to open.
		return 1
	}
	defer elog.Close()

	h := &serviceHandler{args: args, stderr: &eventLogWriter{log: elog}}

	if err := svc.Run(serviceName, h); err != nil {
		_ = elog.Error(1, fmt.Sprintf("%s service failed: %v", serviceName, err))

		return 1
	}

	return h.exitCode
}

// serviceHandler implements svc.Handler, running go-backup-tool's jobs
// (via runWithContext) for the lifetime of the service and translating SCM
// stop/shutdown requests into context cancellation.
type serviceHandler struct {
	args     []string
	stderr   io.Writer
	exitCode int
}

func (h *serviceHandler) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown

	changes <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan int, 1)

	go func() {
		done <- runWithContext(ctx, h.args, h.stderr)
	}()

	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case h.exitCode = <-done:
			changes <- svc.Status{State: svc.Stopped}

			return false, uint32(h.exitCode) //nolint:gosec // exit codes are the small fixed set Run returns (0/1/2)
		case cr := <-r:
			switch cr.Cmd {
			case svc.Interrogate:
				changes <- cr.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()

				h.exitCode = <-done

				changes <- svc.Status{State: svc.Stopped}

				return false, uint32(h.exitCode) //nolint:gosec // exit codes are the small fixed set Run returns (0/1/2)
			}
		}
	}
}

// eventLogWriter adapts an *eventlog.Log to io.Writer so it can be passed
// as runWithContext's stderr, routing each line to the matching Event Log
// severity by the level slog's text handler tags every line with (see
// newLogger in app.go): "level=ERROR", "level=WARN", or otherwise
// (INFO/DEBUG) treated as informational.
type eventLogWriter struct {
	log *eventlog.Log
}

func (w *eventLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg == "" {
		return len(p), nil
	}

	var err error

	switch {
	case strings.Contains(msg, "level=ERROR"):
		err = w.log.Error(1, msg)
	case strings.Contains(msg, "level=WARN"):
		err = w.log.Warning(1, msg)
	default:
		err = w.log.Info(1, msg)
	}

	if err != nil {
		return 0, err
	}

	return len(p), nil
}
