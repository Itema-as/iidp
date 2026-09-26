// Package appconfig is iidp.yaml, the file at the root of an Application
// repository that holds the settings the Platform takes from the code
// rather than from the Platform repository, because they must change
// together with the code: the migration command
// (docs/implementation-notes/66-migration-command-in-repo.md) and the
// Scheduled tasks (docs/implementation-notes/91-scheduled-tasks.md).
//
// iidp ci set-image reads it from the checkout of the commit being deployed
// or promoted and sends it to the Deploy gate with the image tag, so an
// Environment always runs the migration command and the Scheduled tasks of
// the code it runs.
//
// What the file's presence means is part of the contract:
//
//   - No iidp.yaml: the deploy sends no migration command, and the gate
//     leaves the Environment's postgres.migrationCommand as it is. An
//     Application made before the file existed keeps working unchanged.
//   - iidp.yaml without migrationCommand, or with an empty one: the deploy
//     sends "", and the gate clears the Environment's command. Deleting the
//     line is how a developer says "no migration".
//   - iidp.yaml with migrationCommand: the gate sets it.
//
// Scheduled tasks have no "leave it as it is" case: they only ever come
// from iidp.yaml, so a deploy that sends none (no tasks, or no iidp.yaml at
// all) removes any the Environment had, and deleting a task stops it.
package appconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

// FileName is the file's name, at the root of the Application repository.
const FileName = "iidp.yaml"

// MaxMigrationCommandBytes is the longest migration command the Deploy gate
// accepts. The command is one shell line run with sh -c; anything longer
// belongs in a script shipped in the image.
const MaxMigrationCommandBytes = 1024

// MaxTaskCommandBytes is the longest command a Scheduled task may run: one
// shell line, the same limit as the migration command's.
const MaxTaskCommandBytes = MaxMigrationCommandBytes

// MaxTasks is how many Scheduled tasks an Application may declare. Each is
// a CronJob, and each run a Pod with the smallest size's CPU and memory on
// a single node. A handful covers the nightly cleanups and reports tasks
// are for; more belongs in one task that does several things.
const MaxTasks = 5

// maxCronJobName is the longest CronJob name Kubernetes accepts: the
// controller appends 11 characters to it to name each Job, and a Job's name
// is at most 63.
const maxCronJobName = 52

// longestEnvironmentSuffix is the longest suffix an Environment adds to the
// Application's name in its objects' names: -staging (the chart's
// application.fullname). Preview Environments run no tasks.
const longestEnvironmentSuffix = "-staging"

// File is iidp.yaml's content.
type File struct {
	// MigrationCommand is the shell line the migration Job runs before
	// every rollout, with DATABASE_URL set. "" means no migration.
	MigrationCommand string
	// Tasks are the Application's Scheduled tasks, in the order declared;
	// nil when there are none.
	Tasks []Task
}

// Task is one Scheduled task: Command runs with sh -c on Schedule, on
// Europe/Oslo time, in a one-off container from the Application's image
// with the Environment's env, secrets and DATABASE_URL. The chart renders
// it as the CronJob <Environment's object name>-<Name>. It is also the
// shape the Deploy gate receives and writes into values.yaml's tasks.
type Task struct {
	Name     string `json:"name" yaml:"name"`
	Schedule string `json:"schedule" yaml:"schedule"`
	Command  string `json:"command" yaml:"command"`
}

// Read reads FileName from dir. ok is false when dir has no such file.
// Only known keys are accepted, each with the type it must have, so a
// misspelt key fails the deploy instead of silently clearing the
// migration command. Surrounding whitespace is trimmed, so a folded
// block scalar (>) reads as the one line it folds to.
func Read(dir string) (f File, ok bool, err error) {
	path := filepath.Join(dir, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// iidp.yml would otherwise be ignored without a word, and the
		// command in it never sent.
		if _, statErr := os.Stat(filepath.Join(dir, "iidp.yml")); statErr == nil {
			return File{}, false, fmt.Errorf("found iidp.yml, which iidp does not read: rename it to %s", FileName)
		}
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, fmt.Errorf("reading %s: %w", FileName, err)
	}
	f, err = Parse(data)
	if err != nil {
		return File{}, false, err
	}
	return f, true, nil
}

