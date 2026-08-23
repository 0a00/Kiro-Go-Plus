package awsregion

import (
	"fmt"
	"strings"
)

// Normalize validates an AWS region before it is interpolated into a hostname.
func Normalize(value string) (string, error) {
	region := strings.ToLower(strings.TrimSpace(value))
	if len(region) < 5 || len(region) > 32 || !strings.Contains(region, "-") {
		return "", fmt.Errorf("invalid AWS region %q", region)
	}
	if region[0] < 'a' || region[0] > 'z' || strings.Contains(region, "--") {
		return "", fmt.Errorf("invalid AWS region %q", region)
	}
	for _, char := range region {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return "", fmt.Errorf("invalid AWS region %q", region)
		}
	}
	last := region[len(region)-1]
	if last < '0' || last > '9' {
		return "", fmt.Errorf("invalid AWS region %q", region)
	}
	return region, nil
}

func Valid(value string) bool {
	_, err := Normalize(value)
	return err == nil
}
