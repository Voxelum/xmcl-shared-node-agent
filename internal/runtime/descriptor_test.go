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

func descriptor(minecraft, component string, major int, loader string) string {
	return `{"schemaVersion":1,"minecraftVersion":"` + minecraft + `","java":{"component":"` +
		component + `","major":` + strconv.Itoa(major) + `},"runtimeCatalog":{"sha256":"` +
		reviewedCatalog.SHA256 +
		`"},"loader":{"kind":"` + loader + `","version":"1.0.0"},"launch":{"kind":"generated-server-launcher","path":".xmcl/launch.sh","arguments":[]},"contentSha256":"` + contentSHA + `"}`
}

func TestValidateWorkspaceSelectsEveryCatalogBundledJRE(t *testing.T) {
	hasJava25 := false
	for _, fixture := range reviewedCatalog.Requirements {
		root := t.TempDir()
		writeDescriptor(t, root, descriptor("1.21.1", fixture.Component, fixture.Major, "fabric"))
		got, err := ValidateWorkspace(root, contentSHA)
		if err != nil || got.Java.Major != fixture.Major {
			t.Fatalf("Java %d validation failed: %#v %v", fixture.Major, got, err)
		}
		java, err := BundledJava(got.Java)
		if err != nil || !strings.Contains(java, "/"+strconv.Itoa(fixture.Major)+"/") {
			t.Fatalf("Java %d selection = %q, %v", fixture.Major, java, err)
		}
		hasJava25 = hasJava25 || fixture.Major == 25
	}
	if !hasJava25 {
		t.Fatal("reviewed catalog no longer includes Java 25")
	}
}

func TestValidateWorkspaceRejectsUntrustedDescriptorFields(t *testing.T) {
	cases := []string{
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), `.xmcl/launch.sh`, `../bin/sh`, 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), `"arguments":[]`, `"arguments":["-Duser=x"]`, 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), `"kind":"fabric"`, `"kind":"vanilla"`, 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), contentSHA, strings.Repeat("b", 64), 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), `"schemaVersion":1`, `"schemaVersion":2`, 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), `"major":21`, `"major":25`, 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), "java-runtime-delta", "unreviewed-component", 1),
		strings.Replace(descriptor("1.21.1", "java-runtime-delta", 21, "fabric"), reviewedCatalog.SHA256, strings.Repeat("c", 64), 1),
		descriptor("1.12.2", "jre-legacy", 8, "neoforge"),
	}
	for _, body := range cases {
		root := t.TempDir()
		writeDescriptor(t, root, body)
		if _, err := ValidateWorkspace(root, contentSHA); err == nil {
			t.Fatalf("unsafe descriptor was accepted: %s", body)
		}
	}
}
