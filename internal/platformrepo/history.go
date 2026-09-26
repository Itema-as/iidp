package platformrepo

import (
	"context"
	"path"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/git"
)

// historyLimit bounds how far back TagDeployedAt looks: that many commits
// that changed the Environment's values file.
const historyLimit = 100

// TagDeployedAt is when tag was deployed to an Environment: the time of
// the Platform repository commit that set image.tag in the Environment's
// values.yaml to tag, the newest such commit when the tag was set more
// than once. repo must be a clone with history (git.CloneWithHistory). ok
// is false when no commit in the last historyLimit changes of the file set
// it.
func TagDeployedAt(ctx context.Context, repo *git.Repository, application, environment, tag string) (at time.Time, ok bool, err error) {
	valuesPath := path.Join(EnvironmentDir(application, environment), "values.yaml")
	commits, err := repo.FileHistory(ctx, valuesPath, historyLimit)
	if err != nil {
		return time.Time{}, false, err
	}
	// Newest first: skip commits after the tag was replaced (the cluster
	// may not have caught up with a newer deploy yet), then walk back
	// through the commits that kept it. The oldest of those set it.
	for _, commit := range commits {
		data, err := repo.Show(ctx, commit.SHA, valuesPath)
		if err != nil {
			// The file was deleted in this commit (iidp app delete):
			// nothing ran it from here on.
			if ok {
				break
			}
			continue
		}
		var values struct {
			Image struct {
				Tag string `yaml:"tag"`
			} `yaml:"image"`
		}
		if yaml.Unmarshal(data, &values) != nil || values.Image.Tag != tag {
			if ok {
				break
			}
			continue
		}
		at, ok = commit.Time, true
	}
	return at, ok, nil
}
