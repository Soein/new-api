package service

import (
	"context"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

type walletReservationLeaseManager struct {
	mu      sync.Mutex
	entries map[string]func() bool
	wake    chan struct{}
	running bool
}

var walletReservationLeases = &walletReservationLeaseManager{entries: map[string]func() bool{}, wake: make(chan struct{}, 1)}

func (m *walletReservationLeaseManager) add(ctx context.Context, requestID string, duration time.Duration) func() {
	m.mu.Lock()
	m.entries[requestID] = context.AfterFunc(ctx, func() { m.remove(requestID) })
	if !m.running {
		m.running = true
		go m.run(duration)
	}
	m.mu.Unlock()
	return func() { m.remove(requestID) }
}

func (m *walletReservationLeaseManager) remove(requestID string) {
	m.mu.Lock()
	stopContext := m.entries[requestID]
	delete(m.entries, requestID)
	empty := len(m.entries) == 0
	m.mu.Unlock()
	if stopContext != nil {
		stopContext()
	}
	if empty {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// The process uses one startup-configured TTL. A fixed cadence batches every
// live request, so staggered arrival times never cause per-request table scans.
// Ordinary add/remove operations are O(1); the worker exits when empty.
func (m *walletReservationLeaseManager) run(duration time.Duration) {
	ticker := time.NewTicker(max(duration/3, time.Millisecond))
	defer ticker.Stop()
	for {
		m.mu.Lock()
		if len(m.entries) == 0 {
			m.running = false
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		select {
		case <-m.wake:
			continue
		case now := <-ticker.C:
			m.mu.Lock()
			requestIDs := make([]string, 0, len(m.entries))
			for id := range m.entries {
				requestIDs = append(requestIDs, id)
			}
			m.mu.Unlock()
			for start := 0; start < len(requestIDs); start += 128 {
				batch := requestIDs[start:min(start+128, len(requestIDs))]
				if err := model.RenewWalletQuotaReservations(batch, now.UnixMilli(), now.Add(duration).UnixMilli()); err != nil {
					common.SysLog(common.LogText("failed to renew wallet reminder reservations: %s", err.Error()))
				}
			}
		}
	}
}
