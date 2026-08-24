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

func descriptor(minecraft, component string, major int, loader, loaderVersion string) string {
	return `{"schemaVersion":1,"runtimeCatalogRevision":"` + reviewedCatalog.SHA256 +
		`","minecraftVersion":"` + minecraft + `","loader":{"kind":"` + loader +
		`","version":"` + loaderVersion + `"},"java":{"component":"` + component +
		`","major":` + strconv.Itoa(major) + `,"jreId":"` + component + `-` +
		strconv.Itoa(major) +
		`"},"launch":{"path":".xmcl/launch.sh","kind":"generated-server-launcher","arguments":["-jar","server.jar"]}}`
}

func TestValidateWorkspaceSelectsEveryReviewedToolchain(t *testing.T) {
	hasJava25 := false
	for _, fixture := range reviewedCatalog.Toolchains {
		root := t.TempDir()
		writeDescriptor(t, root, descriptor(
			fixture.MinecraftVersion,
			fixture.Java.Component,
			fixture.Java.Major,
			fixture.Loader.Kind,
			fixture.Loader.Version,
		))
		got, err := ValidateWorkspace(root, contentSHA)
		if err != nil || got.Java.Major != fixture.Java.Major {
			t.Fatalf("Java %d validation failed: %#v %v", fixture.Java.Major, got, err)
		}
		java, err := BundledJava(got.Java)
		if err != nil || !strings.Contains(java, "/"+strconv.Itoa(fixture.Java.Major)+"/") {
			t.Fatalf("Java %d selection = %q, %v", fixture.Java.Major, java, err)
		}
		hasJava25 = hasJava25 || fixture.MinecraftVersion == "26.2" && fixture.Java.Major == 25
	}
	if !hasJava25 {
		t.Fatal("reviewed catalog no longer includes 26.2 Java 25")
	}
}

func TestValidateWorkspaceRejectsUntrustedDescriptorFields(t *testing.T) {
	valid := descriptor("1.21.1", "java-runtime-delta", 21, "neoforge", "21.1.115")
	cases := []string{
		strings.Replace(valid, `.xmcl/launch.sh`, `../bin/sh`, 1),
		strings.Replace(valid, `"arguments":["-jar","server.jar"]`, `"arguments":["-Duser=x"]`, 1),
		strings.Replace(valid, `"kind":"neoforge"`, `"kind":"vanilla"`, 1),
		strings.Replace(valid, `"schemaVersion":1`, `"schemaVersion":2`, 1),
		strings.Replace(valid, `"major":21`, `"major":25`, 1),
		strings.Replace(valid, "java-runtime-delta", "unreviewed-component", 1),
		strings.Replace(valid, reviewedCatalog.SHA256, strings.Repeat("c", 64), 1),
		strings.Replace(valid, `"version":"21.1.115"`, `"version":"21.1.116"`, 1),
		strings.Replace(valid, `"jreId":"java-runtime-delta-21"`, `"jreId":"../java"`, 1),
		descriptor("1.12.2", "jre-legacy", 8, "neoforge", "21.1.115"),
	}
	for _, body := range cases {
		root := t.TempDir()
		writeDescriptor(t, root, body)
		if _, err := ValidateWorkspace(root, contentSHA); err == nil {
			t.Fatalf("unsafe descriptor was accepted: %s", body)
		}
	}
}

func TestValidateWorkspaceAuthenticatesArchiveHashOutsideDescriptor(t *testing.T) {
	root := t.TempDir()
	writeDescriptor(t, root, descriptor(
		"1.21.1",
		"java-runtime-delta",
		21,
		"neoforge",
		"21.1.115",
	))
	if _, err := ValidateWorkspace(root, contentSHA); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateWorkspace(root, "not-a-sha256"); err == nil {
		t.Fatal("invalid externally authenticated archive hash was accepted")
	}
}

func TestValidateWorkspaceRejectsUnsafeMinecraftVersionIDs(t *testing.T) {
	valid := descriptor("26.2", "java-runtime-epsilon", 25, "fabric", "0.19.3")
	for _, minecraft := range []string{
		" 26.2", "26.2 ", "26.02", "../26.2", "https://example.test/26.2", "26.2;cmd", "26.2\n", "26.3",
	} {
		root := t.TempDir()
		writeDescriptor(t, root, strings.Replace(valid, `"minecraftVersion":"26.2"`, `"minecraftVersion":"`+minecraft+`"`, 1))
		if _, err := ValidateWorkspace(root, contentSHA); err == nil {
			t.Fatalf("unsafe Minecraft version was accepted: %q", minecraft)
		}
	}
}
