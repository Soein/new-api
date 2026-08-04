package billing_setting

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateBillingExprJSONAcceptsPerImageV2(t *testing.T) {
	err := ValidateBillingExprJSON(`{
		"image-model":"v2:tier(\"image\", per_image(0.04)) * rule(\"quality\", param(\"quality\") == \"high\", 2)",
		"text-model":"tier(\"base\", p * 2 + c * 8)"
	}`)

	require.NoError(t, err)
}

func TestValidateBillingExprJSONRejectsUnsafeExpressions(t *testing.T) {
	tests := []struct {
		name  string
		value string
	}{
		{name: "invalid json", value: `{`},
		{name: "empty expression", value: `{"model":""}`},
		{name: "compile error", value: `{"model":"invalid +-+ syntax"}`},
		{name: "negative image price", value: `{"model":"v2:per_image(-0.04)"}`},
		{name: "non-positive request multiplier", value: `{"model":"v2:rule(\"bad\", true, 0)"}`},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateBillingExprJSON(test.value)
			require.Error(t, err)
			assert.NotEmpty(t, err.Error())
		})
	}
}
