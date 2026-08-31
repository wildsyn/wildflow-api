package common

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateIPCIDRListAcceptsEmptyAndBlankLists(t *testing.T) {
	assert.NoError(t, ValidateIPCIDRList(""))
	assert.NoError(t, ValidateIPCIDRList("   \n  \n"))
}

func TestValidateIPCIDRListAcceptsValidIPv4(t *testing.T) {
	entries, err := NormalizeIPCIDRList("192.168.1.1\n10.0.0.0/8")
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1", "10.0.0.0/8"}, entries)
}

func TestValidateIPCIDRListAcceptsValidIPv6(t *testing.T) {
	entries, err := NormalizeIPCIDRList("2001:db8::1\nfe80::/10")
	require.NoError(t, err)
	assert.Equal(t, []string{"2001:db8::1", "fe80::/10"}, entries)
}

func TestValidateIPCIDRListAcceptsCommaSeparationAndSpaces(t *testing.T) {
	entries, err := NormalizeIPCIDRList(" 192.168.1.1 , 10.0.0.0/8 \n fd00::/8 ")
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1", "10.0.0.0/8", "fd00::/8"}, entries)
}

func TestValidateIPCIDRListRejectsInvalidEntries(t *testing.T) {
	invalidLists := map[string]string{
		"not-an-ip":     "not-an-ip",
		"truncated":     "192.168.1",
		"bad-cidr":      "10.0.0.0/33",
		"ipv6-cidr-bad": "fe80::/200",
		"host-port":     "192.168.1.1:8080",
	}
	for name, list := range invalidLists {
		t.Run(name, func(t *testing.T) {
			err := ValidateIPCIDRList(list)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "entry 1")
		})
	}
}

func TestValidateIPCIDRListReportsOffendingEntryLine(t *testing.T) {
	err := ValidateIPCIDRList("10.0.0.1\n10.0.0.2\nnot-an-ip\n10.0.0.3")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "entry 3")
	assert.Contains(t, err.Error(), "not-an-ip")
}

func TestValidateIPCIDRListRejectsInvalidEntryMixedWithValid(t *testing.T) {
	err := ValidateIPCIDRList("10.0.0.1,,10.0.0.broken")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "10.0.0.broken")
}
