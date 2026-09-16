package helper

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
	if constant.StreamingTimeout == 0 {
		constant.StreamingTimeout = 30
	}
}

func setupStreamTest(t *testing.T, body io.Reader) (*gin.Context, *http.Response, *relaycommon.RelayInfo) {
	t.Helper()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		Body: io.NopCloser(body),
	}

	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	return c, resp, info
}

func buildSSEBody(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d,\"choices\":[{\"delta\":{\"content\":\"token_%d\"}}]}\n", i, i)
	}
	b.WriteString("data: [DONE]\n")
	return b.String()
}

// ---------- Basic correctness ----------

func TestStreamScannerHandler_NilInputs(t *testing.T) {
	t.Parallel()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	StreamScannerHandler(c, nil, info, func(data string, sr *StreamResult) {})
	StreamScannerHandler(c, &http.Response{Body: io.NopCloser(strings.NewReader(""))}, info, nil)
}

func TestNewStreamScanner_AllowsLargeStreamLine(t *testing.T) {
	oldBufferMB := constant.StreamScannerMaxBufferMB
	constant.StreamScannerMaxBufferMB = 1
	t.Cleanup(func() {
		constant.StreamScannerMaxBufferMB = oldBufferMB
	})

	payload := strings.Repeat("x", 128<<10)
	scanner := NewStreamScanner(strings.NewReader("data: " + payload + "\n"))
	scanner.Split(bufio.ScanLines)

	require.True(t, scanner.Scan())
	assert.Equal(t, "data: "+payload, scanner.Text())
	require.NoError(t, scanner.Err())
}

func TestStreamScannerHandler_EmptyBody(t *testing.T) {
	t.Parallel()

	c, resp, info := setupStreamTest(t, strings.NewReader(""))

	var called atomic.Bool
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		called.Store(true)
	})

	assert.False(t, called.Load(), "handler should not be called for empty body")
}

func TestStreamScannerHandler_1000Chunks(t *testing.T) {
	t.Parallel()

	const numChunks = 1000
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(numChunks), count.Load())
	assert.Equal(t, numChunks, info.ReceivedResponseCount)
}

func TestStreamScannerHandler_OrderPreserved(t *testing.T) {
	t.Parallel()

	const numChunks = 500
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var mu sync.Mutex
	received := make([]string, 0, numChunks)

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		mu.Lock()
		received = append(received, data)
		mu.Unlock()
	})

	require.Equal(t, numChunks, len(received))
	for i := 0; i < numChunks; i++ {
		expected := fmt.Sprintf("{\"id\":%d,\"choices\":[{\"delta\":{\"content\":\"token_%d\"}}]}", i, i)
		assert.Equal(t, expected, received[i], "chunk %d out of order", i)
	}
}

func TestStreamScannerHandler_DoneStopsScanner(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(50) + "data: should_not_appear\n"
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(50), count.Load(), "data after [DONE] must not be processed")
}

func TestStreamScannerHandler_StopStopsStream(t *testing.T) {
	t.Parallel()

	const numChunks = 200
	body := buildSSEBody(numChunks)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	const stopAt int64 = 50
	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= stopAt {
			sr.Stop(fmt.Errorf("fatal at %d", n))
		}
	})

	assert.Equal(t, stopAt, count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
}

func TestStreamScannerHandler_SkipsNonDataLines(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	b.WriteString(": comment line\n")
	b.WriteString("event: message\n")
	b.WriteString("id: 12345\n")
	b.WriteString("retry: 5000\n")
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "data: payload_%d\n", i)
		b.WriteString(": interleaved comment\n")
	}
	b.WriteString("data: [DONE]\n")

	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
	})

	assert.Equal(t, int64(100), count.Load())
}

func TestStreamScannerHandler_DataWithExtraSpaces(t *testing.T) {
	t.Parallel()

	body := "data:   {\"trimmed\":true}  \ndata: [DONE]\n"
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var got string
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		got = data
	})

	assert.Equal(t, "{\"trimmed\":true}", got)
}

