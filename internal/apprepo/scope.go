package apprepo

import (
	"errors"
	"fmt"

	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/templates"
)

// WorkflowScope is the OAuth scope GitHub requires of a token that pushes a
// commit creating or changing a file under .github/workflows/ over HTTPS.
const WorkflowScope = "workflow"

// ErrMissingWorkflowScope is wrapped by CheckWorkflowScope when the
// developer's token lacks WorkflowScope.
var ErrMissingWorkflowScope = errors.New("your GitHub login lacks the workflow scope")

// CheckWorkflowScope refuses a token whose reported scopes lack
// WorkflowScope, before anything is created: GitHub would refuse the push
// of templates.DeployWorkflowPath only after Create made the repository. A
// token whose scopes are unknown is let through.
func CheckWorkflowScope(scopes github.TokenScopes) error {
	if !scopes.Known || scopes.Has(WorkflowScope) {
		return nil
	}
	return fmt.Errorf("%w, and GitHub refuses a push that adds %s without it. Nothing was created. Add the scope with:\n\n  gh auth refresh -s workflow\n\nthen run this command again", ErrMissingWorkflowScope, templates.DeployWorkflowPath)
}
