//go:build ignore

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
)

const contentKey = "shared-hosting/acceptance-account/acceptance-service/compiler-content/acceptance.tar.zst"

type contentFile struct {
	name string
	mode int64
	data []byte
}

func main() {
	if len(os.Args) != 3 {
		panic("usage: go run generate-acceptance-content.go <archive> <metadata>")
	}
	temporary, err := os.MkdirTemp("", "xmcl-acceptance-content-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(temporary)

	source := []byte("import java.net.ServerSocket; public class MockServer { public static void main(String[] args) throws Exception { try (ServerSocket server = new ServerSocket(25565)) { while (true) { server.accept().close(); } } } }\n")
	if err := os.WriteFile(filepath.Join(temporary, "MockServer.java"), source, 0o600); err != nil {
		panic(err)
	}
	compile := exec.Command("java", "-m", "jdk.compiler/com.sun.tools.javac.Main", "MockServer.java")
	compile.Dir = temporary
	if output, err := compile.CombinedOutput(); err != nil {
		panic(errors.New(err.Error() + ": " + string(output)))
	}
	class, err := os.ReadFile(filepath.Join(temporary, "MockServer.class"))
	if err != nil {
		panic(err)
	}
	runtimeJSON, err := json.Marshal(map[string]any{
		"schemaVersion":          1,
		"runtimeCatalogRevision": runtimecontract.CatalogSHA256(),
		"minecraftVersion":       "1.21.1",
		"loader": map[string]any{
			"kind": "neoforge", "version": "21.1.115",
		},
		"java": map[string]any{
			"component": "java-runtime-delta", "major": 21,
			"jreId": "java-runtime-delta-21",
		},
		"launch": map[string]any{
			"path": ".xmcl/launch.sh", "kind": "generated-server-launcher",
			"arguments": []string{"MockServer"},
		},
	})
	if err != nil {
		panic(err)
	}
	files := []contentFile{
		{name: ".xmcl/runtime.json", mode: 0o644, data: runtimeJSON},
		{
			name: ".xmcl/launch.sh",
			mode: 0o755,
			data: []byte("#!/bin/sh\nset -eu\n: \"${XMCL_JAVA:?XMCL_JAVA is required}\"\nexec \"$XMCL_JAVA\" MockServer\n"),
		},
		{name: "MockServer.class", mode: 0o644, data: class},
	}
	var archive bytes.Buffer
	tarWriter := tar.NewWriter(&archive)
	var logicalSize int64
	paths := make([]string, 0, len(files))
	for _, file := range files {
		if err := tarWriter.WriteHeader(&tar.Header{
			Name: file.name, Mode: file.mode, Size: int64(len(file.data)),
			ModTime: time.Unix(0, 0).UTC(),
		}); err != nil {
			panic(err)
		}
		if _, err := tarWriter.Write(file.data); err != nil {
			panic(err)
		}
		logicalSize += int64(len(file.data))
		paths = append(paths, file.name)
	}
	if err := tarWriter.Close(); err != nil {
		panic(err)
	}
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		panic(err)
	}
	if _, err := encoder.Write(archive.Bytes()); err != nil {
		panic(err)
	}
	if err := encoder.Close(); err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[1], compressed.Bytes(), 0o600); err != nil {
		panic(err)
	}
	digest := sha256.Sum256(compressed.Bytes())
	metadata, err := json.MarshalIndent(map[string]any{
		"key":            contentKey,
		"sha256":         hex.EncodeToString(digest[:]),
		"compressedSize": compressed.Len(),
		"logicalSize":    logicalSize,
		"paths":          paths,
	}, "", "  ")
	if err != nil {
		panic(err)
	}
	if err := os.WriteFile(os.Args[2], append(metadata, '\n'), 0o600); err != nil {
		panic(err)
	}
}
