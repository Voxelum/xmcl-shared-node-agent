//go:build linux

package main

import (
	"encoding/json"
	"github.com/voxelum/xmcl-shared-node-agent/internal/grantbroker"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("protected configuration path required")
	}
	f, e := openProtectedGrant(os.Args[1])
	if e != nil {
		log.Fatal("protected configuration unavailable")
	}
	var c struct {
		Secret    string `json:"secret"`
		Root      string `json:"root"`
		Socket    string `json:"socket"`
		ReaderGID int    `json:"readerGid"`
	}
	d := json.NewDecoder(f)
	d.DisallowUnknownFields()
	e = d.Decode(&c)
	f.Close()
	if e != nil || c.ReaderGID <= 0 || !filepath.IsAbs(c.Socket) {
		log.Fatal("invalid configuration")
	}
	b, e := grantbroker.Open(c.Root, c.Secret)
	if e != nil {
		log.Fatal("grant store unavailable")
	}
	defer b.Close()
	// Never unlink an existing socket: ambiguous owner/startup is fail-closed.
	listener, e := net.Listen("unix", c.Socket)
	if e != nil {
		log.Fatal("socket unavailable")
	}
	defer listener.Close()
	if os.Chown(c.Socket, 0, c.ReaderGID) != nil || os.Chmod(c.Socket, 0660) != nil {
		log.Fatal("socket protection failed")
	}
	server := &http.Server{Handler: b, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, MaxHeaderBytes: 8192}
	if e = server.Serve(listener); e != nil {
		log.Fatal("broker stopped")
	}
}
