package platformrepo

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/Itema-as/iidp/internal/appconfig"
	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/render"
)

// EnvironmentAuto is the Environment a deploy asks for when it leaves the
// choice to the Platform: the Deploy gate turns it into staging or prod
// from the Platform repository and the ref being deployed, never from what
// the workflow says (docs/implementation-notes/60-deploy-gate.md).
const EnvironmentAuto = "auto"

// ImageTagChange is one Deploy or Promote: a new image tag for one
// Environment of an Application.
type ImageTagChange struct {
	Application string
	Tag         string
	// Environment decides, on each fresh clone at dir, which Environment
	// the tag is written to, or refuses the change by returning an error.
	// It runs once the Application is known to have a directory, so it can
	// read the Application's binding and look for a staging Environment in
	// the same clone the write is made from. Nothing is written when it
	// errors.
	Environment func(dir string) (string, error)
	// Body, when set, follows the commit subject after a blank line.
	Body string
	// MigrationCommand, when not nil, is written into
	// postgres.migrationCommand in the same commit as the tag: "" clears
	// it. nil leaves it as it is. A non-empty command for an Environment
	// without Postgres is refused with ErrPostgresMissing, and nothing is
	// written (docs/implementation-notes/66-migration-command-in-repo.md).
	MigrationCommand *string
	// Tasks are written into the top-level tasks list in the same commit
	// as the tag, replacing whatever the Environment had: none removes
	// them, since tasks only ever come from the deployed commit's
	// iidp.yaml. Tasks for a Static site are refused with
	// ErrTasksOnStaticSite, and nothing is written
	// (docs/implementation-notes/91-scheduled-tasks.md).
	Tasks []appconfig.Task
}

// ErrPostgresMissing is wrapped when a deploy carries a migration command
// for an Environment that has no Postgres Capability.
var ErrPostgresMissing = errors.New("the Environment has no Postgres Capability")

// ErrTasksOnStaticSite is wrapped when a deploy carries Scheduled tasks
// for a Static site.
var ErrTasksOnStaticSite = errors.New("the Application is a Static site")

// DeployResult is what SetImageTag wrote.
type DeployResult struct {
	// Environment is the Environment the tag was written to.
	Environment string
	// File is the values file, relative to the Platform repository root.
	File string
	// Commit is the commit that wrote it, or "" when Unchanged.
	Commit string
	// Unchanged is true when the Environment already ran this tag with
	// this migration command and these tasks: nothing was committed, so a
	// retried request is harmless.
	Unchanged bool
	// MigrationCommandChanged is true when the commit changed
	// postgres.migrationCommand.
	MigrationCommandChanged bool
	// TasksChanged is true when the commit changed the tasks.
	TasksChanged bool
}

// SetImageTag writes change.Tag into image.tag of an Environment's
// values.yaml. It clones main, refuses an Application with no directory,
// asks change.Environment which Environment to write, refuses one without
// a live application.yaml, edits image.tag in place (every other key, and
// every comment, untouched), and postgres.migrationCommand too when the
// change carries one, and the tasks list to the change's tasks, commits
// them together as "Deploy <application> <environment> <tag>" and pushes,
// with the same retry-once-on-a-moved-main logic as CreateApplication.
// Every check runs again on the retry's fresh clone.
func (w *Writer) SetImageTag(ctx context.Context, change ImageTagChange) (DeployResult, error) {
	if change.Environment == nil {
		return DeployResult{}, errors.New("SetImageTag: no Environment decision")
	}
	return runWithRetry(ctx, func(ctx context.Context, _ bool) (DeployResult, error) {
		return w.attemptSetImageTag(ctx, change)
	})
}

