package otlpconfig

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func ValidateEndpoint(raw string) error {
	endpoint, err := url.Parse(raw)
	if err != nil || endpoint.Host == "" || endpoint.Hostname() == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		endpoint.Opaque != "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be an absolute HTTPS URL or loopback HTTP URL")
	}
	if port := endpoint.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT port is invalid")
		}
	}
	if endpoint.Scheme == "https" {
		return nil
	}
	host := endpoint.Hostname()
	if endpoint.Scheme != "http" ||
		(host != "localhost" && net.ParseIP(host) == nil) ||
		(host != "localhost" && !net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT must be an absolute HTTPS URL or loopback HTTP URL")
	}
	return nil
}

func ParseHeaders(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 4096 || strings.ContainsAny(raw, "\r\n") {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS is invalid")
	}
	headers := make(map[string]string)
	for value := range strings.SplitSeq(raw, ",") {
		key, headerValue, found := strings.Cut(value, "=")
		decodedKey, keyErr := url.QueryUnescape(key)
		decodedValue, valueErr := url.QueryUnescape(headerValue)
		if !found || keyErr != nil || valueErr != nil ||
			!validHeaderName(decodedKey) || !validHeaderValue(decodedValue) {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS is invalid")
		}
		headers[decodedKey] = decodedValue
	}
	return headers, nil
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		character := value[i]
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func validHeaderValue(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] > 0x7e {
			return false
		}
	}
	return true
}
