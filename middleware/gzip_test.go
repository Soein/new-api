package middleware

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func encodeZstdFrame(t *testing.T, payload []byte, options ...zstd.EOption) []byte {
	t.Helper()

	encoder, err := zstd.NewWriter(nil, options...)
	require.NoError(t, err)
	compressed := encoder.EncodeAll(payload, nil)
	require.NoError(t, encoder.Close())
	return compressed
}

func performZstdRequest(t *testing.T, compressed []byte) *httptest.ResponseRecorder {
	t.Helper()

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/zstd", DecompressRequestMiddleware(), func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.Status(http.StatusBadRequest)
			return
		}
		c.Data(http.StatusOK, "application/octet-stream", body)
	})

	request := httptest.NewRequest(http.MethodPost, "/zstd", bytes.NewReader(compressed))
	request.Header.Set("Content-Encoding", "zstd")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func useOneMegabyteRequestLimit(t *testing.T) {
	t.Helper()

	previous := constant.MaxRequestBodyMB
	constant.MaxRequestBodyMB = 1
	t.Cleanup(func() {
		constant.MaxRequestBodyMB = previous
	})
}

func TestDecompressRequestMiddlewareAcceptsZstdBody(t *testing.T) {
	useOneMegabyteRequestLimit(t)
	payload := []byte(`{"message":"hello"}`)
	compressed := encodeZstdFrame(t, payload, zstd.WithEncoderConcurrency(1))

	response := performZstdRequest(t, compressed)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, payload, response.Body.Bytes())
}

func TestDecompressRequestMiddlewareRejectsZstdWindowOverRequestLimit(t *testing.T) {
	useOneMegabyteRequestLimit(t)

	compressed, err := (&zstd.Header{WindowSize: 2 << 20}).AppendTo(nil)
	require.NoError(t, err)
	// Last, empty raw block: this completes a valid frame while keeping the
	// compressed request small and its declared decoder window observable.
	compressed = append(compressed, 0x01, 0x00, 0x00)

	response := performZstdRequest(t, compressed)

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Empty(t, response.Body.Bytes())
}

func TestDecompressRequestMiddlewareRejectsZstdMemoryOverRequestLimit(t *testing.T) {
	useOneMegabyteRequestLimit(t)
	payload := bytes.Repeat([]byte("a"), (1<<20)+1)
	compressed := encodeZstdFrame(
		t,
		payload,
		zstd.WithEncoderConcurrency(1),
		zstd.WithSingleSegment(true),
	)

	response := performZstdRequest(t, compressed)

	require.Equal(t, http.StatusBadRequest, response.Code)
	require.Empty(t, response.Body.Bytes())
}