// Parse parses iidp.yaml's content: a YAML mapping (or an empty document,
// only comments) whose keys are migrationCommand, a string or null, and
// tasks, a list of tasks or null.
func Parse(data []byte) (File, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		// A plain scalar cannot start with *, so an unquoted schedule
		// such as */15 * * * * reads as an alias and fails here.
		if strings.Contains(err.Error(), "alphabetic or numeric character") {
			return File{}, fmt.Errorf("%s is not valid YAML: %w; quote a schedule that starts with *, such as schedule: \"*/15 * * * *\"", FileName, err)
		}
		return File{}, fmt.Errorf("%s is not valid YAML: %w", FileName, err)
	}
	var f File
	if len(doc.Content) == 0 {
		return f, nil
	}
	root := doc.Content[0]
	if root.Kind == yaml.ScalarNode && root.Tag == "!!null" {
		return f, nil
	}
	if root.Kind != yaml.MappingNode {
		return File{}, fmt.Errorf("%s must be a mapping of settings, such as migrationCommand: npx prisma migrate deploy", FileName)
	}
	seen := map[string]bool{}
	for i := 0; i+1 < len(root.Content); i += 2 {
		key, value := root.Content[i], root.Content[i+1]
		if seen[key.Value] {
			return File{}, fmt.Errorf("%s line %d: %s is set twice", FileName, key.Line, key.Value)
		}
		seen[key.Value] = true
		switch key.Value {
		case "migrationCommand":
			if value.Kind != yaml.ScalarNode || (value.Tag != "!!str" && value.Tag != "!!null") {
				return File{}, fmt.Errorf("%s line %d: migrationCommand must be a string, one shell line; quote it if YAML reads it as something else", FileName, value.Line)
			}
			if value.Tag == "!!str" {
				f.MigrationCommand = strings.TrimSpace(value.Value)
			}
		case "tasks":
			tasks, err := parseTasks(value)
			if err != nil {
				return File{}, err
			}
			f.Tasks = tasks
		default:
			return File{}, fmt.Errorf("%s line %d: unknown setting %q; the settings are migrationCommand and tasks", FileName, key.Line, key.Value)
		}
	}
	if err := ValidateMigrationCommand(f.MigrationCommand); err != nil {
		return File{}, fmt.Errorf("%s: %w", FileName, err)
	}
	if err := ValidateTasks(f.Tasks); err != nil {
		return File{}, fmt.Errorf("%s: %w", FileName, err)
	}
	return f, nil
}

// parseTasks reads the tasks setting: a list (or null, for none) of
// mappings with exactly name, schedule and command, each a string.
func parseTasks(value *yaml.Node) ([]Task, error) {
	if value.Kind == yaml.ScalarNode && value.Tag == "!!null" {
		return nil, nil
	}
	if value.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%s line %d: tasks must be a list of tasks, each with name, schedule and command", FileName, value.Line)
	}
	var tasks []Task
	for _, item := range value.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("%s line %d: each task must be a mapping with name, schedule and command", FileName, item.Line)
		}
		var task Task
		seen := map[string]bool{}
		for i := 0; i+1 < len(item.Content); i += 2 {
			key, field := item.Content[i], item.Content[i+1]
			var into *string
			switch key.Value {
			case "name":
				into = &task.Name
			case "schedule":
				into = &task.Schedule
			case "command":
				into = &task.Command
			default:
				return nil, fmt.Errorf("%s line %d: unknown task setting %q; a task has name, schedule and command, and always runs on Europe/Oslo time", FileName, key.Line, key.Value)
			}
			if seen[key.Value] {
				return nil, fmt.Errorf("%s line %d: the task's %s is set twice", FileName, key.Line, key.Value)
			}
			seen[key.Value] = true
			if field.Kind != yaml.ScalarNode || field.Tag != "!!str" {
				return nil, fmt.Errorf("%s line %d: a task's %s must be a string; quote it if YAML reads it as something else", FileName, field.Line, key.Value)
			}
			*into = strings.TrimSpace(field.Value)
		}
		for _, required := range []string{"name", "schedule", "command"} {
			if !seen[required] {
				return nil, fmt.Errorf("%s line %d: the task has no %s; a task has name, schedule and command", FileName, item.Line, required)
			}
		}
		tasks = append(tasks, task)
	}
	return tasks, nil
}

