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
	"strconv"
	"strings"
)

var (
	sha256Pattern    = regexp.MustCompile(`^[a-f0-9]{64}$`)
	minecraftPattern = regexp.MustCompile(`^1\.[0-9]+\.[0-9]+$`)
	loaderPattern    = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,127}$`)
)

const maxDescriptorBytes int64 = 1 << 20

type Descriptor struct {
	SchemaVersion    int    `json:"schemaVersion"`
	MinecraftVersion string `json:"minecraftVersion"`
	JavaMajor        int    `json:"javaMajor"`
	Loader           Loader `json:"loader"`
	Launch           Launch `json:"launch"`
	ContentSHA256    string `json:"contentSha256"`
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

func BundledJava(major int) (string, error) {
	switch major {
	case 8, 16, 17, 21:
		return fmt.Sprintf("/opt/xmcl/jre/%d/bin/java", major), nil
	default:
		return "", errors.New("runtime descriptor requests an unsupported Java major")
	}
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
	if _, err := BundledJava(value.JavaMajor); err != nil {
		return err
	}
	if !loaderPattern.MatchString(value.Loader.Version) {
		return errors.New("runtime descriptor has an invalid loader version")
	}
	expectedJava, err := JavaForCompatibility(
		value.MinecraftVersion,
		value.Loader.Kind,
		value.Loader.Version,
	)
	if err != nil {
		return err
	}
	if value.JavaMajor != expectedJava {
		return errors.New("runtime descriptor Java major is incompatible with the selected Minecraft and loader metadata")
	}
	if value.Launch.Kind != "generated-server-launcher" ||
		value.Launch.Path != ".xmcl/launch.sh" || len(value.Launch.Arguments) != 0 {
		return errors.New("runtime descriptor has an untrusted launch request")
	}
	return nil
}

// JavaForCompatibility mirrors the control-plane descriptor contract. It uses
// the exact Minecraft release plus loader metadata rather than accepting an
// arbitrary Java major from the content archive.
func JavaForCompatibility(minecraftVersion, loaderKind, loaderVersion string) (int, error) {
	match := minecraftPattern.FindStringSubmatch(minecraftVersion)
	if match == nil || !loaderPattern.MatchString(loaderVersion) {
		return 0, errors.New("runtime descriptor has unsupported compatibility metadata")
	}
	minor, err := strconv.Atoi(strings.Split(match[0], ".")[1])
	if err != nil {
		return 0, errors.New("runtime descriptor has unsupported Minecraft version")
	}
	patch, err := strconv.Atoi(strings.Split(match[0], ".")[2])
	if err != nil {
		return 0, errors.New("runtime descriptor has unsupported Minecraft version")
	}

	switch loaderKind {
	case "forge", "fabric", "quilt":
	case "neoforge":
		// NeoForge starts at Minecraft 1.20.2. Rejecting an invented legacy
		// mapping is safer than treating it as Forge.
		if minor < 20 || (minor == 20 && patch < 2) {
			return 0, errors.New("runtime descriptor has unsupported NeoForge compatibility")
		}
	default:
		return 0, errors.New("runtime descriptor has an unsupported loader")
	}

	switch {
	case minor <= 16:
		return 8, nil
	case minor == 17:
		return 16, nil
	case minor < 20 || (minor == 20 && patch <= 4):
		return 17, nil
	default:
		return 21, nil
	}
}
