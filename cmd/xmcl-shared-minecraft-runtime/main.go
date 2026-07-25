// xmcl-shared-minecraft-runtime is the fixed entrypoint for the generic
// shared-hosting image. It reads no customer-supplied Docker command/options.
package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
)

const dataRoot = "/data"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "health" {
		if err := health(); err != nil {
			os.Exit(1)
		}
		return
	}
	if err := launch(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func health() error {
	connection, err := net.DialTimeout("tcp", "127.0.0.1:25565", 2*time.Second)
	if err != nil {
		return err
	}
	return connection.Close()
}

func launch() error {
	descriptor, err := runtimecontract.ValidateWorkspace(dataRoot, "")
	if err != nil {
		return fmt.Errorf("validate runtime descriptor: %w", err)
	}
	if os.Getenv("XMCL_EULA_ACCEPTED") != "true" {
		return errors.New("server-side EULA acceptance is required")
	}
	if err := os.WriteFile(filepath.Join(dataRoot, "eula.txt"), []byte("eula=true\n"), 0o640); err != nil {
		return fmt.Errorf("record accepted EULA: %w", err)
	}
	java, err := runtimecontract.BundledJava(descriptor.Java)
	if err != nil {
		return err
	}
	launcher := filepath.Join(dataRoot, ".xmcl", "launch.sh")
	environment := append(os.Environ(), "XMCL_JAVA="+java)
	return syscall.Exec("/bin/sh", []string{"/bin/sh", launcher}, environment)
}
