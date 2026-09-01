package common

import (
	"fmt"
	"net"
	"strings"
)

// SplitIPCIDRList splits a newline/comma separated list into its non-empty
// entries. Write-time validation (NormalizeIPCIDRList) and request-time
// enforcement (Token.GetIpLimits) must both go through this function so the
// stored allow_ips string and the enforced list can never disagree on what
// separates entries.
func SplitIPCIDRList(list string) []string {
	if strings.TrimSpace(list) == "" {
		return nil
	}
	normalized := strings.ReplaceAll(list, " ", "")
	normalized = strings.ReplaceAll(normalized, ",", "\n")
	entries := make([]string, 0)
	for _, entry := range strings.Split(normalized, "\n") {
		entry = strings.TrimSpace(entry)
		if entry != "" {
			entries = append(entries, entry)
		}
	}
	return entries
}

// IsValidIPOrCIDR reports whether one entry is a valid IP address or CIDR
// range (IPv4 and IPv6).
func IsValidIPOrCIDR(entry string) bool {
	if strings.Contains(entry, "/") {
		_, _, err := net.ParseCIDR(entry)
		return err == nil
	}
	return net.ParseIP(entry) != nil
}

// ValidateIPCIDRList checks a newline/comma separated list of IP addresses or
// CIDR ranges (IPv4 and IPv6). Each non-empty entry must be a valid IP or
// CIDR; the first invalid entry is reported with its 1-based position so
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
	entries := SplitIPCIDRList(list)
	for i, entry := range entries {
		if err := validateIPOrCIDREntry(entry); err != nil {
			return nil, fmt.Errorf("entry %d %q: %w", i+1, entry, err)
		}
	}
	return entries, nil
}

func validateIPOrCIDREntry(entry string) error {
	if !IsValidIPOrCIDR(entry) {
		if strings.Contains(entry, "/") {
			_, _, err := net.ParseCIDR(entry)
			return fmt.Errorf("invalid CIDR: %w", err)
		}
		return fmt.Errorf("invalid IP address")
	}
	return nil
}
