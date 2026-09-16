package helper

import (
	"io"
	"sync"
	"sync/atomic"
	"time"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
)

var (
	drainSlotMutex        sync.Mutex
	activeDrainGlobal     int
	activeDrainPerChannel = make(map[int]int)
)

type DrainSlotTicket struct {
	channelId   int
	releaseOnce sync.Once
}

func (t *DrainSlotTicket) Release() {
	if t == nil {
		return
	}
	t.releaseOnce.Do(func() {
		drainSlotMutex.Lock()
		defer drainSlotMutex.Unlock()
		if activeDrainGlobal > 0 {
			activeDrainGlobal--
		}
		if t.channelId > 0 {
			if count, ok := activeDrainPerChannel[t.channelId]; ok {
				if count <= 1 {
					delete(activeDrainPerChannel, t.channelId)
				} else {
					activeDrainPerChannel[t.channelId] = count - 1
				}
			}
		}
	})
}

func tryAcquireDrainSlot(channelId int, maxGlobal, maxChannel int) (*DrainSlotTicket, bool) {
	drainSlotMutex.Lock()
	defer drainSlotMutex.Unlock()

	if activeDrainGlobal >= maxGlobal {
		return nil, false
	}
	if channelId > 0 && activeDrainPerChannel[channelId] >= maxChannel {
		return nil, false
	}

	activeDrainGlobal++
	if channelId > 0 {
		activeDrainPerChannel[channelId]++
	}

	return &DrainSlotTicket{channelId: channelId}, true
}

type drainTrackingReader struct {
	r         io.Reader
	mu        sync.Mutex
	counting  bool
	byteCount int64
	maxBytes  int64
	exceeded  bool
}

func newDrainTrackingReader(r io.Reader, maxBytes int64) *drainTrackingReader {
	return &drainTrackingReader{
		r:        r,
		maxBytes: maxBytes,
	}
}

func (tr *drainTrackingReader) startCounting() {
	tr.mu.Lock()
	tr.counting = true
	tr.mu.Unlock()
}