// TestStreamScannerHandler_ClientCancelAbortsUpstreamAndReturns pins the
// disconnect contract: when the client goes away, the handler must return
// promptly (all goroutines joined, so the gin.Context can never leak into a
// pooled reuse), the upstream body must be closed to stop token generation,
// and no data received after the disconnect may be processed or written.
func TestStreamScannerHandler_ClientCancelAbortsUpstreamAndReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pr, pw := io.Pipe()
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)

	resp := &http.Response{Body: pr}
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	var count atomic.Int64
	firstHandled := make(chan struct{})
	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
			_ = StringData(c, data)
			if data == "first" {
				close(firstHandled)
			}
		})
		close(done)
	}()

	_, err := fmt.Fprint(pw, "data: first\n")
	require.NoError(t, err)

	select {
	case <-firstHandled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first chunk")
	}

	cancel()

	// The handler must return without any further upstream input: cleanup
	// closes resp.Body, which unblocks the scanner goroutine.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after client disconnect")
	}

	// Upstream read side must be closed so the provider stops generating
	// (and billing) for a request nobody is listening to.
	_, err = fmt.Fprint(pw, "data: second\n")
	require.ErrorIs(t, err, io.ErrClosedPipe, "upstream body should be closed after client disconnect")

	assert.Equal(t, int64(1), count.Load(), "no chunk after disconnect should be processed")
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonClientGone, info.StreamStatus.EndReason)

	body := recorder.Body.String()
	assert.Contains(t, body, "first")
	assert.NotContains(t, body, "second")
}

// ---------- Ping tests ----------

func TestStreamScannerHandler_PingSentDuringSlowUpstream(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	common.OptionMapRWMutex.Lock()
	oldEnabled := setting.PingIntervalEnabled
	oldSeconds := setting.PingIntervalSeconds
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 1
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		setting.PingIntervalEnabled = oldEnabled
		setting.PingIntervalSeconds = oldSeconds
		common.OptionMapRWMutex.Unlock()
	})

	pr, pw := io.Pipe()
	fixtureCtx, cancel := context.WithCancel(context.Background())
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	go func() {
		defer writerWg.Done()
		defer pw.Close()
		for i := 0; i < 4; i++ {
			select {
			case <-time.After(400 * time.Millisecond):
				fmt.Fprintf(pw, "data: chunk_%d\n", i)
			case <-fixtureCtx.Done():
				return
			}
		}
		fmt.Fprint(pw, "data: [DONE]\n")
	}()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pr,
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	var count atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		_ = pr.Close()
		_ = pw.Close()
		<-done
		writerWg.Wait()
		t.Fatal("timed out waiting for stream to finish")
	}

	cancel()
	writerWg.Wait()

	assert.Equal(t, int64(4), count.Load())

	body := recorder.Body.String()
	pingCount := strings.Count(body, ": PING")
	assert.GreaterOrEqual(t, pingCount, 1,
		"expected at least 1 ping during slow stream with 1s interval; got %d", pingCount)
}

func TestStreamScannerHandler_PingDisabledByRelayInfo(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	common.OptionMapRWMutex.Lock()
	oldEnabled := setting.PingIntervalEnabled
	oldSeconds := setting.PingIntervalSeconds
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 1
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		setting.PingIntervalEnabled = oldEnabled
		setting.PingIntervalSeconds = oldSeconds
		common.OptionMapRWMutex.Unlock()
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(buildSSEBody(5))),
	}
	info := &relaycommon.RelayInfo{
		DisablePing: true,
		ChannelMeta: &relaycommon.ChannelMeta{},
	}

	var count atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
			count.Add(1)
		})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	assert.Equal(t, int64(5), count.Load())

	body := recorder.Body.String()
	pingCount := strings.Count(body, ": PING")
	assert.Equal(t, 0, pingCount, "pings should be disabled when DisablePing=true")
}

