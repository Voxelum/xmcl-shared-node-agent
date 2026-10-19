package bootstraprunner

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"
)

//go:embed testdata/ts-digest.json
var tsDigestVector []byte

func TestTSGoReconcileDigestParity(t *testing.T) {
	var v struct {
		Request Request `json:"request"`
		SHA256  string  `json:"sha256"`
	}
	if e := json.Unmarshal(tsDigestVector, &v); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(v.Request)
	if e != nil {
		t.Fatal(e)
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(b))
	if actual != v.SHA256 {
		t.Fatalf("TS/Go digest mismatch %s != %s", v.SHA256, actual)
	}
}