func (w *Writer) attemptSetImageTag(ctx context.Context, change ImageTagChange) (DeployResult, error) {
	dir, err := os.MkdirTemp("", "iidp-platform-")
	if err != nil {
		return DeployResult{}, err
	}
	defer os.RemoveAll(dir)

	repo, err := git.Clone(ctx, w.URL, Branch, dir, w.Auth)
	if err != nil {
		return DeployResult{}, fmt.Errorf("cloning %s: %w", platform.Repository, err)
	}

	appDir := filepath.Join(dir, filepath.FromSlash(ApplicationsDir), change.Application)
	switch _, err := os.Stat(appDir); {
	case errors.Is(err, fs.ErrNotExist):
		return DeployResult{}, fmt.Errorf("%w: %q has no directory under %s/ in %s", ErrApplicationMissing, change.Application, ApplicationsDir, platform.Repository)
	case err != nil:
		return DeployResult{}, fmt.Errorf("checking for the Application: %w", err)
	}

	environment, err := change.Environment(dir)
	if err != nil {
		return DeployResult{}, err
	}
	live, err := HasEnvironment(dir, change.Application, environment)
	if err != nil {
		return DeployResult{}, err
	}
	envDir := EnvironmentDir(change.Application, environment)
	if !live {
		return DeployResult{}, fmt.Errorf("%w: %s has no %s Environment for %q (expected %s/application.yaml)", ErrEnvironmentMissing, platform.Repository, environment, change.Application, envDir)
	}

	valuesRelPath := path.Join(envDir, "values.yaml")
	valuesAbs := filepath.Join(dir, filepath.FromSlash(valuesRelPath))
	data, err := os.ReadFile(valuesAbs)
	if err != nil {
		return DeployResult{}, fmt.Errorf("reading %s: %w", valuesRelPath, err)
	}
	data, changed, err := render.SetImageTag(data, change.Tag)
	if err != nil {
		return DeployResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
	}
	res := DeployResult{Environment: environment, File: valuesRelPath}
	var migrationNote string
	if change.MigrationCommand != nil {
		command := *change.MigrationCommand
		data, res.MigrationCommandChanged, err = render.SetMigrationCommand(data, command)
		switch {
		case errors.Is(err, render.ErrNoPostgres):
			return DeployResult{}, fmt.Errorf("%w: %s's %s Environment has no database to migrate, so it cannot take the migration command in iidp.yaml. Add the Postgres Capability first (iidp app add-capability %s --postgres), or remove migrationCommand from iidp.yaml", ErrPostgresMissing, change.Application, environment, change.Application)
		case err != nil:
			return DeployResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
		case res.MigrationCommandChanged && command == "":
			migrationNote = "Clear the migration command: iidp.yaml sets none."
		case res.MigrationCommandChanged:
			migrationNote = "Migration command, from iidp.yaml: " + command
		}
		changed = changed || res.MigrationCommandChanged
	}
	data, res.TasksChanged, err = render.SetTasks(data, change.Tasks)
	switch {
	case errors.Is(err, render.ErrTasksOnStaticSite):
		return DeployResult{}, fmt.Errorf("%w: %s's %s Environment is a Static site, which serves files and has no command of its own to run on a schedule, so it cannot take the tasks in iidp.yaml. Remove tasks from iidp.yaml, or run them from a Web service", ErrTasksOnStaticSite, change.Application, environment)
	case err != nil:
		return DeployResult{}, fmt.Errorf("%s: %w", valuesRelPath, err)
	}
	var tasksNote string
	if res.TasksChanged {
		tasksNote = tasksCommitNote(change.Tasks)
	}
	changed = changed || res.TasksChanged
	if !changed {
		res.Unchanged = true
		return res, nil
	}
	if err := os.WriteFile(valuesAbs, data, 0o644); err != nil {
		return DeployResult{}, err
	}

	if err := repo.Add(ctx, valuesRelPath); err != nil {
		return DeployResult{}, err
	}
	message := fmt.Sprintf("Deploy %s %s %s", change.Application, environment, change.Tag)
	if migrationNote != "" {
		message += "\n\n" + migrationNote
	}
	if tasksNote != "" {
		message += "\n\n" + tasksNote
	}
	if change.Body != "" {
		message += "\n\n" + change.Body
	}
	if err := repo.Commit(ctx, message); err != nil {
		return DeployResult{}, err
	}
	if w.BeforePush != nil {
		if err := w.BeforePush(); err != nil {
			return DeployResult{}, err
		}
	}
	if err := repo.Push(ctx, Branch); err != nil {
		if errors.Is(err, git.ErrPushRejected) {
			return DeployResult{}, err
		}
		return DeployResult{}, fmt.Errorf("pushing to %s: %w\nIf this is a permission error, the GitHub App needs contents: write on %s", platform.Repository, err, platform.Repository)
	}
	res.Commit, err = repo.Head(ctx)
	if err != nil {
		return DeployResult{}, err
	}
	return res, nil
}

// tasksCommitNote is the paragraph of a deploy's commit message that says
// its Scheduled tasks changed, one line per task, so the Platform
// repository's log shows when a task was added, changed or removed.
func tasksCommitNote(tasks []appconfig.Task) string {
	if len(tasks) == 0 {
		return "Remove the Scheduled tasks: iidp.yaml declares none."
	}
	lines := []string{"Scheduled tasks, from iidp.yaml:"}
	for _, task := range tasks {
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", task.Name, task.Schedule, task.Command))
	}
	return strings.Join(lines, "\n")
}

// HasEnvironment reports whether application has a live environment (an
// application.yaml ArgoCD applies) in the clone of the Platform repository
// at dir.
func HasEnvironment(dir, application, environment string) (bool, error) {
	if err := ValidateEnvironmentName(environment); err != nil {
		return false, err
	}
	switch _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(EnvironmentDir(application, environment)), "application.yaml")); {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	default:
		return false, fmt.Errorf("checking for the %s Environment: %w", environment, err)
	}
}