func TestStreamScannerHandler_ChannelOptInPing(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	common.OptionMapRWMutex.Lock()
	oldEnabled := setting.PingIntervalEnabled
	oldIDs := setting.StreamPingChannelIDs
	oldSec := setting.StreamPingIntervalSeconds
	setting.PingIntervalEnabled = false
	setting.StreamPingChannelIDs = []int{42}
	setting.StreamPingIntervalSeconds = 1
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		setting.PingIntervalEnabled = oldEnabled
		setting.StreamPingChannelIDs = oldIDs
		setting.StreamPingIntervalSeconds = oldSec
		common.OptionMapRWMutex.Unlock()
	})

	tests := []struct {
		name        string
		channelId   int
		statusCode  int
		disablePing bool
		expectPing  bool
	}{
		{
			name:        "PositiveOptIn",
			channelId:   42,
			statusCode:  http.StatusOK,
			disablePing: false,
			expectPing:  true,
		},
		{
			name:        "NonOptIn",
			channelId:   99,
			statusCode:  http.StatusOK,
			disablePing: false,
			expectPing:  false,
		},
		{
			name:        "Non2xxNoPing",
			channelId:   42,
			statusCode:  http.StatusUnauthorized,
			disablePing: false,
			expectPing:  false,
		},
		{
			name:        "DisablePingWins",
			channelId:   42,
			statusCode:  http.StatusOK,
			disablePing: true,
			expectPing:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

			pr, pw := io.Pipe()
			resp := &http.Response{
				StatusCode: tc.statusCode,
				Body:       pr,
			}
			info := &relaycommon.RelayInfo{
				DisablePing: tc.disablePing,
				ChannelMeta: &relaycommon.ChannelMeta{
					ChannelId: tc.channelId,
				},
			}

			fixtureCtx, cancel := context.WithCancel(context.Background())
			var writerWg sync.WaitGroup
			writerWg.Add(1)
			go func() {
				defer writerWg.Done()
				defer pw.Close()

				timer := time.NewTimer(1200 * time.Millisecond)
				defer timer.Stop()

				select {
				case <-timer.C:
					_, _ = fmt.Fprint(pw, "data: chunk\n\ndata: [DONE]\n\n")
				case <-fixtureCtx.Done():
					return
				}
			}()

			done := make(chan struct{})
			go func() {
				defer close(done)
				StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				cancel()
				_ = pr.Close()
				_ = pw.Close()
				<-done
				writerWg.Wait()
				t.Fatal("timed out waiting for stream handler")
			}

			cancel()
			writerWg.Wait()

			pingCount := strings.Count(recorder.Body.String(), ": PING")
			if tc.expectPing {
				assert.GreaterOrEqual(t, pingCount, 1, "expected at least 1 ping for opt-in channel")
			} else {
				assert.Equal(t, 0, pingCount, "pings should be suppressed")
			}
		})
	}
}

