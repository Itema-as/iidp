// Package repoaccess checks a developer's permission on an Application's
// repository with their own GitHub token, for the services that answer
// developers: the Deploy gate's status endpoint and the database tunnel.
// Both read the Application's binding (ADR-0005) from a clone of the
// Platform repository made with that token, and ask GitHub for the token's
// permission on the bound repository by its numeric id.
package repoaccess

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Itema-as/iidp/internal/git"
	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// Reason is why a check refused.
type Reason int

const (
	// TokenRejected: GitHub does not accept the token.
	TokenRejected Reason = iota + 1
	// PlatformUnreadable: the token may not read the Platform repository.
	PlatformUnreadable
	// NotBound: the Application has no complete repository binding.
	NotBound
	// OutsideOrg: the Application is bound to a repository outside the org.
	OutsideOrg
	// NoAccess: the token may not read the bound repository.
	NoAccess
	// Unavailable: GitHub, or the Platform repository, could not be asked.
	Unavailable
)

// Refusal is a check's refusal, worded for the developer.
type Refusal struct {
	Reason  Reason
	Message string
}

func (r *Refusal) Error() string { return r.Message }

// Status is the HTTP status a service answers the refusal with.
func (r *Refusal) Status() int {
	switch r.Reason {
	case TokenRejected:
		return http.StatusUnauthorized
	case Unavailable:
		return http.StatusBadGateway
	}
	return http.StatusForbidden
}

func refuse(reason Reason, format string, args ...any) *Refusal {
	return &Refusal{Reason: reason, Message: fmt.Sprintf(format, args...)}
}

// Purpose words what a check is for in its refusals.
type Purpose struct {
	// Service is the one checking, as a sentence's subject: "the Deploy
	// gate".
	Service string
	// Action is what only the bound repository's readers may do, with %s
	// for whose: "see %s status" reads "see its status" and "see shop's
	// status".
	Action string
}

func (p Purpose) action(whose string) string {
	return fmt.Sprintf(p.Action, whose)
}

// Clone clones the Platform repository at url into dir with the
// developer's token, with history or shallow. A clone that fails because
// of the token is a *Refusal.
func Clone(ctx context.Context, url, dir, token string, history bool, p Purpose) (*git.Repository, error) {
	clone := git.Clone
	if history {
		clone = git.CloneWithHistory
	}
	repo, err := clone(ctx, url, platformrepo.Branch, dir, git.Auth{Token: token})
	if err != nil {
		return nil, cloneRefusal(err, p)
	}
	return repo, nil
}

// cloneRefusal words a failed clone of the Platform repository with the
// caller's token. GitHub answers "Repository not found" to a token that may
// not read it, and "Authentication failed" to one it does not accept.
func cloneRefusal(err error, p Purpose) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "Authentication failed"), strings.Contains(msg, "Invalid username or token"):
		return refuse(TokenRejected, "GitHub does not accept your token; run gh auth login again")
	case strings.Contains(msg, "not found"), strings.Contains(msg, "403"):
		return refuse(PlatformUnreadable, "refused: your GitHub login cannot read the Platform repository %s", platform.Repository)
	default:
		return refuse(Unavailable, "%s could not read the Platform repository %s: %v", p.Service, platform.Repository, err)
	}
}

// Check reads application's binding from the clone at dir, checks it is
// bound inside the org whose id is orgID, and asks GitHub, with the
// client gh holding the developer's token, for their permission on the
// bound repository. A developer who may not even read it is refused. The
// binding is returned as far as it was read, refused or not.
func Check(ctx context.Context, dir, application string, orgID int64, gh *github.Client, p Purpose) (github.ReadableRepository, platformrepo.RepositoryBinding, error) {
	binding, ok, err := platformrepo.ReadRepositoryBinding(dir, application)
	if err != nil || !ok || !binding.Complete() {
		return github.ReadableRepository{}, binding, refuse(NotBound, "refused: %s is not bound to an Application repository, so there is no repository whose readers may %s. Bind it first: iidp app bind %s --repo %s/<repository>", application, p.action("its"), application, platform.Org)
	}
	if binding.RepositoryOwnerID != orgID {
		return github.ReadableRepository{}, binding, refuse(OutsideOrg, "refused: %s is bound to a repository outside %s (owner id %d); rebind it with iidp app bind %s --repo %s/<repository> --rebind", application, platform.Org, binding.RepositoryOwnerID, application, platform.Org)
	}
	repo, err := gh.RepositoryByID(ctx, binding.RepositoryID)
	switch {
	case github.IsUnauthorized(err):
		return repo, binding, refuse(TokenRejected, "GitHub does not accept your token; run gh auth login again")
	case github.IsNotFound(err), err == nil && !repo.CanPull:
		// GitHub answers 404, not 403, for a private repository the user
		// cannot see: both mean no access.
		return repo, binding, refuse(NoAccess, "refused: you cannot read %s's Application repository (%s, repository id %d) on GitHub, and only its readers may %s. Ask for read access to it", application, binding.Repository, binding.RepositoryID, p.action(application+"'s"))
	case err != nil:
		return repo, binding, refuse(Unavailable, "%s could not ask GitHub whether you can read %s's repository: %v", p.Service, application, err)
	}
	return repo, binding, nil
}