func (tr *drainTrackingReader) isExceeded() bool {
	if tr == nil {
		return false
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.exceeded
}

func (tr *drainTrackingReader) Read(p []byte) (int, error) {
	tr.mu.Lock()
	if tr.counting && tr.maxBytes > 0 && tr.byteCount >= tr.maxBytes {
		tr.mu.Unlock()
		return 0, io.EOF
	}
	toRead := len(p)
	if tr.maxBytes > 0 {
		limit := tr.maxBytes
		if tr.counting {
			limit -= tr.byteCount
		}
		if limit < 0 {
			limit = 0
		}
		if limit < int64(toRead) {
			toRead = int(limit)
		}
	}
	tr.mu.Unlock()

	n, err := tr.r.Read(p[:toRead])
	if n > 0 {
		tr.mu.Lock()
		if tr.counting && tr.maxBytes > 0 {
			remaining := tr.maxBytes - tr.byteCount
			if remaining <= 0 {
				tr.exceeded = true
				tr.mu.Unlock()
				return 0, io.EOF
			}
			if int64(n) >= remaining {
				n = int(remaining)
				tr.byteCount = tr.maxBytes
				tr.exceeded = true
				tr.mu.Unlock()
				return n, io.EOF
			}
			tr.byteCount += int64(n)
			if tr.byteCount >= tr.maxBytes {
				tr.exceeded = true
				if err == nil {
					err = io.EOF
				}
			}
		}
		tr.mu.Unlock()
	}
	return n, err
}

type StreamDrainController struct {
	policy           operation_setting.ResponsesDrainPolicy
	channelId        int
	transitionOnce   sync.Once
	cleanupOnce      sync.Once
	downstreamClosed atomic.Bool
	draining         atomic.Bool
	drainGranted     atomic.Bool
	cleanedUp        atomic.Bool
	ticket           *DrainSlotTicket
	ticketMu         sync.Mutex
	drainTimer       *time.Timer
	drainTimerChan   <-chan time.Time
	drainStartedChan chan struct{}
	drainStartedOnce sync.Once
	countingReader   *drainTrackingReader
	stopFunc         func()

	mu           sync.Mutex
	stopCause    string
	recovered    bool
	finalOutcome string
}

func NewStreamDrainController(policy operation_setting.ResponsesDrainPolicy, channelId int) *StreamDrainController {
	dc := &StreamDrainController{
		policy:           policy,
		channelId:        channelId,
		drainStartedChan: make(chan struct{}),
	}
	return dc
}

func (dc *StreamDrainController) WrapReader(r io.Reader) io.Reader {
	if dc == nil || !dc.policy.Enabled {
		return r
	}
	dc.countingReader = newDrainTrackingReader(r, dc.policy.MaxBytes)
	return dc.countingReader
}

func (dc *StreamDrainController) SetStopFunc(stop func()) {
	if dc != nil {
		dc.stopFunc = stop
	}
}

func (dc *StreamDrainController) IsDownstreamClosed() bool {
	if dc == nil {
		return false
	}
	return dc.downstreamClosed.Load()
}

func (dc *StreamDrainController) IsDrainGranted() bool {
	if dc == nil {
		return false
	}
	return dc.drainGranted.Load()
}

func (dc *StreamDrainController) IsDraining() bool {
	if dc == nil {
		return false
	}
	return dc.draining.Load()
}

func (dc *StreamDrainController) DrainStarted() <-chan struct{} {
	if dc == nil {
		return nil
	}
	return dc.drainStartedChan
}

func (dc *StreamDrainController) TriggerDownstreamTransition(status *relaycommon.StreamStatus, reason relaycommon.StreamEndReason, err error) bool {
	if dc == nil {
		return false
	}
	dc.transitionOnce.Do(func() {
		dc.downstreamClosed.Store(true)

		if status != nil {
			status.SetEndReason(reason, err)
		}

		if !dc.policy.Enabled {
			if dc.stopFunc != nil {
				dc.stopFunc()
			}
			return
		}

		if dc.cleanedUp.Load() {
			return
		}

		dc.mu.Lock()
		alreadyRecovered := dc.recovered
		dc.mu.Unlock()

		if alreadyRecovered {
			if dc.stopFunc != nil {
				dc.stopFunc()
			}
			return
		}

		ticket, ok := tryAcquireDrainSlot(dc.channelId, dc.policy.MaxGlobalSlots, dc.policy.MaxChannelSlots)
		if !ok {
			dc.mu.Lock()
			dc.stopCause = "capacity"
			dc.mu.Unlock()
			if dc.stopFunc != nil {
				dc.stopFunc()
			}
			return
		}

		dc.ticketMu.Lock()
		dc.ticket = ticket
		dc.ticketMu.Unlock()

		dc.drainGranted.Store(true)
		dc.draining.Store(true)
		if dc.countingReader != nil {
			dc.countingReader.startCounting()
		}
		dc.drainTimer = time.NewTimer(dc.policy.Timeout)
		dc.drainTimerChan = dc.drainTimer.C
		dc.drainStartedOnce.Do(func() {
			close(dc.drainStartedChan)
		})
	})
	return dc.drainGranted.Load()
}

func (dc *StreamDrainController) MarkRecovered() {
	if dc == nil {
		return
	}
	dc.mu.Lock()
	dc.recovered = true
	dc.mu.Unlock()
}

func (dc *StreamDrainController) IsRecovered() bool {
	if dc == nil {
		return false
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	return dc.recovered
}

func (dc *StreamDrainController) setStopCause(cause string) {
	if dc == nil {
		return
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.stopCause == "" {
		dc.stopCause = cause
	}
}

func (dc *StreamDrainController) FinalizeOutcome() string {
	if dc == nil || !dc.policy.Enabled {
		return ""
	}
	dc.mu.Lock()
	defer dc.mu.Unlock()
	if dc.finalOutcome != "" {
		return dc.finalOutcome
	}
	// Normal opt-in requests with NO downstream closure must have empty DrainResult,
	// even though internal recovered flag is set to prevent pointless late acquisition.
	if !dc.downstreamClosed.Load() {
		return ""
	}
	// If downstream did close and trusted usage is already captured, report recovered,
	// including terminal-write failure.
	if dc.recovered {
		dc.finalOutcome = "recovered"
		return dc.finalOutcome
	}
	// Otherwise keep explicit capacity/timeout.
	if dc.stopCause != "" {
		dc.finalOutcome = dc.stopCause
		return dc.finalOutcome
	}
	// If drain started and no explicit cause, derive byte_limit (reader exceeded) or upstream_end.
	if dc.drainGranted.Load() {
		if dc.countingReader != nil && dc.countingReader.isExceeded() {
			dc.finalOutcome = "byte_limit"
		} else {
			dc.finalOutcome = "upstream_end"
		}
		return dc.finalOutcome
	}
	return ""
}

func (dc *StreamDrainController) getResult() string {
	return dc.FinalizeOutcome()
}

func (dc *StreamDrainController) setResult(res string) {
	if res == "recovered" {
		dc.MarkRecovered()
		return
	}
	dc.setStopCause(res)
}

func (dc *StreamDrainController) Cleanup() {
	if dc == nil {
		return
	}
	dc.cleanupOnce.Do(func() {
		dc.cleanedUp.Store(true)
		if dc.drainTimer != nil {
			dc.drainTimer.Stop()
		}
		dc.ticketMu.Lock()
		if dc.ticket != nil {
			dc.ticket.Release()
			dc.ticket = nil
		}
		dc.ticketMu.Unlock()
	})
}