func TestGetStreamPingPolicy_BoundsAndFallback(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	common.OptionMapRWMutex.Lock()
	oldEnabled := setting.PingIntervalEnabled
	oldLegacySec := setting.PingIntervalSeconds
	oldIDs := setting.StreamPingChannelIDs
	oldSec := setting.StreamPingIntervalSeconds
	setting.PingIntervalEnabled = false
	setting.StreamPingChannelIDs = []int{99}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		setting.PingIntervalEnabled = oldEnabled
		setting.PingIntervalSeconds = oldLegacySec
		setting.StreamPingChannelIDs = oldIDs
		setting.StreamPingIntervalSeconds = oldSec
		common.OptionMapRWMutex.Unlock()
	})

	common.OptionMapRWMutex.Lock()
	setting.StreamPingIntervalSeconds = 0
	common.OptionMapRWMutex.Unlock()
	enabled, interval := operation_setting.GetStreamPingPolicy(99, false)
	assert.True(t, enabled)
	assert.Equal(t, 15*time.Second, interval)

	common.OptionMapRWMutex.Lock()
	setting.StreamPingIntervalSeconds = 999999
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(99, false)
	assert.True(t, enabled)
	assert.Equal(t, 15*time.Second, interval)

	common.OptionMapRWMutex.Lock()
	setting.StreamPingIntervalSeconds = 5
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(99, false)
	assert.True(t, enabled)
	assert.Equal(t, 5*time.Second, interval)

	enabled, _ = operation_setting.GetStreamPingPolicy(100, false)
	assert.False(t, enabled)

	// Legacy PingIntervalSeconds: preserve positive intervals (> 86400) on unselected channels.
	common.OptionMapRWMutex.Lock()
	setting.PingIntervalEnabled = true
	setting.PingIntervalSeconds = 86401
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(100, false)
	assert.True(t, enabled)
	assert.Equal(t, 86401*time.Second, interval)

	preEnabled, preInterval := operation_setting.GetPreHeaderPingPolicy(false)
	assert.True(t, preEnabled)
	assert.Equal(t, 86401*time.Second, preInterval)

	// Overflow-safe legacy values: values exceeding time.Duration fallback to 10s.
	common.OptionMapRWMutex.Lock()
	setting.PingIntervalSeconds = math.MaxInt
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(100, false)
	assert.True(t, enabled)
	if int64(math.MaxInt) > int64(math.MaxInt64/time.Second) {
		assert.Equal(t, 10*time.Second, interval)
	}
	preEnabled, preInterval = operation_setting.GetPreHeaderPingPolicy(false)
	assert.True(t, preEnabled)
	if int64(math.MaxInt) > int64(math.MaxInt64/time.Second) {
		assert.Equal(t, 10*time.Second, preInterval)
	}

	// Invalid / nonpositive legacy values fallback to 10s.
	common.OptionMapRWMutex.Lock()
	setting.PingIntervalSeconds = 0
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(100, false)
	assert.True(t, enabled)
	assert.Equal(t, 10*time.Second, interval)
	preEnabled, preInterval = operation_setting.GetPreHeaderPingPolicy(false)
	assert.True(t, preEnabled)
	assert.Equal(t, 10*time.Second, preInterval)

	common.OptionMapRWMutex.Lock()
	setting.PingIntervalSeconds = -10
	common.OptionMapRWMutex.Unlock()
	enabled, interval = operation_setting.GetStreamPingPolicy(100, false)
	assert.True(t, enabled)
	assert.Equal(t, 10*time.Second, interval)
	preEnabled, preInterval = operation_setting.GetPreHeaderPingPolicy(false)
	assert.True(t, preEnabled)
	assert.Equal(t, 10*time.Second, preInterval)
}

func TestGetPreHeaderPingPolicy_IgnoresChannelOptIn(t *testing.T) {
	setting := operation_setting.GetGeneralSetting()
	common.OptionMapRWMutex.Lock()
	oldEnabled := setting.PingIntervalEnabled
	oldIDs := setting.StreamPingChannelIDs
	setting.PingIntervalEnabled = false
	setting.StreamPingChannelIDs = []int{42}
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		setting.PingIntervalEnabled = oldEnabled
		setting.StreamPingChannelIDs = oldIDs
		common.OptionMapRWMutex.Unlock()
	})

	enabled, _ := operation_setting.GetPreHeaderPingPolicy(false)
	assert.False(t, enabled, "pre-header ping must not enable from channel opt-in")
}

// ---------- StreamStatus integration ----------

func TestStreamScannerHandler_StreamStatus_DoneReason(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(10)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Nil(t, info.StreamStatus.EndError)
	assert.True(t, info.StreamStatus.IsNormalEnd())
	assert.False(t, info.StreamStatus.HasErrors())
}

func TestStreamScannerHandler_StreamStatus_EOFWithoutDone(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d}\n", i)
	}
	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonEOF, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.IsNormalEnd())
}

func TestStreamScannerHandler_StreamStatus_HandlerStop(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(100)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= 10 {
			sr.Stop(fmt.Errorf("stop at 10"))
		}
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.HasErrors())
}

func TestStreamScannerHandler_StreamStatus_HandlerDone(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(20)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		n := count.Add(1)
		if n >= 5 {
			sr.Done()
		}
	})

	assert.Equal(t, int64(5), count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.False(t, info.StreamStatus.HasErrors())
}

