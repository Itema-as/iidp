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

// CheckWorkflowScope refuses a token whose scopes GitHub reported and which
// lacks WorkflowScope: both Create and Adopt push templates.DeployWorkflowPath
// with the developer's gh token, and GitHub refuses that push without the
// scope, which for Create would come only after the repository already
// exists. A token whose scopes are unknown (a fine-grained personal access
// token or a GitHub App token, which report no scopes) is let through,
// since nothing up front can say whether it may push workflows
// (docs/implementation-notes/47-workflow-scope.md).
func CheckWorkflowScope(scopes github.TokenScopes) error {
	if !scopes.Known || scopes.Has(WorkflowScope) {
		return nil
	}
	return fmt.Errorf("%w, and GitHub refuses a push that adds %s without it. Nothing was created. Add the scope with:\n\n  gh auth refresh -s workflow\n\nthen run this command again", ErrMissingWorkflowScope, templates.DeployWorkflowPath)
}
