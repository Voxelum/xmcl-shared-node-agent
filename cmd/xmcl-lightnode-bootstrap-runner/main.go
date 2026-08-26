package main

import (
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/voxelum/xmcl-shared-node-agent/internal/bootstraprunner"
)

func main() {
	logger := log.New(os.Stdout, "", 0)
	port, err := strconv.Atoi(value("XMCL_LIGHTNODE_SSH_PORT", "22"))
	if err != nil || port < 1 || port > 65535 {
		logger.Fatal("invalid XMCL_LIGHTNODE_SSH_PORT")
	}
	executor := bootstraprunner.ShellExecutor{
		RunnerScript:    required("XMCL_LIGHTNODE_RUNNER_SCRIPT"),
		BootstrapScript: required("XMCL_LIGHTNODE_BOOTSTRAP_SCRIPT"),
		ServiceFile:     required("XMCL_LIGHTNODE_AGENT_SERVICE"),
		PrivateKey:      required("XMCL_LIGHTNODE_PRIVATE_KEY"),
		SSHUser:         value("XMCL_LIGHTNODE_SSH_USER", "ubuntu"),
		Port:            port,
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