func TestStreamScannerHandler_StreamStatus_Timeout(t *testing.T) {
	// Not parallel: modifies global constant.StreamingTimeout
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 1
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })

	pr, pw := io.Pipe()
	go func() {
		fmt.Fprint(pw, "data: {\"id\":1}\n")
		time.Sleep(2 * time.Second)
		pw.Close()
	}()

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	resp := &http.Response{Body: pr}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{}}

	done := make(chan struct{})
	go func() {
		StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for stream timeout")
	}

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonTimeout, info.StreamStatus.EndReason)
	assert.False(t, info.StreamStatus.IsNormalEnd())
}

func TestStreamScannerHandler_StreamStatus_SoftErrors(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(10)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		sr.Error(fmt.Errorf("soft error for chunk"))
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.True(t, info.StreamStatus.HasErrors())
	assert.Equal(t, 10, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_MultipleErrorsPerChunk(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(5)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		sr.Error(fmt.Errorf("error A"))
		sr.Error(fmt.Errorf("error B"))
	})

	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Equal(t, 10, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_ErrorThenStop(t *testing.T) {
	t.Parallel()

	// Use a large body without [DONE] to avoid race between scanner's [DONE]
	// and handler's Stop on the sync.Once EndReason.
	var b strings.Builder
	for i := 0; i < 100; i++ {
		fmt.Fprintf(&b, "data: {\"id\":%d}\n", i)
	}
	c, resp, info := setupStreamTest(t, strings.NewReader(b.String()))

	var count atomic.Int64
	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		count.Add(1)
		sr.Error(fmt.Errorf("soft error"))
		sr.Stop(fmt.Errorf("fatal"))
	})

	assert.Equal(t, int64(1), count.Load())
	require.NotNil(t, info.StreamStatus)
	assert.Equal(t, relaycommon.StreamEndReasonHandlerStop, info.StreamStatus.EndReason)
	assert.Equal(t, 2, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_StreamStatus_InitializedIfNil(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(1)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	assert.Nil(t, info.StreamStatus)

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	assert.NotNil(t, info.StreamStatus)
}

func TestStreamScannerHandler_StreamStatus_ReplacesPreInitialized(t *testing.T) {
	t.Parallel()

	body := buildSSEBody(5)
	c, resp, info := setupStreamTest(t, strings.NewReader(body))

	info.StreamStatus = relaycommon.NewStreamStatus()
	info.StreamStatus.RecordError("pre-existing error")

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {})

	assert.Equal(t, relaycommon.StreamEndReasonDone, info.StreamStatus.EndReason)
	assert.Equal(t, 0, info.StreamStatus.TotalErrorCount())
}

func TestStreamScannerHandler_DrainCapacityExhaustionAndReusability(t *testing.T) {
	policy := operation_setting.ResponsesDrainPolicy{
		Enabled:         true,
		Timeout:         10 * time.Second,
		MaxBytes:        1024,
		MaxGlobalSlots:  2,
		MaxChannelSlots: 1,
	}

	dc1 := NewStreamDrainController(policy, 101)
	status1 := relaycommon.NewStreamStatus()
	granted1 := dc1.TriggerDownstreamTransition(status1, relaycommon.StreamEndReasonClientGone, nil)
	require.True(t, granted1)

	// Second drain on same channel 101 must be rejected by per-channel limit (max 1)
	dc2 := NewStreamDrainController(policy, 101)
	status2 := relaycommon.NewStreamStatus()
	granted2 := dc2.TriggerDownstreamTransition(status2, relaycommon.StreamEndReasonClientGone, nil)
	require.False(t, granted2)
	assert.Equal(t, "capacity", dc2.getResult())

	// Drain on different channel 102 should be granted (global slot 2 of 2)
	dc3 := NewStreamDrainController(policy, 102)
	status3 := relaycommon.NewStreamStatus()
	granted3 := dc3.TriggerDownstreamTransition(status3, relaycommon.StreamEndReasonClientGone, nil)
	require.True(t, granted3)

	// Now global limit (2) is reached; channel 103 must be rejected by global limit
	dc4 := NewStreamDrainController(policy, 103)
	status4 := relaycommon.NewStreamStatus()
	granted4 := dc4.TriggerDownstreamTransition(status4, relaycommon.StreamEndReasonClientGone, nil)
	require.False(t, granted4)
	assert.Equal(t, "capacity", dc4.getResult())

	// Releasing dc1 frees a global and channel 101 slot
	dc1.Cleanup()

	// Reusable: new drain on channel 101 can now be granted
	dc5 := NewStreamDrainController(policy, 101)
	status5 := relaycommon.NewStreamStatus()
	granted5 := dc5.TriggerDownstreamTransition(status5, relaycommon.StreamEndReasonClientGone, nil)
	require.True(t, granted5)

	dc3.Cleanup()
	dc5.Cleanup()

	// Both released: fresh drains on channel 101 and 102 can both be granted
	dc6 := NewStreamDrainController(policy, 101)
	dc7 := NewStreamDrainController(policy, 102)
	require.True(t, dc6.TriggerDownstreamTransition(relaycommon.NewStreamStatus(), relaycommon.StreamEndReasonClientGone, nil))
	require.True(t, dc7.TriggerDownstreamTransition(relaycommon.NewStreamStatus(), relaycommon.StreamEndReasonClientGone, nil))
	dc6.Cleanup()
	dc7.Cleanup()
}

type stagedByteLimitReader struct {
	mu           sync.Mutex
	drainStarted <-chan struct{}
	initialSent  bool
	postData     []byte
	postOffset   int
	closed       bool
}

func newStagedByteLimitReader(drainStarted <-chan struct{}, postData []byte) *stagedByteLimitReader {
	return &stagedByteLimitReader{
		drainStarted: drainStarted,
		postData:     postData,
	}
}

func (r *stagedByteLimitReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, io.EOF
	}
	if !r.initialSent {
		r.initialSent = true
		initial := []byte("data: initial\n\n")
		n := copy(p, initial)
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()

	<-r.drainStarted

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.postOffset >= len(r.postData) {
		return 0, io.EOF
	}
	n := copy(p, r.postData[r.postOffset:])
	r.postOffset += n
	return n, nil
}

func (r *stagedByteLimitReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return nil
}

func TestStreamScannerHandler_DrainByteLimitCases(t *testing.T) {
	terminalPayload := "data: {\"type\":\"response.completed\",\"usage\":{\"total_tokens\":9999}}\n\n"

	testCases := []struct {
		name              string
		budget            int64
		postData          []byte
		expectTerminalSaw bool
		expectDrainResult string
	}{
		{
			name:              "oversized_no_newline_raw_input_terminates",
			budget:            50,
			postData:          []byte(strings.Repeat("raw_stream_payload_without_any_newline_", 5)),
			expectTerminalSaw: false,
			expectDrainResult: "byte_limit",
		},
		{
			name:   "terminal_strictly_beyond_budget_not_observed",
			budget: 50,
			postData: []byte(": comment line padding to consume budget\n" +
				": second comment line exceeding budget\n" +
				terminalPayload),
			expectTerminalSaw: false,
			expectDrainResult: "byte_limit",
		},
		{
			name:              "terminal_within_budget_followed_by_padding_to_exact_cap_observed",
			budget:            int64(len(terminalPayload) + 40),
			postData:          []byte(terminalPayload + strings.Repeat("x", 40)),
			expectTerminalSaw: true,
			expectDrainResult: "recovered",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			policy := operation_setting.ResponsesDrainPolicy{
				Enabled:         true,
				Timeout:         10 * time.Second,
				MaxBytes:        tc.budget,
				MaxGlobalSlots:  4,
				MaxChannelSlots: 2,
			}

			channelId := 55
			drainCtrl := NewStreamDrainController(policy, channelId)

			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			ctx, cancel := context.WithCancel(context.Background())
			c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

			stagedReader := newStagedByteLimitReader(drainCtrl.DrainStarted(), tc.postData)

			resp := &http.Response{
				StatusCode: http.StatusOK,
				Body:       stagedReader,
			}
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channelId}}

			var sawTerminal atomic.Bool
			StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
				if strings.Contains(data, "initial") {
					cancel()
				}
				if strings.Contains(data, "response.completed") {
					sawTerminal.Store(true)
					sr.MarkDrainRecovered()
				}
			}, WithDrainController(drainCtrl))

			assert.Equal(t, tc.expectDrainResult, info.DrainResult)
			assert.Equal(t, tc.expectTerminalSaw, sawTerminal.Load())

			// Prove resources release and capacity is reusable after return
			dcReuse := NewStreamDrainController(policy, channelId)
			statusReuse := relaycommon.NewStreamStatus()
			require.True(t, dcReuse.TriggerDownstreamTransition(statusReuse, relaycommon.StreamEndReasonClientGone, nil))
			dcReuse.Cleanup()
		})
	}
}

