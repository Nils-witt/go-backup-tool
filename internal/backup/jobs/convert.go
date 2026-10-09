package jobs

import (
	"slices"
	"strings"

	"nilswitt.dev/go-backup-tool/internal/backup/config"
	"nilswitt.dev/go-backup-tool/internal/backup/store"
)

// normalizeServer trims fs's fields, so the stored value is what's used.
func normalizeServer(fs config.FileServer) config.FileServer {
	fs.Name = strings.TrimSpace(fs.Name)
	fs.Type = strings.TrimSpace(fs.Type)
	fs.Endpoint = strings.TrimSpace(fs.Endpoint)
	fs.Path = strings.TrimSpace(fs.Path)
	fs.Retention = strings.TrimSpace(fs.Retention)

	return fs
}

// normalizeCommand trims fc's single-line fields.
func normalizeCommand(fc config.FileCommand) config.FileCommand {
	fc.ID = strings.TrimSpace(fc.ID)
	fc.Cmd = strings.TrimSpace(fc.Cmd)
	fc.Timeout = strings.TrimSpace(fc.Timeout)
	fc.Container = strings.TrimSpace(fc.Container)
	fc.ContainerUser = strings.TrimSpace(fc.ContainerUser)

	return fc
}

// normalizeJob trims fj's fields and drops empty list entries.
func normalizeJob(fj config.FileJob) config.FileJob {
	for _, f := range []*string{&fj.Name, &fj.Cmd, &fj.Key, &fj.GPGBin, &fj.GPGHomedir, &fj.Interval, &fj.StartTime, &fj.StagingDir, &fj.Container, &fj.ContainerUser} {
		*f = strings.TrimSpace(*f)
	}

	fj.Recipients = trimAll(fj.Recipients)
	fj.FailureNotifications = trimAll(fj.FailureNotifications)

	targets := make([]config.FileJobTarget, len(fj.Targets))
	for i, t := range fj.Targets {
		t.Server = strings.TrimSpace(t.Server)
		t.Bucket = strings.TrimSpace(t.Bucket)
		t.Retention = strings.TrimSpace(t.Retention)

		if t.OnError != nil {
			onError := *t.OnError
			onError.Command = strings.TrimSpace(onError.Command)
			t.OnError = &onError
		}

		if t.OnRecover != nil {
			t.OnRecover = &config.FileTargetOnRecover{Command: strings.TrimSpace(t.OnRecover.Command)}
		}

		targets[i] = t
	}

	fj.Targets = targets

	return fj
}

func trimAll(ss []string) []string {
	out := make([]string, 0, len(ss))

	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}

	return out
}

func toStoreServer(fs config.FileServer) store.ServerConfig {
	return store.ServerConfig{Name: fs.Name, Type: fs.Type, Endpoint: fs.Endpoint, Path: fs.Path, Retention: fs.Retention}
}

func fromStoreServer(sc store.ServerConfig) config.FileServer {
	return config.FileServer{Name: sc.Name, Type: sc.Type, Endpoint: sc.Endpoint, Path: sc.Path, Retention: sc.Retention}
}

func toStoreCommand(fc config.FileCommand) store.CommandConfig {
	return store.CommandConfig{ID: fc.ID, Cmd: fc.Cmd, Timeout: fc.Timeout, Container: fc.Container, ContainerUser: fc.ContainerUser}
}

// FileCommandFrom converts a stored command back into the definition it
// was entered as, the way FileJobFrom does for a job.
func FileCommandFrom(cc store.CommandConfig) config.FileCommand {
	return config.FileCommand{ID: cc.ID, Cmd: cc.Cmd, Timeout: cc.Timeout, Container: cc.Container, ContainerUser: cc.ContainerUser}
}

func toStoreJob(fj config.FileJob) store.JobConfig {
	targets := make([]store.JobTarget, len(fj.Targets))
	for i, t := range fj.Targets {
		targets[i] = toStoreTarget(t)
	}

	return store.JobConfig{
		Name: fj.Name, Cmd: fj.Cmd, Key: fj.Key, Targets: targets, Recipients: slices.Clone(fj.Recipients), Armor: fj.Armor,
		GPGBin: fj.GPGBin, GPGHomedir: fj.GPGHomedir, Interval: fj.Interval, StartTime: fj.StartTime, StagingDir: fj.StagingDir,
		FailureNotifications: slices.Clone(fj.FailureNotifications), Container: fj.Container, ContainerUser: fj.ContainerUser,
	}
}

func toStoreTarget(t config.FileJobTarget) store.JobTarget {
	out := store.JobTarget{Server: t.Server, Bucket: t.Bucket, Retention: t.Retention}

	if e := t.OnError; e != nil {
		out.OnError = &store.JobTargetOnError{Command: e.Command, After: e.After, Repeat: e.Repeat}
	}

	if r := t.OnRecover; r != nil {
		out.OnRecover = &store.JobTargetOnRecover{Command: r.Command}
	}

	return out
}

// FileJobFrom converts a stored job back into the definition it was
// entered as, e.g. to send it to the web UI's edit form.
func FileJobFrom(jc store.JobConfig) config.FileJob {
	fj := config.FileJob{
		Name: jc.Name, Cmd: jc.Cmd, Key: jc.Key, Recipients: slices.Clone(jc.Recipients), Armor: jc.Armor,
		GPGBin: jc.GPGBin, GPGHomedir: jc.GPGHomedir, Interval: jc.Interval, StartTime: jc.StartTime, StagingDir: jc.StagingDir,
		FailureNotifications: slices.Clone(jc.FailureNotifications), Container: jc.Container, ContainerUser: jc.ContainerUser,
		Targets: make([]config.FileJobTarget, len(jc.Targets)),
	}

	for i, t := range jc.Targets {
		fj.Targets[i] = config.FileJobTarget{Server: t.Server, Bucket: t.Bucket, Retention: t.Retention}

		if e := t.OnError; e != nil {
			fj.Targets[i].OnError = &config.FileTargetOnError{Command: e.Command, After: e.After, Repeat: e.Repeat}
		}

		if r := t.OnRecover; r != nil {
			fj.Targets[i].OnRecover = &config.FileTargetOnRecover{Command: r.Command}
		}
	}

	return fj
}
