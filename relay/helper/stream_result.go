package helper

import (
	"context"

	relaycommon "github.com/QuantumNous/new-api/relay/common"
)

// StreamResult is passed to each dataHandler invocation, providing methods
// to record soft errors, signal fatal stops, or mark normal completion.
// StreamScannerHandler checks IsStopped() after each callback invocation.
type StreamResult struct {
	status    *relaycommon.StreamStatus
	stopped   bool
	drainCtrl *StreamDrainController
	clientCtx context.Context
}

func newStreamResult(status *relaycommon.StreamStatus, drainCtrl *StreamDrainController, clientCtx context.Context) *StreamResult {
	return &StreamResult{
		status:    status,
		drainCtrl: drainCtrl,
		clientCtx: clientCtx,
	}
}

// Error records a soft error. The stream continues processing.
// Can be called multiple times per chunk.
func (r *StreamResult) Error(err error) {
	if err == nil {
		return
	}
	r.status.RecordError(err.Error())
}

// Stop records a fatal error and marks the stream to stop after this chunk.
func (r *StreamResult) Stop(err error) {
	if err != nil {
		r.status.RecordError(err.Error())
	}
	r.status.SetEndReason(relaycommon.StreamEndReasonHandlerStop, err)
	r.stopped = true
}

// Done signals that the handler has finished processing normally
// (e.g., Dify "message_end"). The stream stops after this chunk.
func (r *StreamResult) Done() {
	r.status.SetEndReason(relaycommon.StreamEndReasonDone, nil)
	r.stopped = true
}

// IsStopped returns whether Stop() or Done() was called during this chunk.
func (r *StreamResult) IsStopped() bool {
	return r.stopped
}

// IsDownstreamClosed returns whether the downstream connection is closed or has entered drain.
// When no drainCtrl is present, it returns false without consulting clientCtx.
func (r *StreamResult) IsDownstreamClosed() bool {
	if r == nil || r.drainCtrl == nil {
		return false
	}
	if r.drainCtrl.IsDownstreamClosed() {
		return true
	}
	if r.clientCtx != nil {
		select {
		case <-r.clientCtx.Done():
			r.drainCtrl.TriggerDownstreamTransition(r.status, relaycommon.StreamEndReasonClientGone, r.clientCtx.Err())
			return true
		default:
		}
	}
	return false
}

// IsDrainActive returns whether post-disconnect usage drain is granted and active.
func (r *StreamResult) IsDrainActive() bool {
	if r == nil || r.drainCtrl == nil {
		return false
	}
	return r.drainCtrl.IsDrainGranted()
}

// MarkDrainRecovered marks the drain outcome as recovered when terminal usage is captured.
func (r *StreamResult) MarkDrainRecovered() {
	if r == nil || r.drainCtrl == nil {
		return
	}
	r.drainCtrl.MarkRecovered()
}

// DownstreamWriteError records a downstream write error. If drain is supported and granted,
// it transitions to drain mode without stopping upstream stream consumption.
// If drain controller is nil, it immediately uses the original Stop(err) without extra RecordError.
func (r *StreamResult) DownstreamWriteError(err error) {
	if r == nil {
		return
	}
	if r.drainCtrl == nil {
		r.Stop(err)
		return
	}
	if err != nil && r.status != nil {
		r.status.RecordError("downstream write error: " + err.Error())
	}
	granted := r.drainCtrl.TriggerDownstreamTransition(r.status, relaycommon.StreamEndReasonClientGone, err)
	if granted {
		return
	}
	r.Stop(err)
}

// reset clears the per-chunk stopped flag so the object can be reused.
func (r *StreamResult) reset() {
	r.stopped = false
}