type closeNotifyingBody struct {
	io.Reader
	closed    chan struct{}
	closeOnce sync.Once
}

func (b *closeNotifyingBody) Close() error {
	b.closeOnce.Do(func() {
		close(b.closed)
	})
	return nil
}

func TestStreamScannerHandler_LateTransitionDuringCleanupDoesNotLeakCapacity(t *testing.T) {
	policy := operation_setting.ResponsesDrainPolicy{
		Enabled:         true,
		Timeout:         10 * time.Second,
		MaxBytes:        1024,
		MaxGlobalSlots:  1,
		MaxChannelSlots: 1,
	}

	drainCtrl := NewStreamDrainController(policy, 99)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)

	bodyClosed := make(chan struct{})
	fakeBody := &closeNotifyingBody{
		Reader: strings.NewReader("data: chunk1\n\n"),
		closed: bodyClosed,
	}

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       fakeBody,
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 99}}

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		// Wait until cleanup has started and closed resp.Body
		<-bodyClosed
		// Late transition when scanner has already reached EOF and main is cleaning up
		sr.DownstreamWriteError(errors.New("simulated late write error during cleanup"))
	}, WithDrainController(drainCtrl))

	// Capacity MUST be reusable: another drain controller on the same channel must be granted
	dcReuse := NewStreamDrainController(policy, 99)
	statusReuse := relaycommon.NewStreamStatus()
	granted := dcReuse.TriggerDownstreamTransition(statusReuse, relaycommon.StreamEndReasonClientGone, nil)
	assert.True(t, granted, "drain capacity must be reusable after return without leaking slots")
	dcReuse.Cleanup()
}

