package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/bootstraprunner"
)

func main() {
	logger := log.New(os.Stdout, "", 0)
	if runtime.GOOS != "linux" {
		logger.Fatal("unsupported platform: bootstrap execution requires Linux per-job process groups")
	}
	port, err := strconv.Atoi(value("XMCL_LIGHTNODE_SSH_PORT", "22"))
	if err != nil || port < 1 || port > 65535 {
		logger.Fatal("invalid XMCL_LIGHTNODE_SSH_PORT")
	}
	executor := bootstraprunner.ShellExecutor{
		RunnerScript:           required("XMCL_LIGHTNODE_RUNNER_SCRIPT"),
		BootstrapScript:        required("XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT"),
		ServiceFile:            required("XMCL_LIGHTNODE_AGENT_SERVICE"),
		PrivateKey:             required("XMCL_LIGHTNODE_PRIVATE_KEY"),
		ManagedPublicKey:       required("XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY"),
		ManagedPublicKeySHA256: required("XMCL_LIGHTNODE_MANAGED_PUBLIC_KEY_SHA256"),
		SSHUser:                value("XMCL_LIGHTNODE_SSH_USER", "root"),
		Port:                   port,
	}
	handler, err := bootstraprunner.NewHandler(
		required("XMCL_LIGHTNODE_RUNNER_STATE_ROOT"),
		required("XMCL_LIGHTNODE_RUNNER_SECRET"),
		required("XMCL_LIGHTNODE_RUNNER_APPROVAL_SECRET"),
		executor,
	)
	if err != nil {
		logger.Fatal(err)
	}
	// Absent by default. This protected local operator input grants no purchase.
	if grantPath := os.Getenv("XMCL_LIGHTNODE_ISOLATED_TEST_AUTHORIZATION_FILE"); grantPath != "" {
		file, openErr := openProtectedGrant(grantPath)
		if openErr != nil {
			logger.Fatal("cannot open protected isolated test authorization")
		}
		grant, decodeErr := bootstraprunner.DecodeTestTOFUAuthorization(file)
		file.Close()
		if decodeErr != nil {
			logger.Fatal("invalid isolated test authorization")
		}
		handler, err = bootstraprunner.NewIsolatedTestHandler(required("XMCL_LIGHTNODE_RUNNER_STATE_ROOT"), required("XMCL_LIGHTNODE_RUNNER_SECRET"), required("XMCL_LIGHTNODE_RUNNER_APPROVAL_SECRET"), executor, grant)
		if err != nil {
			logger.Fatal(err)
		}
	}
	if socket := os.Getenv("XMCL_LIGHTNODE_GRANT_SOCKET"); socket != "" {
		if os.Getenv("XMCL_LIGHTNODE_ISOLATED_TEST_AUTHORIZATION_FILE") != "" {
			logger.Fatal("grant sources conflict")
		}
		handler.WithGrantLookup(bootstraprunner.UnixGrantLookup(socket))
	}
	server := &http.Server{
		Addr:              value("XMCL_LIGHTNODE_RUNNER_ADDR", "127.0.0.1:8088"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      31 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}
	logger.Printf(`{"event":"lightnode_runner_started","address":%q}`, server.Addr)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal(err)
	}
}

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}

func value(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
