package cli

import (
	"fmt"
	"os"

	"github.com/williamokano/qbt-replicator/internal/backend"
	"github.com/williamokano/qbt-replicator/internal/client"
)

// backend returns the remote API when --host (or $QBTR_HOST) is set, else
// the local service.
func (g *globals) backend() (backend.Backend, error) {
	if h := g.remoteHost(); h != "" {
		key := g.apiKey
		if key == "" {
			key = os.Getenv("QBTR_API_KEY")
		}
		return client.New(h, key)
	}
	svc, err := g.service()
	if err != nil {
		return nil, err
	}
	return backend.Local(svc), nil
}

func (g *globals) remoteHost() string {
	if g.host != "" {
		return g.host
	}
	return os.Getenv("QBTR_HOST")
}

// localOnly refuses commands that need the config file or database in remote mode.
func (g *globals) localOnly(name string) error {
	if g.remoteHost() != "" {
		return fmt.Errorf("%q runs on the qbt-replicator host only; drop --host / QBTR_HOST", name)
	}
	return nil
}
