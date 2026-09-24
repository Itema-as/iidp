package deploygate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Itema-as/iidp/internal/platformrepo"
	"github.com/Itema-as/iidp/internal/registry"
)

// GHCR is the registry the Platform's pull token is for, and the only one
// the gate sends it to.
const GHCR = "ghcr.io"

// RegistryCredentialFromDir reads the GHCR pull credential from the files
// a Kubernetes Secret volume of argocd/ghcr-pull-token holds (username and
// token; infra/README.md, "Adding or rotating the GHCR pull token"). Like
// the App credential, the files are read on every check, so a rotated
// token is picked up once the kubelet refreshes the volume.
func RegistryCredentialFromDir(dir string) func() (registry.Credential, error) {
	return func() (registry.Credential, error) {
		var cred registry.Credential
		for name, into := range map[string]*string{"username": &cred.Username, "token": &cred.Password} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return registry.Credential{}, fmt.Errorf("reading the GHCR pull token: %w", err)
			}
			if *into = strings.TrimSpace(string(data)); *into == "" {
				return registry.Credential{}, fmt.Errorf("the GHCR pull token's %s is empty", name)
			}
		}
		return cred, nil
	}
}

// checkImage refuses a tag the Environment's image repository does not
// have (docs/implementation-notes/61-image-check.md). It runs on the
// clone the write is made from, after the caller is authorised, so an
// unauthorised call never makes the gate ask a registry anything. A tag
// the Environment already runs is not checked again: nothing will be
// committed, and a repeated call stays harmless while a registry is down.
func (g *Gate) checkImage(ctx context.Context, dir, application, environment, tag string) error {
	file := path.Join(platformrepo.EnvironmentDir(application, environment), "values.yaml")
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(file)))
	if err != nil {
		return fmt.Errorf("reading %s: %w", file, err)
	}
	var values struct {
		Image struct {
			Repository string `yaml:"repository"`
			Tag        string `yaml:"tag"`
		} `yaml:"image"`
	}
	if err := yaml.Unmarshal(data, &values); err != nil {
		return fmt.Errorf("parsing %s: %w", file, err)
	}
	if values.Image.Tag == tag {
		return nil
	}
	if values.Image.Repository == "" {
		return fmt.Errorf("%s has no image.repository, so the Deploy gate cannot check the image", file)
	}
	if g.Images == nil {
		return errors.New("the Deploy gate has no image check configured, and deploys nothing unchecked")
	}

	image := values.Image.Repository + ":" + tag
	err = g.Images.Exists(ctx, values.Image.Repository, tag)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, registry.ErrNotFound):
		return refuse(http.StatusUnprocessableEntity, "refused: the image %s does not exist, so nothing was deployed. Push it first (did the workflow's push fail?), or check the tag", image)
	case errors.Is(err, registry.ErrDenied):
		return refuse(http.StatusServiceUnavailable, "couldn't check that the image %s exists, so nothing was deployed: %v. Either %s was never pushed (GHCR answers a missing image like a private one), or the Platform's GHCR pull token cannot read it", image, err, values.Image.Repository)
	case errors.Is(err, registry.ErrUnavailable):
		return refuse(http.StatusServiceUnavailable, "couldn't check that the image %s exists, so nothing was deployed: %v. Run the deploy again once the registry answers", image, err)
	default:
		return fmt.Errorf("checking the image %s: %w", image, err)
	}
}
