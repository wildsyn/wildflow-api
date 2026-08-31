package common

import (
	"fmt"
	"net"
	"strings"
)

// ValidateIPCIDRList checks a newline/comma separated list of IP addresses or
// CIDR ranges (IPv4 and IPv6). Each non-empty entry must be a valid IP or
// CIDR; the first invalid entry is reported with its 1-based line number so
// users can locate and fix a misconfiguration instead of being silently
// locked out. Empty or whitespace-only lists are valid and mean "no limit".
func ValidateIPCIDRList(list string) error {
	_, err := normalizeIPCIDRList(list)
	return err
}

// NormalizeIPCIDRList validates and returns the cleaned entries of a
// newline/comma separated IP/CIDR list.
func NormalizeIPCIDRList(list string) ([]string, error) {
	return normalizeIPCIDRList(list)
}

func normalizeIPCIDRList(list string) ([]string, error) {
	entries := make([]string, 0)
	if strings.TrimSpace(list) == "" {
		return entries, nil
	}
	normalized := strings.ReplaceAll(list, " ", "")
	normalized = strings.ReplaceAll(normalized, ",", "\n")
	for i, entry := range strings.Split(normalized, "\n") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if err := validateIPOrCIDREntry(entry); err != nil {
			return nil, fmt.Errorf("entry %d %q: %w", i+1, entry, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func validateIPOrCIDREntry(entry string) error {
	if strings.Contains(entry, "/") {
		_, _, err := net.ParseCIDR(entry)
		if err != nil {
			return fmt.Errorf("invalid CIDR: %w", err)
		}
		return nil
	}
	if net.ParseIP(entry) == nil {
		return fmt.Errorf("invalid IP address")
	}
	return nil
}
