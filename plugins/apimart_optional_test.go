package plugins

import (
	"context"
	"os"
	"testing"

	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/stretchr/testify/require"
)

func TestAPIMartOptionalPluginContract(t *testing.T) {
	source, err := os.ReadFile("optional/apimart/plugin.js")
	require.NoError(t, err)
	fixture, err := os.ReadFile("optional/apimart/fixtures.json")
	require.NoError(t, err)
	report, err := jsplugin.ReplayFixture(context.Background(), string(source), fixture)
	require.NoError(t, err)
	require.NotZero(t, report.Total)
	require.Equal(t, report.Total, report.Passed, "%+v", report)
}
