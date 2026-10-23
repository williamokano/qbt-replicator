// Command qbt-replicator replicates completed torrents across qBittorrent
// instances that share the same storage.
package main

import (
	"os"

	"github.com/williamokano/qbt-replicator/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
