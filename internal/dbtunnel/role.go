package dbtunnel

import (
	"fmt"

	"github.com/Itema-as/iidp/internal/render"
)

// ChooseRole is the role a developer whose highest permission on the
// Application repository is permission gets on an Environment with
// access: read-write when they qualify for it and did not ask for
// read-only, else read-only when they qualify for that. Whoever qualifies
// for read-write qualifies for read-only too, since read-only never needs
// more, so asking for read-only only steps read-write down. It is false
// when they qualify for neither.
func ChooseRole(permission string, access render.DatabaseAccess, readOnly bool) (render.AccessRole, bool) {
	if !readOnly && render.Qualifies(permission, access.ReadWrite) {
		return render.ReadWriteRole, true
	}
	if render.Qualifies(permission, access.ReadOnly) {
		return render.ReadOnlyRole, true
	}
	return 0, false
}

// levelRefusal words why a developer whose highest permission on
// repository is permission gets no role on an Environment with access,
// and the command that changes the levels.
func levelRefusal(application, environment, repository, permission string, access render.DatabaseAccess, readOnly bool) string {
	if readOnly && access.ReadOnly == render.AccessNone {
		return fmt.Sprintf("refused: your permission on %s is %s, and %s %s's database has no read-only access (read-only is none, and read-write admits %s). Leave out --read-only, or open it with %s --read-only <level>",
			repository, permission, application, environment, levelWords(access.ReadWrite), accessCommand(application, environment))
	}
	return fmt.Sprintf("refused: your permission on %s is %s, and %s %s's database admits read-write for %s and read-only for %s. %s changes who may connect",
		repository, permission, application, environment, levelWords(access.ReadWrite), levelWords(access.ReadOnly), accessCommand(application, environment))
}

// notSetUpRefusal words a level the developer qualifies for whose role or
// password is not there yet.
func notSetUpRefusal(application, environment string) string {
	return fmt.Sprintf("no database access set up for %s yet, run `%s`", environment, accessCommand(application, environment))
}

// levelWords is a level as the permission it needs.
func levelWords(level string) string {
	if level == render.AccessNone {
		return "nobody (none)"
	}
	return level
}

// accessCommand is the iidp app db access command for an Environment's
// levels. A Preview Environment has staging's.
func accessCommand(application, environment string) string {
	if environment != "prod" {
		environment = "staging"
	}
	return fmt.Sprintf("iidp app db access %s --env %s", application, environment)
}
