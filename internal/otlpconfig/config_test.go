package otlpconfig

import "testing"

func TestValidateEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"https://collector.example.test",
		"https://collector.example.test:4318/prefix",
		"http://127.0.0.1:4318",
		"http://[::1]:4318",
	} {
		if err := ValidateEndpoint(endpoint); err != nil {
			t.Fatalf("%q: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{
		"http://collector.example.test:4318",
		"https://:4318",
		"https://collector.example.test:bad",
		"https://user@collector.example.test",
		"https://collector.example.test?token=value",
	} {
		if err := ValidateEndpoint(endpoint); err == nil {
			t.Fatalf("%q was accepted", endpoint)
		}
	}
}

func TestParseHeaders(t *testing.T) {
	headers, err := ParseHeaders("authorization=Bearer%20value,x-node=node-1")
	if err != nil {
		t.Fatal(err)
	}
	if headers["authorization"] != "Bearer value" || headers["x-node"] != "node-1" {
		t.Fatalf("headers = %#v", headers)
	}
	for _, value := range []string{
		"authorization",
		"bad%zz=value",
		"bad%20name=value",
		"authorization=value%0ainjected",
	} {
		if _, err := ParseHeaders(value); err == nil {
			t.Fatalf("%q was accepted", value)
		}
	}
}
