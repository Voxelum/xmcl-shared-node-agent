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

type catalog struct {
	SchemaVersion int           `json:"schemaVersion"`
	SHA256        string        `json:"sha256"`
	Requirements  []catalogJava `json:"requirements"`
	Runtimes      []catalogJava `json:"runtimes"`
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
	return value
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
