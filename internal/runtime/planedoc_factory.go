package runtime

import (
	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/infra/planedoc"
	"github.com/nexu-io/looper/internal/workerhost"
)

type planeDocFactory func(*config.Config, string) (*planedoc.Gateway, string, bool)

func (r *Runtime) resolvePlaneDoc(cfg *config.Config, projectID string) (*planedoc.Gateway, string, bool) {
	if r != nil && r.planeDocFactory != nil {
		return r.planeDocFactory(cfg, projectID)
	}
	return workerhost.PlaneDocForProject(cfg, projectID)
}
