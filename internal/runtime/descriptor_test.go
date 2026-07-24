package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const contentSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func writeDescriptor(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, ".xmcl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".xmcl", "runtime.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".xmcl", "launch.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func descriptor(minecraft string, java int, loader string) string {
	return `{"schemaVersion":1,"minecraftVersion":"` + minecraft + `","javaMajor":` +
		strconv.Itoa(java) +
		`,"loader":{"kind":"` + loader + `","version":"1.0.0"},"launch":{"kind":"generated-server-launcher","path":".xmcl/launch.sh","arguments":[]},"contentSha256":"` + contentSHA + `"}`
}

func TestValidateWorkspaceSelectsOnlyBundledJREs(t *testing.T) {
	for _, fixture := range []struct {
		minecraft string
		java      int
		loader    string
	}{
		{minecraft: "1.12.2", java: 8, loader: "forge"},
		{minecraft: "1.20.4", java: 17, loader: "fabric"},
		{minecraft: "1.21.1", java: 21, loader: "neoforge"},
	} {
		root := t.TempDir()
		writeDescriptor(t, root, descriptor(fixture.minecraft, fixture.java, fixture.loader))
		got, err := ValidateWorkspace(root, contentSHA)
		if err != nil || got.JavaMajor != fixture.java {
			t.Fatalf("Java %d validation failed: %#v %v", fixture.java, got, err)
		}
		java, err := BundledJava(fixture.java)
		if err != nil || !strings.Contains(java, "/"+strconv.Itoa(fixture.java)+"/") {
			t.Fatalf("Java %d selection = %q, %v", fixture.java, java, err)
		}
	}
}

func TestValidateWorkspaceRejectsUntrustedDescriptorFields(t *testing.T) {
	cases := []string{
		strings.Replace(descriptor("1.21.1", 21, "fabric"), `.xmcl/launch.sh`, `../bin/sh`, 1),
		strings.Replace(descriptor("1.21.1", 21, "fabric"), `"arguments":[]`, `"arguments":["-Duser=x"]`, 1),
		strings.Replace(descriptor("1.21.1", 21, "fabric"), `"kind":"fabric"`, `"kind":"vanilla"`, 1),
		strings.Replace(descriptor("1.21.1", 21, "fabric"), contentSHA, strings.Repeat("b", 64), 1),
		strings.Replace(descriptor("1.21.1", 21, "fabric"), `"schemaVersion":1`, `"schemaVersion":2`, 1),
		strings.Replace(descriptor("1.21.1", 21, "fabric"), `"javaMajor":21`, `"javaMajor":8`, 1),
		descriptor("1.12.2", 8, "neoforge"),
	}
	for _, body := range cases {
		root := t.TempDir()
		writeDescriptor(t, root, body)
		if _, err := ValidateWorkspace(root, contentSHA); err == nil {
			t.Fatalf("unsafe descriptor was accepted: %s", body)
		}
	}
}