// taskNamePattern is a DNS-1035 label: a task's name ends its CronJob's
// name and is a label value.
var taskNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// ValidateTasks refuses tasks the Platform cannot run: more than MaxTasks;
// a name that is not lowercase letters, digits and dashes starting with a
// letter, or is used twice; a schedule ValidateSchedule refuses; a command
// that is empty or not one shell line of at most MaxTaskCommandBytes. How
// long a name may be also depends on the Application's name: see
// CheckTaskNamesFit. The Deploy gate applies both to every deploy, and ci
// set-image before it calls the gate.
func ValidateTasks(tasks []Task) error {
	if len(tasks) > MaxTasks {
		return fmt.Errorf("%d tasks are declared, more than the %d allowed; combine related work into one task", len(tasks), MaxTasks)
	}
	names := map[string]bool{}
	for _, task := range tasks {
		if !taskNamePattern.MatchString(task.Name) {
			return fmt.Errorf("task name %q must be lowercase letters, digits and dashes, start with a letter and not end with a dash", task.Name)
		}
		if len(task.Name) > maxCronJobName {
			return fmt.Errorf("task name %q is %d characters; its CronJob's name, which ends with it, can be at most %d", task.Name, len(task.Name), maxCronJobName)
		}
		if names[task.Name] {
			return fmt.Errorf("task name %q is used twice; each task needs a name of its own", task.Name)
		}
		names[task.Name] = true
		if err := ValidateSchedule(task.Schedule); err != nil {
			return fmt.Errorf("task %q: %w", task.Name, err)
		}
		if task.Command == "" {
			return fmt.Errorf("task %q has no command", task.Name)
		}
		if err := validateShellLine("the command", task.Command); err != nil {
			return fmt.Errorf("task %q: %w", task.Name, err)
		}
	}
	return nil
}

// CheckTaskNamesFit refuses a task whose CronJob name would be too long for
// Kubernetes in application's Environments: <application>-<task> in prod
// and <application>-staging-<task> in staging, at most 52 characters. The
// limit is taken against staging's name whichever Environment is deployed,
// so a task prod accepts is not refused once a staging Environment is
// added.
func CheckTaskNamesFit(application string, tasks []Task) error {
	room := maxCronJobName - len(application) - len(longestEnvironmentSuffix) - len("-")
	for _, task := range tasks {
		if len(task.Name) <= room {
			continue
		}
		if room < 1 {
			return fmt.Errorf("task %q: the Application's name %q leaves no room for a task name in its CronJobs' names (%s%s-<task> must be at most %d characters)", task.Name, application, application, longestEnvironmentSuffix, maxCronJobName)
		}
		return fmt.Errorf("task name %q is %d characters, and %s's may be at most %d: its CronJob is named %s%s-<task> in staging, which Kubernetes limits to %d characters", task.Name, len(task.Name), application, room, application, longestEnvironmentSuffix, maxCronJobName)
	}
	return nil
}

// cronParser parses the five standard cron fields the way Kubernetes
// validates CronJob.spec.schedule (robfig/cron's standard parser, without
// its descriptors), so a schedule accepted here is one the API server
// accepts.
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ValidateSchedule refuses anything but a standard five-field cron
// expression: minute, hour, day of month, month, day of week. Macros such
// as @daily and time zone prefixes are refused: tasks always run on
// Europe/Oslo time, and five fields are the one way to say when.
func ValidateSchedule(schedule string) error {
	fields := strings.Fields(schedule)
	switch {
	case len(fields) == 0:
		return errors.New(`the schedule is empty; give five cron fields, such as "0 3 * * *" for 03:00 every day`)
	case strings.HasPrefix(schedule, "@"):
		return fmt.Errorf(`the schedule %q is a macro; give the five cron fields instead, such as "0 3 * * *" for @daily`, schedule)
	case strings.Contains(schedule, "TZ"):
		return fmt.Errorf("the schedule %q names a time zone; tasks always run on Europe/Oslo time, so give only the five cron fields", schedule)
	case len(fields) != 5:
		return fmt.Errorf("the schedule %q has %d fields; a schedule is five cron fields: minute, hour, day of month, month and day of week", schedule, len(fields))
	}
	if _, err := cronParser.Parse(schedule); err != nil {
		return fmt.Errorf("the schedule %q is not a valid cron expression: %v", schedule, err)
	}
	return nil
}

