//go:build ignore

package main

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/klauspost/compress/zstd"
	runtimecontract "github.com/voxelum/xmcl-shared-node-agent/internal/runtime"
)

const (
	serverURL  = "https://piston-data.mojang.com/v1/objects/59353fb40c36d304f2035d51e7d6e6baa98dc05c/server.jar"
	serverSHA  = "e3bc55693e93cda0188f2e60aea28113fc647c5e85a15fa3d1b347349231b4bb"
	serverSize = 51627615
)

var contentKeyPattern = regexp.MustCompile(
	`^shared-hosting/[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}/[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}/compiler-content/vanilla-1\.21\.1\.tar\.zst$`,
)

type contentFile struct {
	name string
	mode int64
	data []byte
}

func main() {
	if len(os.Args) != 4 {
		panic("usage: go run generate-acceptance-content.go <archive> <metadata> <content-key>")
	}
	contentKey := os.Args[3]
	if !contentKeyPattern.MatchString(contentKey) {
		panic("content key must be an isolated shared-hosting account/service compiler-content key")
	}
	server := downloadServer()
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
			"arguments": []string{"-jar", "server.jar", "nogui"},
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
			data: []byte("#!/bin/sh\nset -eu\n: \"${XMCL_JAVA:?XMCL_JAVA is required}\"\nexec \"$XMCL_JAVA\" -Xms256M -Xmx768M -jar server.jar nogui\n"),
		},
		{name: "eula.txt", mode: 0o644, data: []byte("eula=true\n")},
		{
			name: "server.properties",
			mode: 0o644,
			data: []byte("motd=XMCL Together acceptance\nonline-mode=false\nview-distance=2\nsimulation-distance=2\nmax-players=2\nsync-chunk-writes=false\n"),
		},
		{name: "server.jar", mode: 0o644, data: server},
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

func downloadServer() []byte {
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return fmt.Errorf("server download redirect rejected")
		},
	}
	response, err := client.Get(serverURL)
	if err != nil {
		panic(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength != serverSize {
		panic(fmt.Errorf("unexpected server response: status=%d size=%d", response.StatusCode, response.ContentLength))
	}
	server, err := io.ReadAll(io.LimitReader(response.Body, serverSize+1))
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(server)
	if len(server) != serverSize || hex.EncodeToString(sum[:]) != serverSHA {
		panic("server artifact integrity check failed")
	}
	return server
}
