package runtime

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed catalog.json
var catalogJSON []byte

type catalogJava struct {
	Component string `json:"component"`
	Major     int    `json:"major"`
}

type catalogToolchain struct {
	MinecraftVersion string      `json:"minecraftVersion"`
	Loader           Loader      `json:"loader"`
	Java             catalogJava `json:"java"`
}

type catalog struct {
	SchemaVersion int                `json:"schemaVersion"`
	SHA256        string             `json:"sha256"`
	Requirements  []catalogJava      `json:"requirements"`
	Runtimes      []catalogJava      `json:"runtimes"`
	Toolchains    []catalogToolchain `json:"toolchains"`
}

var reviewedCatalog = mustLoadCatalog()

func mustLoadCatalog() catalog {
	var value catalog
	if err := json.Unmarshal(catalogJSON, &value); err != nil {
		panic(fmt.Sprintf("decode compiled runtime catalog: %v", err))
	}
	if value.SchemaVersion != 1 || !sha256Pattern.MatchString(value.SHA256) {
		panic("compiled runtime catalog has an invalid revision")
	}
	for _, requirement := range value.Requirements {
		if requirement.Component == "" || requirement.Major < 1 {
			panic("compiled runtime catalog has an invalid requirement")
		}
	}
	for _, runtime := range value.Runtimes {
		if runtime.Component == "" || runtime.Major < 1 {
			panic("compiled runtime catalog has an invalid runtime")
		}
	}
	for _, toolchain := range value.Toolchains {
		if !validMinecraftVersion(toolchain.MinecraftVersion) ||
			!loaderPattern.MatchString(toolchain.Loader.Version) ||
			!validLoader(toolchain.MinecraftVersion, toolchain.Loader.Kind) ||
			!catalogIncludesJava(value, toolchain.Java) {
			panic("compiled runtime catalog has an invalid toolchain")
		}
	}
	return value
}

func catalogIncludesJava(value catalog, java catalogJava) bool {
	requirement := false
	for _, item := range value.Requirements {
		if item == java {
			requirement = true
			break
		}
	}
	if !requirement {
		return false
	}
	for _, item := range value.Runtimes {
		if item.Major == java.Major {
			return true
		}
	}
	return false
}

func catalogAllowsJava(value Java) bool {
	requirement := false
	for _, item := range reviewedCatalog.Requirements {
		if item.Component == value.Component && item.Major == value.Major {
			requirement = true
			break
		}
	}
	if !requirement {
		return false
	}
	for _, item := range reviewedCatalog.Runtimes {
		if item.Major == value.Major {
			return true
		}
	}
	return false
}

func catalogAllowsToolchain(minecraftVersion string, loader Loader, java Java) bool {
	for _, item := range reviewedCatalog.Toolchains {
		if item.MinecraftVersion == minecraftVersion &&
			item.Loader.Kind == loader.Kind &&
			item.Loader.Version == loader.Version &&
			item.Java.Component == java.Component &&
			item.Java.Major == java.Major {
			return true
		}
	}
	return false
}
