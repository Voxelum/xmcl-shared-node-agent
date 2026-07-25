// Package runtime validates the compiler-produced runtime contract before a
// customer container is created. It has no downloader, image selection, or
// shell-expression input.
package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var (
	sha256Pattern          = regexp.MustCompile(`^[a-f0-9]{64}$`)
	minecraftPattern       = regexp.MustCompile(`^(?:1\.(?:0|[1-9][0-9]{0,2})\.(?:0|[1-9][0-9]{0,2})|[1-9][0-9]{1,3}\.(?:0|[1-9][0-9]{0,2})(?:\.(?:0|[1-9][0-9]{0,2}))?)$`)
	legacyMinecraftPattern = regexp.MustCompile(`^1\.(0|[1-9][0-9]{0,2})\.(0|[1-9][0-9]{0,2})$`)
	loaderPattern          = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,127}$`)
)

const maxDescriptorBytes int64 = 1 << 20

type Descriptor struct {
	SchemaVersion    int     `json:"schemaVersion"`
	MinecraftVersion string  `json:"minecraftVersion"`
	Java             Java    `json:"java"`
	RuntimeCatalog   Catalog `json:"runtimeCatalog"`
	Loader           Loader  `json:"loader"`
	Launch           Launch  `json:"launch"`
	ContentSHA256    string  `json:"contentSha256"`
}

type Java struct {
	Component string `json:"component"`
	Major     int    `json:"major"`
}

type Catalog struct {
	SHA256 string `json:"sha256"`
}

type Loader struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
}

type Launch struct {
	Kind      string   `json:"kind"`
	Path      string   `json:"path"`
	Arguments []string `json:"arguments"`
}

func BundledJava(java Java) (string, error) {
	if !catalogAllowsJava(java) {
		return "", errors.New("runtime descriptor requests an unsupported Java major")
	}
	return fmt.Sprintf("/opt/xmcl/jre/%d/bin/java", java.Major), nil
}

// ValidateWorkspace rejects every dynamic launch field. expectedContentSHA is
// provided by the command-selected immutable blob and binds runtime.json to the
// archive the agent already hashed while restoring.
func ValidateWorkspace(root, expectedContentSHA string) (Descriptor, error) {
	if expectedContentSHA != "" && !sha256Pattern.MatchString(expectedContentSHA) {
		return Descriptor{}, errors.New("assigned runtime content hash is invalid")
	}
	path := filepath.Join(root, ".xmcl", "runtime.json")
	info, err := os.Lstat(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("inspect runtime descriptor: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxDescriptorBytes {
		return Descriptor{}, errors.New("runtime descriptor is not a safe bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return Descriptor{}, fmt.Errorf("open runtime descriptor: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxDescriptorBytes))
	decoder.DisallowUnknownFields()
	var descriptor Descriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("decode runtime descriptor: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Descriptor{}, errors.New("runtime descriptor has trailing data")
	}
	if err := validate(descriptor, expectedContentSHA); err != nil {
		return Descriptor{}, err
	}
	launcher := filepath.Join(root, ".xmcl", "launch.sh")
	info, err = os.Lstat(launcher)
	if err != nil {
		return Descriptor{}, fmt.Errorf("inspect generated launcher: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return Descriptor{}, errors.New("generated launcher is not a safe regular immutable file")
	}
	return descriptor, nil
}

func validate(value Descriptor, expectedContentSHA string) error {
	if value.SchemaVersion != 1 || !minecraftPattern.MatchString(value.MinecraftVersion) ||
		!sha256Pattern.MatchString(value.ContentSHA256) {
		return errors.New("runtime descriptor has invalid canonical fields")
	}
	if expectedContentSHA != "" && value.ContentSHA256 != expectedContentSHA {
		return errors.New("runtime descriptor content hash does not match restored content")
	}
	if value.RuntimeCatalog.SHA256 != reviewedCatalog.SHA256 {
		return errors.New("runtime descriptor catalog revision does not match the reviewed catalog")
	}
	if _, err := BundledJava(value.Java); err != nil {
		return err
	}
	if !loaderPattern.MatchString(value.Loader.Version) {
		return errors.New("runtime descriptor has an invalid loader version")
	}
	if !validLoader(value.MinecraftVersion, value.Loader.Kind) {
		return errors.New("runtime descriptor has unsupported loader compatibility")
	}
	if !catalogAllowsToolchain(value.MinecraftVersion, value.Loader, value.Java) {
		return errors.New("runtime descriptor has unsupported reviewed toolchain")
	}
	if value.Launch.Kind != "generated-server-launcher" ||
		value.Launch.Path != ".xmcl/launch.sh" || len(value.Launch.Arguments) != 0 {
		return errors.New("runtime descriptor has an untrusted launch request")
	}
	return nil
}

func validLoader(minecraftVersion, loaderKind string) bool {
	if !validMinecraftVersion(minecraftVersion) {
		return false
	}
	switch loaderKind {
	case "forge", "fabric", "quilt":
		return true
	case "neoforge":
		match := legacyMinecraftPattern.FindStringSubmatch(minecraftVersion)
		if match == nil {
			return true
		}
		var minor, patch int
		if _, err := fmt.Sscanf(minecraftVersion, "1.%d.%d", &minor, &patch); err != nil {
			return false
		}
		return minor > 20 || (minor == 20 && patch >= 2)
	default:
		return false
	}
}

func validMinecraftVersion(value string) bool {
	return minecraftPattern.MatchString(value)
}
