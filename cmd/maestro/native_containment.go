package main

import (
	"fmt"
	"os"

	"github.com/befeast/maestro/internal/aiexecution"
)

func nativeContainmentCmd(entry bool) {
	var err error
	if entry {
		err = aiexecution.RunNativeEntry()
	} else {
		err = aiexecution.RunNativeMonitor()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func nativeForgejoCredentialCmd(args []string) {
	if len(args) != 1 {
		os.Exit(2)
	}
	if err := aiexecution.RunNativeForgejoCredential(args[0], os.Getenv("FORGEJO_REPOSITORY"), os.Getenv("FORGEJO_TOKEN"), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func nativeForgejoPullRequestCmd() {
	if err := aiexecution.RunNativeForgejoPullRequest(os.Getenv("FORGEJO_REPOSITORY"), os.Getenv("FORGEJO_TOKEN"), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
