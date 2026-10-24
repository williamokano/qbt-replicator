// Command qbt-replicator replicates completed torrents across qBittorrent
// instances that share the same storage.
package main

import (
	"os"
	_ "time/tzdata" // the distroless image has no zoneinfo; TZ must still work (daily summaries)

	"github.com/williamokano/qbt-replicator/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