// ValidateMigrationCommand refuses a migration command that is not one
// shell line of at most MaxMigrationCommandBytes: valid UTF-8, no line
// break or other control character (a tab is fine). "" is valid: it
// means no migration. The Deploy gate applies it to every command it
// receives, and ci set-image before it calls the gate.
func ValidateMigrationCommand(command string) error {
	return validateShellLine("the migration command", command)
}

// validateShellLine refuses a command, called what in the message, that is
// not one shell line of at most MaxMigrationCommandBytes: valid UTF-8, no
// line break or other control character (a tab is fine).
func validateShellLine(what, command string) error {
	if len(command) > MaxMigrationCommandBytes {
		return fmt.Errorf("%s is %d bytes, more than the %d allowed; put longer work in a script in the image and run that", what, len(command), MaxMigrationCommandBytes)
	}
	if !utf8.ValidString(command) {
		return fmt.Errorf("%s is not valid UTF-8", what)
	}
	for _, r := range command {
		switch {
		case r == '\n' || r == '\r':
			return fmt.Errorf("%s must be one line: it runs with sh -c, so chain steps with &&", what)
		case r != '\t' && unicode.IsControl(r):
			return fmt.Errorf("%s contains the control character %U", what, r)
		}
	}
	return nil
}

// MigrationCommandLine is the line of iidp.yaml that sets command, quoted
// as YAML needs it: what add-capability --postgres tells the developer
// to add, and what Render writes.
func MigrationCommandLine(command string) string {
	value, err := yaml.Marshal(command)
	if err != nil {
		// A string always marshals.
		panic(err)
	}
	return "migrationCommand: " + strings.TrimSpace(string(value))
}

// exampleCommand is shown commented out in a file that sets no command.
const exampleCommand = "npx prisma migrate deploy"

// Render is the iidp.yaml iidp app create and Adopt write: a comment
// saying what the file is and when and where the migration command runs,
// then the command, or a commented-out example when there is none, and a
// commented-out example of a Scheduled task.
func Render(migrationCommand string) []byte {
	var b strings.Builder
	b.WriteString(`# Settings the Platform takes from this repository rather than from the
# Platform repository, because they change with the code. The deploy
# workflow's iidp ci set-image reads this file from the commit it deploys
# (a push to main) or promotes (a v* tag) and sends it to the Platform's
# Deploy gate together with the image, so an Environment always runs the
# settings of the code it runs. Written by iidp app create; edit it freely.
#
# migrationCommand runs before every rollout of this Application, in each
# Environment it deploys to: a Kubernetes Job in that Environment, from the
# image just built from this commit, with DATABASE_URL and the
# Application's own environment and secrets set. It runs with sh -c, so it
# is one shell line; chain steps with &&. If it fails, the rollout stops and
# the previous version keeps running. It needs the Postgres Capability
# (iidp app add-capability <name> --postgres). Remove the line to run no
# migration; the next deploy clears it. Deleting this whole file instead
# leaves whatever command the Platform already has.
`)
	if migrationCommand == "" {
		b.WriteString("#\n# " + MigrationCommandLine(exampleCommand) + "\n")
	} else {
		b.WriteString(MigrationCommandLine(migrationCommand) + "\n")
	}
	b.WriteString(`
# tasks are Scheduled tasks, for a Web service only: each command runs on
# its schedule in a one-off container from this commit's image, with the
# same environment, secrets and DATABASE_URL as the Application, in each
# Environment this commit is deployed to. schedule is five cron fields
# (minute, hour, day of month, month, day of week) on Europe/Oslo time;
# quote it. command runs with sh -c, one line. A run is stopped after an
# hour, and a run that is still going when the next is due makes that one
# skip. At most 5 tasks, each named with lowercase letters, digits and
# dashes. Remove a task, or the whole list, and the next deploy stops it.
#
# tasks:
#   - name: nightly-cleanup
#     schedule: "0 3 * * *"
#     command: node scripts/cleanup.js
`)
	return []byte(b.String())
}
