package main

import (
	"fmt"
	"os"

	"github.com/cloudsync/cloudsync/internal/daemon"
	"github.com/kardianos/service"
)

func main() {
	prg := daemon.NewProgram(version)
	cfg := daemon.BuildServiceConfig("")

	svc, err := service.New(prg, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloudsyncd:", err)
		os.Exit(1)
	}

	if err := svc.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "cloudsyncd:", err)
		os.Exit(1)
	}
}
