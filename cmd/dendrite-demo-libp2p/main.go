package main

import (
	"flag"

	"github.com/matrix-org/dendrite/cmd/dendrite-p2p-demo/server"
)

func main() {
	instanceName := flag.String("name", "dendrite-p2p", "the name of this P2P demo instance")
	instancePort := flag.Int("port", 8080, "the port that the client API will listen on")
	flag.Parse()
	server.Init(".", *instanceName, *instancePort)
}
