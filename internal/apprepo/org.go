package apprepo

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Itema-as/iidp/internal/github"
	"github.com/Itema-as/iidp/internal/platform"
	"github.com/Itema-as/iidp/internal/platformrepo"
)

// ErrOutsideOrg is wrapped when an Application repository is not in
// platform.Org. The Deploy gate only accepts tokens from repositories the
// org owns.
var ErrOutsideOrg = errors.New("repository is outside the org")

// CheckInOrg refuses owner/name unless owner is platform.Org, compared
// case-insensitively as GitHub compares logins. Callers check both --repo as
// typed and the owner GitHub reports, which differs when the name redirects
// to a repository that was transferred away.
func CheckInOrg(owner, name string) error {
	if strings.EqualFold(owner, platform.Org) {
		return nil
	}
	return fmt.Errorf("%w: %s/%s belongs to %s, and only repositories in %s can be Applications. Transfer it to %s first (on GitHub: the repository's Settings, Danger Zone, Transfer ownership), then run this again with --repo %s/%s", ErrOutsideOrg, owner, name, owner, platform.Org, platform.Org, platform.Org, name)
}

// Binding is the Platform's binding for a repository GitHub reported as
// fullName with id, owned by owner. It refuses a response without both ids
// rather than record a binding that binds nothing.
func Binding(fullName string, id int64, owner github.Owner) (platformrepo.RepositoryBinding, error) {
	b := platformrepo.RepositoryBinding{Repository: fullName, RepositoryID: id, RepositoryOwnerID: owner.ID}
	if !b.Complete() {
		return platformrepo.RepositoryBinding{}, fmt.Errorf("GitHub reported no repository id or owner id for %s, so the Application cannot be bound to it", fullName)
	}
	return b, nil
}

// BindingForRepository checks that repo, as GitHub reported it, is in the
// org and returns its binding. requested names the repository when GitHub's
// full name is missing.
func BindingForRepository(requested string, repo github.Repository) (platformrepo.RepositoryBinding, error) {
	fullName := repo.FullName
	if fullName == "" {
		fullName = requested
	}
	name := fullName[strings.LastIndex(fullName, "/")+1:]
	if err := CheckInOrg(repo.Owner.Login, name); err != nil {
		return platformrepo.RepositoryBinding{}, err
	}
	return Binding(fullName, repo.ID, repo.Owner)
}
