package builder

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
)

type canonicalVectorFixture struct {
	Domain        string `json:"domain"`
	CanonicalJSON string `json:"canonicalJson"`
	BuildKey      string `json:"buildKey"`
}

func loadCanonicalVector(t *testing.T) canonicalVectorFixture {
	t.Helper()
	data, err := os.ReadFile("testdata/prepared-build-key-v1.fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture canonicalVectorFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func vectorSpec() controlplane.BuildSpecV1 {
	return controlplane.BuildSpecV1{
		SchemaVersion: 1,
		Source: controlplane.BuildSourceIdentity{
			Format: "zip", SHA256: "8739c76e681f900923b900c9df0ef75cf421d39cabb54650c4b9ad19b6a76d85",
		},
		Compatibility: controlplane.BuildCompatibility{
			MinecraftVersion: "1.21.1",
			Loader:           controlplane.BuildLoader{Kind: "fabric", Version: "0.16.10"},
			Java:             controlplane.BuildJava{Component: "java-runtime-delta", Major: 21},
		},
		Files:        []controlplane.BuildFile{},
		Dependencies: []controlplane.BuildDependency{},
		Builder: controlplane.BuildImageIdentity{
			Image:  BuilderImageName + "@sha256:" + strings.Repeat("1", 64),
			Schema: Schema, ToolchainCatalogRevision: strings.Repeat("3", 64),
		},
		Runtime: controlplane.RuntimeImageIdentity{
			Image:                  RuntimeImageName + "@sha256:" + strings.Repeat("2", 64),
			RuntimeCatalogRevision: strings.Repeat("4", 64),
		},
		Target: controlplane.BuildTarget{OS: "linux", Architecture: "amd64"},
	}
}

func fullSpec() controlplane.BuildSpecV1 {
	return controlplane.BuildSpecV1{
		SchemaVersion: 1,
		Source: controlplane.BuildSourceIdentity{
			Format: "mrpack", SHA256: strings.Repeat("1", 64),
		},
		Compatibility: controlplane.BuildCompatibility{
			MinecraftVersion: "1.21.1",
			Loader:           controlplane.BuildLoader{Kind: "neoforge", Version: "21.1.249"},
			Java:             controlplane.BuildJava{Component: "java-runtime-delta", Major: 21},
		},
		Files: []controlplane.BuildFile{{
			Path: "overrides/config/example.toml", TargetPath: "config/example.toml",
			SHA256: strings.Repeat("2", 64), SizeBytes: 123,
		}},
		Dependencies: []controlplane.BuildDependency{
			{Kind: "mod", ID: "modrinth:project:version:example.jar", TargetPath: "mods/example.jar", SHA256: strings.Repeat("3", 64), SizeBytes: 456},
			{Kind: "toolchain", ID: "net.neoforged:neoforge:21.1.249:installer", TargetPath: "toolchain/neoforge-installer.jar", SHA256: strings.Repeat("4", 64), SizeBytes: 789},
		},
		Builder: controlplane.BuildImageIdentity{
			Image:  BuilderImageName + "@sha256:" + strings.Repeat("a", 64),
			Schema: Schema, ToolchainCatalogRevision: strings.Repeat("c", 64),
		},
		Runtime: controlplane.RuntimeImageIdentity{
			Image:                  RuntimeImageName + "@sha256:" + strings.Repeat("b", 64),
			RuntimeCatalogRevision: strings.Repeat("d", 64),
		},
		Target: controlplane.BuildTarget{OS: "linux", Architecture: "amd64"},
	}
}

func identityFor(spec controlplane.BuildSpecV1) Identity {
	return Identity{
		BuilderImage: spec.Builder.Image, ToolchainCatalogRevision: spec.Builder.ToolchainCatalogRevision,
		RuntimeImage: spec.Runtime.Image, RuntimeCatalogRevision: spec.Runtime.RuntimeCatalogRevision,
	}
}

func TestBuildSpecCanonicalVector(t *testing.T) {
	fixture := loadCanonicalVector(t)
	if fixture.Domain != buildKeyDomain {
		t.Fatalf("build key domain = %q", fixture.Domain)
	}
	spec := vectorSpec()
	canonical, err := CanonicalJSON(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != fixture.CanonicalJSON {
		t.Fatalf("canonical JSON changed:\n%s", canonical)
	}
	key, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	if key != fixture.BuildKey {
		t.Fatalf("build key = %s", key)
	}
}

func TestCanonicalJSONUsesPortableJSONStringEscaping(t *testing.T) {
	canonical, err := CanonicalJSON(map[string]any{"value": "<\b\u2028"})
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != "{\"value\":\"<\\b\\u2028\"}" {
		t.Fatalf("canonical JSON string = %s", canonical)
	}
}

func TestBuildKeyBindsEveryCanonicalFieldGroup(t *testing.T) {
	base := fullSpec()
	baseKey, _ := BuildKey(base)
	mutations := map[string]func(*controlplane.BuildSpecV1){
		"source":        func(s *controlplane.BuildSpecV1) { s.Source.Format = "zip" },
		"compatibility": func(s *controlplane.BuildSpecV1) { s.Compatibility.Loader.Version = "21.1.250" },
		"files":         func(s *controlplane.BuildSpecV1) { s.Files[0].SizeBytes++ },
		"dependencies":  func(s *controlplane.BuildSpecV1) { s.Dependencies[0].SizeBytes++ },
		"builder":       func(s *controlplane.BuildSpecV1) { s.Builder.ToolchainCatalogRevision = strings.Repeat("e", 64) },
		"runtime":       func(s *controlplane.BuildSpecV1) { s.Runtime.RuntimeCatalogRevision = strings.Repeat("f", 64) },
		"target":        func(s *controlplane.BuildSpecV1) { s.Target.Architecture = "arm64" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			value := fullSpec()
			mutate(&value)
			key, err := BuildKey(value)
			if err != nil {
				t.Fatal(err)
			}
			if key == baseKey {
				t.Fatal("canonical field mutation did not change build key")
			}
		})
	}
}

func TestValidateRequestExcludesLocationAndResolvedURLFromBuildKey(t *testing.T) {
	spec := fullSpec()
	expectedBuildKey, err := BuildKey(spec)
	if err != nil {
		t.Fatal(err)
	}
	command := controlplane.Command{
		CommandID: "cmd-1", Kind: controlplane.ContentPrepare, NodeID: "node-1",
		ServiceID: "service-1", AssignmentID: "assignment-1", AccountID: "account-1", Attempt: 1,
		Lease: controlplane.CommandLease{Token: "lease", Generation: 1},
		Prepare: &controlplane.ContentPrepareRequest{
			SchemaVersion: 1, ExpectedBuildKey: expectedBuildKey, BuildSpec: spec,
			Source: controlplane.BuildSourceLocation{
				Key: "shared-hosting/account-1/service-1/build-inputs/source.mrpack", SizeBytes: 123,
			},
			ResolvedDependencies: []controlplane.ResolvedDependency{
				{ID: spec.Dependencies[0].ID, URL: "https://cdn.modrinth.com/first"},
				{ID: spec.Dependencies[1].ID, URL: "https://maven.neoforged.net/first"},
			},
		},
	}
	if err := ValidateRequest(command, identityFor(spec)); err != nil {
		t.Fatal(err)
	}
	command.Prepare.Source.SizeBytes = 456
	command.Prepare.ResolvedDependencies[0].URL = "https://cdn.modrinth.com/second"
	if err := ValidateRequest(command, identityFor(spec)); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSpecStrictlyRejectsUnknownFields(t *testing.T) {
	input := strings.Replace(loadCanonicalVector(t).CanonicalJSON, `"schemaVersion":1`, `"schemaVersion":1,"unknown":true`, 1)
	var spec controlplane.BuildSpecV1
	if err := json.Unmarshal([]byte(input), &spec); err == nil {
		t.Fatal("unknown BuildSpec field was accepted")
	}
}

func TestValidateBuildSpecRejectsNoncanonicalOrder(t *testing.T) {
	spec := fullSpec()
	spec.Dependencies[0], spec.Dependencies[1] = spec.Dependencies[1], spec.Dependencies[0]
	var failure *Failure
	if err := ValidateBuildSpec(spec, identityFor(spec)); !errors.As(err, &failure) ||
		failure.Code != "invalid_build_spec" || failure.Class != "terminal" {
		t.Fatalf("failure = %#v, error = %v", failure, err)
	}
}

func TestFailureTaxonomy(t *testing.T) {
	expected := map[string]string{
		"invalid_prepare_command": "terminal", "invalid_build_spec": "terminal",
		"build_key_mismatch": "terminal", "builder_identity_mismatch": "terminal",
		"runtime_identity_mismatch": "terminal", "source_hash_mismatch": "terminal",
		"source_pack_invalid": "terminal", "unsupported_compatibility": "terminal",
		"dependency_rejected": "terminal", "dependency_hash_mismatch": "terminal",
		"installer_failed": "terminal", "builder_output_invalid": "terminal",
		"cache_manifest_invalid": "terminal", "cache_content_invalid": "terminal",
		"immutable_cache_conflict": "terminal-security", "grant_unavailable": "retryable",
		"source_download_failed": "retryable", "dependency_download_failed": "retryable",
		"builder_image_unavailable": "retryable", "docker_unavailable": "retryable",
		"builder_timeout": "retryable", "builder_resource_exhausted": "retryable",
		"content_publish_uncertain": "retryable", "manifest_publish_uncertain": "retryable",
		"lease_lost": "lease-lost",
	}
	if len(failureClasses) != len(expected) {
		t.Fatalf("failure taxonomy has %d codes, want %d", len(failureClasses), len(expected))
	}
	for code, class := range expected {
		if actual, ok := FailureClass(code); !ok || actual != class {
			t.Fatalf("%s class = %q, %v", code, actual, ok)
		}
	}
}
