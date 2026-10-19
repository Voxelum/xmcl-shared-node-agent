package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/voxelum/xmcl-shared-node-agent/internal/controlplane"
	"golang.org/x/text/unicode/norm"
)

const (
	Schema                     = "agent_builder_v1"
	buildKeyDomain             = "xmcl-prepared-content-build-key-v1\n"
	preparedPrefix             = "shared-runtime/prepared/v1/linux-amd64/"
	BuilderImageName           = "ghcr.io/voxelum/xmcl-shared-node-builder"
	RuntimeImageName           = "ghcr.io/voxelum/xmcl-shared-minecraft-runtime"
	MaxSourceBytes       int64 = 400 << 20
	MaxFileBytes         int64 = 256 << 20
	MaxDependencyBytes   int64 = 512 << 20
	MaxPackageInputBytes int64 = 384 << 20
	MaxContentBytes      int64 = 401 << 20
	MaxBuildEntries            = 16384
)

var (
	shaPattern       = regexp.MustCompile(`^[a-f0-9]{64}$`)
	versionPattern   = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,127}$`)
	minecraftPattern = regexp.MustCompile(`^(?:1\.(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})|[1-9][0-9]{1,3}\.(?:0|[1-9][0-9]{0,2})(?:\.(?:0|[1-9][0-9]{0,2}))?)$`)
	idPattern        = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._:+@/-]{0,511}$`)
)

type Identity struct {
	BuilderImage             string
	ToolchainCatalogRevision string
	RuntimeImage             string
	RuntimeCatalogRevision   string
}

type Failure struct {
	Code  string
	Class string
	Err   error
}

func (e *Failure) Error() string { return e.Err.Error() }
func (e *Failure) Unwrap() error { return e.Err }

var failureClasses = map[string]string{
	"invalid_prepare_command":    "terminal",
	"invalid_build_spec":         "terminal",
	"build_key_mismatch":         "terminal",
	"builder_identity_mismatch":  "terminal",
	"runtime_identity_mismatch":  "terminal",
	"source_hash_mismatch":       "terminal",
	"source_pack_invalid":        "terminal",
	"unsupported_compatibility":  "terminal",
	"dependency_rejected":        "terminal",
	"dependency_hash_mismatch":   "terminal",
	"installer_failed":           "terminal",
	"builder_output_invalid":     "terminal",
	"cache_manifest_invalid":     "terminal",
	"cache_content_invalid":      "terminal",
	"immutable_cache_conflict":   "terminal-security",
	"grant_unavailable":          "retryable",
	"source_download_failed":     "retryable",
	"dependency_download_failed": "retryable",
	"builder_image_unavailable":  "retryable",
	"docker_unavailable":         "retryable",
	"builder_timeout":            "retryable",
	"builder_resource_exhausted": "retryable",
	"content_publish_uncertain":  "retryable",
	"manifest_publish_uncertain": "retryable",
	"lease_lost":                 "lease-lost",
}

func FailureClass(code string) (string, bool) {
	class, ok := failureClasses[code]
	return class, ok
}

func terminal(code string, err error) error {
	class, ok := FailureClass(code)
	if !ok || (class != "terminal" && class != "terminal-security") {
		panic("invalid terminal builder failure code: " + code)
	}
	return &Failure{Code: code, Class: class, Err: err}
}

func retryable(code string, err error) error {
	if class, ok := FailureClass(code); !ok || class != "retryable" {
		panic("invalid retryable builder failure code: " + code)
	}
	return &Failure{Code: code, Class: "retryable", Err: err}
}

func CacheKeys(buildKey string) (string, string, error) {
	if !shaPattern.MatchString(buildKey) {
		return "", "", errors.New("invalid build key")
	}
	base := preparedPrefix + buildKey + "/"
	return base + "content.tar.zst", base + "manifest.json", nil
}

func ValidateRequest(command controlplane.Command, identity Identity) error {
	if command.Kind != controlplane.ContentPrepare || command.CommandID == "" ||
		command.NodeID == "" || command.ServiceID == "" || command.AssignmentID == "" ||
		command.AccountID == "" || command.Attempt < 1 || command.Attempt > 3 ||
		command.Prepare == nil || command.Prepare.SchemaVersion != 1 ||
		!shaPattern.MatchString(command.Prepare.ExpectedBuildKey) {
		return terminal("invalid_prepare_command", errors.New("invalid content prepare command"))
	}
	if err := ValidateBuildSpec(command.Prepare.BuildSpec, identity); err != nil {
		return err
	}
	source := command.Prepare.Source
	if source.SizeBytes < 1 || source.SizeBytes > MaxSourceBytes ||
		!validSourceKey(source.Key, command.AccountID, command.ServiceID) {
		return terminal("invalid_prepare_command", errors.New("invalid content source location"))
	}
	dependencies := command.Prepare.BuildSpec.Dependencies
	resolved := command.Prepare.ResolvedDependencies
	if len(resolved) != len(dependencies) {
		return terminal("dependency_rejected", errors.New("resolved dependency set is incomplete"))
	}
	resolvedIDs := make(map[string]struct{}, len(resolved))
	for index, dependency := range dependencies {
		if resolved[index].ID != dependency.ID || resolved[index].URL == "" {
			return terminal("dependency_rejected", errors.New("resolved dependency order or identity is invalid"))
		}
		if _, exists := resolvedIDs[resolved[index].ID]; exists {
			return terminal("dependency_rejected", errors.New("resolved dependency identity is duplicated"))
		}
		resolvedIDs[resolved[index].ID] = struct{}{}
	}
	key, err := BuildKey(command.Prepare.BuildSpec)
	if err != nil {
		return terminal("invalid_build_spec", err)
	}
	if key != command.Prepare.ExpectedBuildKey {
		return terminal("build_key_mismatch", errors.New("content prepare build key does not match canonical specification"))
	}
	return nil
}