func TestStreamScannerHandler_DrainTimeoutFixedAndNotReset(t *testing.T) {
	policy := operation_setting.ResponsesDrainPolicy{
		Enabled:         true,
		Timeout:         40 * time.Millisecond, // very short drain timeout
		MaxBytes:        1024 * 1024,
		MaxGlobalSlots:  4,
		MaxChannelSlots: 2,
	}

	drainCtrl := NewStreamDrainController(policy, 77)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

	// Upstream reader produces chunks with small intervals
	pipeR, pipeW := io.Pipe()
	go func() {
		defer pipeW.Close()
		_, _ = fmt.Fprintf(pipeW, "data: chunk-0\n\n")
		for i := 1; i <= 20; i++ {
			time.Sleep(10 * time.Millisecond)
			_, err := fmt.Fprintf(pipeW, "data: chunk-%d\n\n", i)
			if err != nil {
				return
			}
		}
	}()

	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       pipeR,
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 77}}

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		if data == "chunk-0" {
			cancel()
		}
	}, WithDrainController(drainCtrl))

	assert.Equal(t, "timeout", info.DrainResult)

	// Capacity reusable
	dcReuse := NewStreamDrainController(policy, 77)
	statusReuse := relaycommon.NewStreamStatus()
	require.True(t, dcReuse.TriggerDownstreamTransition(statusReuse, relaycommon.StreamEndReasonClientGone, nil))
	dcReuse.Cleanup()
}

