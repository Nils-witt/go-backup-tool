package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// removedIn is the release that removed the config-file settings below.
const removedIn = "v8.0.0"

// removedJobKeys are job keys removed in removedIn, with what to use
// instead. Job keys may also be set at the top level as shared defaults, so
// both places are checked.
var removedJobKeys = map[string]string{
	"cmd":     "add the command to commands: and list its id under the job's files:",
	"command": "list the command under the job's files: instead",
}

// removedSections are top-level sections removed in removedIn, with what to
// do instead. Their settings live in the state db and are managed in the
// web UI; a settings file (see the settingsfile package) takes the same
// sections as they were written here.
var removedSections = map[string]string{
	"receivers":     "receivers are managed in the web UI; to keep these, move the section into a settings file and run go-backup-tool -import-settings <file>",
	"notifications": "notifications are managed in the web UI; to keep these, move the section into a settings file and run go-backup-tool -import-settings <file>",
	"report":        "the report is managed in the web UI; to keep it, move the section into a settings file and run go-backup-tool -import-settings <file>",
}

// checkRemovedKeys reports the first removed key in doc (the config file's
// parsed YAML) by name, with what to do instead, so a config file written
// for an older release fails with that rather than the decoder's generic
// unknown-field error.
func checkRemovedKeys(doc *yaml.Node) error {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil
	}

	root := doc.Content[0].Content
	for i := 0; i+1 < len(root); i += 2 {
		key, value := root[i].Value, root[i+1]

		if hint, ok := removedSections[key]; ok {
			return removedKeyError("top level", key, hint)
		}

		if hint, ok := removedJobKeys[key]; ok {
			return removedKeyError("top level", key, hint)
		}

		if key == "jobs" && value.Kind == yaml.SequenceNode {
			for j, job := range value.Content {
				if err := checkRemovedJobKeys(job, j); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// checkRemovedJobKeys reports the first removed key in job, the index-th
// jobs: entry.
func checkRemovedJobKeys(job *yaml.Node, index int) error {
	if job.Kind != yaml.MappingNode {
		return nil
	}

	where := fmt.Sprintf("jobs[%d]", index)

	for i := 0; i+1 < len(job.Content); i += 2 {
		if job.Content[i].Value == "name" {
			where = fmt.Sprintf("job %q", job.Content[i+1].Value)
		}
	}

	for i := 0; i+1 < len(job.Content); i += 2 {
		if hint, ok := removedJobKeys[job.Content[i].Value]; ok {
			return removedKeyError(where, job.Content[i].Value, hint)
		}
	}

	return nil
}

// checkJobEditingSections rejects jobs:, servers: and commands: entries
// while webui.job-editing is on: the state db holds them then, and the
// config file's entries are no longer imported (removed in removedIn).
func checkJobEditingSections(fc *fileConfig) error {
	if !fc.WebUI.JobEditing || len(fc.Jobs)+len(fc.Servers)+len(fc.Commands) == 0 {
		return nil
	}

	return fmt.Errorf("jobs:, servers: and commands: are managed in the web UI while webui.job-editing is on, and are no longer imported from the config file since %s: remove them from the config file, or turn job-editing off to keep managing them here", removedIn)
}

func removedKeyError(where, key, hint string) error {
	return fmt.Errorf("%s: %s: was removed in %s: %s", where, key, removedIn, hint)
}
