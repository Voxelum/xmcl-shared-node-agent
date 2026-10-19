package bootstraprunner

import (
	"encoding/json"
	"errors"
	"io"
)

// Token-level exact schema: duplicate, case-folded, unknown and null fields fail.
func DecodeTestTOFUAuthorization(reader io.Reader) (TestTOFUAuthorization, error) {
	var g TestTOFUAuthorization
	d := json.NewDecoder(io.LimitReader(reader, 8193))
	t, e := d.Token()
	if e != nil || t != json.Delim('{') {
		return g, errors.New("invalid authorization object")
	}
	values := map[string]any{}
	for d.More() {
		k, e := d.Token()
		if e != nil {
			return g, e
		}
		key, ok := k.(string)
		if !ok {
			return g, errors.New("invalid authorization key")
		}
		if _, ok := values[key]; ok {
			return g, errors.New("duplicate authorization key")
		}
		v, e := d.Token()
		if e != nil {
			return g, e
		}
		switch key {
		case "authorizationId", "operationId", "jobId", "instanceId", "address", "allocationCreatedAt", "firstPinExpiresAt":
			if _, ok := v.(string); !ok {
				return g, errors.New("invalid authorization string")
			}
		case "emptyIsolatedTestVM", "acceptFirstConnectionMitmRisk":
			if _, ok := v.(bool); !ok {
				return g, errors.New("invalid authorization flag")
			}
		default:
			return g, errors.New("unknown authorization key")
		}
		values[key] = v
	}
	t, e = d.Token()
	if e != nil || t != json.Delim('}') || len(values) != 9 {
		return g, errors.New("incomplete authorization")
	}
	if _, e = d.Token(); e != io.EOF {
		return g, errors.New("trailing authorization JSON")
	}
	b, _ := json.Marshal(values)
	e = json.Unmarshal(b, &g)
	return g, e
}