func TestStreamScannerHandler_DrainEOFWithoutTerminalUpstreamEnd(t *testing.T) {
	policy := operation_setting.ResponsesDrainPolicy{
		Enabled:         true,
		Timeout:         10 * time.Second,
		MaxBytes:        1024 * 1024,
		MaxGlobalSlots:  4,
		MaxChannelSlots: 2,
	}

	drainCtrl := NewStreamDrainController(policy, 88)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	ctx, cancel := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil).WithContext(ctx)

	// Upstream reader closes cleanly after 2 chunks without terminal
	body := "data: hello\n\ndata: world\n\n"
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{ChannelId: 88}}

	StreamScannerHandler(c, resp, info, func(data string, sr *StreamResult) {
		if data == "hello" {
			cancel()
		}
	}, WithDrainController(drainCtrl))

	assert.Equal(t, "upstream_end", info.DrainResult)

	// Capacity reusable
	dcReuse := NewStreamDrainController(policy, 88)
	statusReuse := relaycommon.NewStreamStatus()
	require.True(t, dcReuse.TriggerDownstreamTransition(statusReuse, relaycommon.StreamEndReasonClientGone, nil))
	dcReuse.Cleanup()
}

type drainTrackingTestReader struct {
	r          *strings.Reader
	consumed   int
	beforeRead func()
}

func (r *drainTrackingTestReader) Read(p []byte) (int, error) {
	if r.beforeRead != nil {
		r.beforeRead()
	}
	n, err := r.r.Read(p)
	r.consumed += n
	return n, err
}

func TestDrainTrackingReader(t *testing.T) {
	t.Run("in_flight_transition_bounds_underlying_read", func(t *testing.T) {
		const budget = int64(16)
		data := "0123456789abcdef_extra_unbounded_bytes"
		callerBuf := make([]byte, 64)

		var tr *drainTrackingReader
		underlying := &drainTrackingTestReader{
			r: strings.NewReader(data),
			beforeRead: func() {
				tr.startCounting()
			},
		}
		tr = newDrainTrackingReader(underlying, budget)

		// First read: starts before counting, transitions to counting inside underlying Read
		n1, err1 := tr.Read(callerBuf)
		require.Equal(t, int(budget), n1)
		require.Equal(t, io.EOF, err1)
		assert.Equal(t, data[:budget], string(callerBuf[:n1]))

		// The underlying reader must not have consumed more than the budget.
		// On the unfixed implementation, toRead is len(callerBuf) (64), so all 38 bytes
		// are consumed from the underlying reader before truncation occurs.
		assert.LessOrEqual(t, int64(underlying.consumed), budget, "underlying reader must not consume more bytes than budget")

		// Budget is exhausted; second read must return 0, io.EOF without consuming underlying reader
		consumedAfterFirst := underlying.consumed
		n2, err2 := tr.Read(callerBuf)
		assert.Equal(t, 0, n2)
		assert.Equal(t, io.EOF, err2)
		assert.Equal(t, consumedAfterFirst, underlying.consumed, "second read must not consume additional underlying bytes")

		assert.True(t, tr.isExceeded())
	})

	t.Run("normal_streaming_without_drain_preserves_full_data", func(t *testing.T) {
		const budget = int64(16)
		data := strings.Repeat("hello_world_", 4) // 48 bytes (> budget 16)

		underlying := &drainTrackingTestReader{
			r: strings.NewReader(data),
		}
		tr := newDrainTrackingReader(underlying, budget)

		readAll, err := io.ReadAll(tr)
		require.NoError(t, err)

		assert.Equal(t, data, string(readAll), "normal streaming must deliver full data without truncation")
		assert.Equal(t, len(data), underlying.consumed)
		assert.False(t, tr.isExceeded())
	})

	t.Run("unlimited_budget_when_max_bytes_non_positive", func(t *testing.T) {
		data := "unlimited_payload_without_byte_cap"

		for _, maxBytes := range []int64{0, -1} {
			t.Run(fmt.Sprintf("max_bytes_%d", maxBytes), func(t *testing.T) {
				var tr *drainTrackingReader
				underlying := &drainTrackingTestReader{
					r: strings.NewReader(data),
					beforeRead: func() {
						tr.startCounting()
					},
				}
				tr = newDrainTrackingReader(underlying, maxBytes)

				readAll, err := io.ReadAll(tr)
				require.NoError(t, err)

				assert.Equal(t, data, string(readAll), "maxBytes <= 0 must not truncate stream")
				assert.Equal(t, len(data), underlying.consumed)
				assert.False(t, tr.isExceeded())
			})
		}
	})
}
