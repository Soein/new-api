package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// A ConvertImageRequest failure (e.g. an out-of-range count/size field caught
// by an adaptor) is a client request-validation error, not a server fault.
// It must surface as 4xx so it doesn't read as a backend outage in
// monitoring/alerting and doesn't get retried as if it were transient.
func TestImageHelper_ConvertRequestValidationErrorIsBadRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeSiliconFlow)

	imageReq := &dto.ImageRequest{
		Model: "test-model",
		Extra: map[string]json.RawMessage{
			// Bypasses the bounded top-level n; SiliconFlow's
			// ConvertImageRequest must reject this.
			"batch_size": json.RawMessage("999999"),
		},
	}
	info := relaycommon.GenRelayInfoImage(c, imageReq)

	newAPIError := ImageHelper(c, info)

	require.NotNil(t, newAPIError)
	require.Equal(t, http.StatusBadRequest, newAPIError.StatusCode)
}