func ValidateBuildSpec(spec controlplane.BuildSpecV1, identity Identity) error {
	if spec.SchemaVersion != 1 ||
		(spec.Source.Format != "mrpack" && spec.Source.Format != "zip") ||
		!shaPattern.MatchString(spec.Source.SHA256) ||
		spec.Target.OS != "linux" || spec.Target.Architecture != "amd64" ||
		len(spec.Files) > MaxBuildEntries || len(spec.Dependencies) > MaxBuildEntries {
		return terminal("invalid_build_spec", errors.New("build specification identity is invalid"))
	}
	if spec.Builder.Schema != Schema ||
		spec.Builder.Image != identity.BuilderImage ||
		spec.Builder.ToolchainCatalogRevision != identity.ToolchainCatalogRevision ||
		!digestImage(spec.Builder.Image, BuilderImageName) ||
		!shaPattern.MatchString(spec.Builder.ToolchainCatalogRevision) {
		return terminal("builder_identity_mismatch", errors.New("builder image is not the reviewed immutable image"))
	}
	if spec.Runtime.Image != identity.RuntimeImage ||
		spec.Runtime.RuntimeCatalogRevision != identity.RuntimeCatalogRevision ||
		!digestImage(spec.Runtime.Image, RuntimeImageName) ||
		!shaPattern.MatchString(spec.Runtime.RuntimeCatalogRevision) {
		return terminal("runtime_identity_mismatch", errors.New("runtime image is not the reviewed immutable image"))
	}
	if !minecraftPattern.MatchString(spec.Compatibility.MinecraftVersion) ||
		!allowedLoader(spec.Compatibility.Loader.Kind) ||
		!versionPattern.MatchString(spec.Compatibility.Loader.Version) ||
		!versionPattern.MatchString(spec.Compatibility.Java.Component) ||
		spec.Compatibility.Java.Major < 8 || spec.Compatibility.Java.Major > 99 {
		return terminal("invalid_build_spec", errors.New("build compatibility is invalid"))
	}
	previousFile := ""
	targets := map[string]struct{}{}
	sources := map[string]struct{}{}
	var packageBytes int64
	for _, file := range spec.Files {
		order := file.TargetPath + "\x00" + file.Path
		if previousFile != "" && order <= previousFile {
			return terminal("invalid_build_spec", errors.New("build files are not canonically sorted"))
		}
		previousFile = order
		if !safePath(file.Path) || !safeOutputPath(file.TargetPath) ||
			!shaPattern.MatchString(file.SHA256) || file.SizeBytes < 0 ||
			file.SizeBytes > MaxFileBytes {
			return terminal("invalid_build_spec", errors.New("build file identity is invalid"))
		}
		sourceKey := collisionKey(file.Path)
		if _, exists := sources[sourceKey]; exists {
			return terminal("invalid_build_spec", errors.New("build source paths collide"))
		}
		sources[sourceKey] = struct{}{}
		key := collisionKey(file.TargetPath)
		if _, exists := targets[key]; exists {
			return terminal("invalid_build_spec", errors.New("build file targets collide"))
		}
		targets[key] = struct{}{}
		if file.SizeBytes > MaxPackageInputBytes-packageBytes {
			return terminal("invalid_build_spec", errors.New("build package input exceeds aggregate limit"))
		}
		packageBytes += file.SizeBytes
	}
	previousDependency := ""
	ids := map[string]struct{}{}
	for _, dependency := range spec.Dependencies {
		order := dependency.Kind + "\x00" + dependency.ID + "\x00" + dependency.TargetPath
		if previousDependency != "" && order <= previousDependency {
			return terminal("invalid_build_spec", errors.New("build dependencies are not canonically sorted"))
		}
		previousDependency = order
		if !allowedDependencyKind(dependency.Kind) || !validDependencyID(dependency) ||
			!safeOutputPath(dependency.TargetPath) || !shaPattern.MatchString(dependency.SHA256) ||
			dependency.SizeBytes < 1 || dependency.SizeBytes > MaxDependencyBytes {
			return terminal("invalid_build_spec", errors.New("build dependency identity is invalid"))
		}
		if _, exists := ids[dependency.ID]; exists {
			return terminal("invalid_build_spec", errors.New("duplicate build dependency identity"))
		}
		ids[dependency.ID] = struct{}{}
		key := collisionKey(dependency.TargetPath)
		if _, exists := targets[key]; exists {
			return terminal("invalid_build_spec", errors.New("build dependency target collides"))
		}
		targets[key] = struct{}{}
		if dependency.Kind == "mod" {
			if dependency.SizeBytes > MaxPackageInputBytes-packageBytes {
				return terminal("invalid_build_spec", errors.New("build package input exceeds aggregate limit"))
			}
			packageBytes += dependency.SizeBytes
		}
	}
	return nil
}

func BuildKey(spec controlplane.BuildSpecV1) (string, error) {
	canonical, err := CanonicalJSON(spec)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(buildKeyDomain))
	_, _ = hash.Write(canonical)
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func CanonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var generic any
	if err := decoder.Decode(&generic); err != nil {
		return nil, err
	}
	var output bytes.Buffer
	if err := writeCanonical(&output, generic); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func writeCanonical(output *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		output.WriteString("null")
	case bool:
		if typed {
			output.WriteString("true")
		} else {
			output.WriteString("false")
		}
	case string:
		if err := writeJSONString(output, typed); err != nil {
			return err
		}
	case json.Number:
		if strings.ContainsAny(typed.String(), ".eE") {
			return errors.New("canonical build JSON cannot contain floating-point values")
		}
		if _, err := strconv.ParseInt(typed.String(), 10, 64); err != nil {
			return err
		}
		output.WriteString(typed.String())
	case []any:
		output.WriteByte('[')
		for index, entry := range typed {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeCanonical(output, entry); err != nil {
				return err
			}
		}
		output.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		output.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				output.WriteByte(',')
			}
			if err := writeJSONString(output, key); err != nil {
				return err
			}
			output.WriteByte(':')
			if err := writeCanonical(output, typed[key]); err != nil {
				return err
			}
		}
		output.WriteByte('}')
	default:
		return fmt.Errorf("unsupported canonical JSON value %T", value)
	}
	return nil
}

func writeJSONString(output *bytes.Buffer, value string) error {
	if !utf8.ValidString(value) {
		return errors.New("canonical JSON string is not valid UTF-8")
	}
	output.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"', '\\':
			output.WriteByte('\\')
			output.WriteRune(character)
		case '\b':
			output.WriteString(`\b`)
		case '\f':
			output.WriteString(`\f`)
		case '\n':
			output.WriteString(`\n`)
		case '\r':
			output.WriteString(`\r`)
		case '\t':
			output.WriteString(`\t`)
		case '\u2028':
			output.WriteString(`\u2028`)
		case '\u2029':
			output.WriteString(`\u2029`)
		default:
			if character < 0x20 {
				fmt.Fprintf(output, `\u%04x`, character)
			} else {
				output.WriteRune(character)
			}
		}
	}
	output.WriteByte('"')
	return nil
}

func digestImage(value, name string) bool {
	return strings.HasPrefix(value, name+"@sha256:") &&
		len(value) == len(name)+len("@sha256:")+64 &&
		shaPattern.MatchString(strings.TrimPrefix(value, name+"@sha256:"))
}

func validSourceKey(key, accountID, serviceID string) bool {
	prefix := "shared-hosting/" + accountID + "/" + serviceID + "/build-inputs/"
	return strings.HasPrefix(key, prefix) && len(key) > len(prefix) &&
		!strings.Contains(key, "\\") && !strings.Contains(key, "..")
}

func safePath(value string) bool {
	return value != "" && len(value) <= 1024 && !strings.Contains(value, "\\") &&
		!strings.HasPrefix(value, "/") && !strings.Contains(value, "\x00") &&
		allSafeParts(value)
}

func safeOutputPath(value string) bool {
	if !safePath(value) || len(value) > 512 {
		return false
	}
	lower := strings.ToLower(value)
	return !strings.HasPrefix(lower, ".xmcl/") &&
		!strings.HasPrefix(lower, "world/") &&
		!strings.HasSuffix(lower, ".sh") &&
		!strings.HasSuffix(lower, ".bat") &&
		!strings.HasSuffix(lower, ".cmd")
}

func allSafeParts(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func allowedDependencyKind(value string) bool {
	switch value {
	case "toolchain", "minecraft", "mapping", "library", "mod":
		return true
	default:
		return false
	}
}

func collisionKey(value string) string {
	return strings.ToLower(norm.NFKC.String(value))
}

func allowedLoader(value string) bool {
	switch value {
	case "fabric", "forge", "neoforge", "quilt", "vanilla":
		return true
	default:
		return false
	}
}

func validDependencyID(dependency controlplane.BuildDependency) bool {
	if !idPattern.MatchString(dependency.ID) {
		return false
	}
	if dependency.Kind == "mod" {
		return strings.HasPrefix(dependency.ID, "modrinth:") ||
			strings.HasPrefix(dependency.ID, "curseforge:")
	}
	return !strings.Contains(dependency.ID, "/") && strings.Count(dependency.ID, ":") >= 2
}
