// Copyright © 2022 Kaleido, Inc.
//
// SPDX-License-Identifier: Apache-2.0
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package fftm

import (
	"context"
	"time"

	"github.com/hyperledger/firefly-common/pkg/fftypes"
	"github.com/hyperledger/firefly-common/pkg/i18n"
	"github.com/hyperledger/firefly-common/pkg/log"
	"github.com/hyperledger/firefly-transaction-manager/internal/confirmations"
	"github.com/hyperledger/firefly-transaction-manager/internal/persistence"
	"github.com/hyperledger/firefly-transaction-manager/internal/tmmsgs"
	"github.com/hyperledger/firefly-transaction-manager/pkg/apitypes"
	"github.com/hyperledger/firefly-transaction-manager/pkg/ffcapi"
)

func (m *manager) policyLoop() {
	defer close(m.policyLoopDone)
	ctx := log.WithLogField(m.ctx, "role", "policyloop")

	for {
		timer := time.NewTimer(m.policyLoopInterval)
		select {
		case <-m.inflightUpdate:
			m.policyLoopCycle(ctx, false)
		case <-m.inflightStale:
			m.policyLoopCycle(ctx, true)
		case <-timer.C:
			m.policyLoopCycle(ctx, false)
		case <-ctx.Done():
			log.L(ctx).Infof("Receipt poller exiting")
			return
		}
	}
}

func (m *manager) markInflightStale() {
	select {
	case m.inflightStale <- true:
	default:
	}
}

func (m *manager) markInflightUpdate() {
	select {
	case m.inflightUpdate <- true:
	default:
	}
}

func (m *manager) updateInflightSet(ctx context.Context) bool {

	oldInflight := m.inflight
	m.inflight = make([]*pendingState, 0, len(oldInflight))

	// Run through removing those that are removed
	for _, p := range oldInflight {
		if !p.remove {
			m.inflight = append(m.inflight, p)
		}
	}

	// If we are not at maximum, then query if there are more candidates now
	spaces := m.maxInFlight - len(m.inflight)
	if spaces > 0 {
		var after *fftypes.UUID
		if len(m.inflight) > 0 {
			after = m.inflight[len(m.inflight)-1].mtx.SequenceID
		}
		var additional []*apitypes.ManagedTX
		// We retry the get from persistence indefinitely (until the context cancels)
		err := m.retry.Do(ctx, "get pending transactions", func(attempt int) (retry bool, err error) {
			additional, err = m.persistence.ListTransactionsPending(ctx, after, spaces, persistence.SortDirectionAscending)
			return true, err
		})
		if err != nil {
			log.L(ctx).Infof("Policy loop context cancelled while retrying")
			return false
		}
		for _, mtx := range additional {
			m.inflight = append(m.inflight, &pendingState{mtx: mtx})
		}
		newLen := len(m.inflight)
		if newLen > 0 {
			log.L(ctx).Debugf("Inflight set updated len=%d head-seq=%s tail-seq=%s old-tail=%s", len(m.inflight), m.inflight[0].mtx.SequenceID, m.inflight[newLen-1].mtx.SequenceID, after)
		}
	}
	return true

}

func (m *manager) policyLoopCycle(ctx context.Context, inflightStale bool) {

	if inflightStale {
		if !m.updateInflightSet(ctx) {
			return
		}
	}

	// Go through executing the policy engine against them
	for _, pending := range m.inflight {
		err := m.execPolicy(ctx, pending)
		if err != nil {
			log.L(ctx).Errorf("Failed policy cycle transaction=%s operation=%s: %s", pending.mtx.TransactionHash, pending.mtx.ID, err)
		}
	}

}

func (m *manager) addError(mtx *apitypes.ManagedTX, reason ffcapi.ErrorReason, err error) {
	newLen := len(mtx.ErrorHistory) + 1
	if newLen > m.errorHistoryCount {
		newLen = m.errorHistoryCount
	}
	oldHistory := mtx.ErrorHistory
	mtx.ErrorHistory = make([]*apitypes.ManagedTXError, newLen)
	latestError := &apitypes.ManagedTXError{
		Time:   fftypes.Now(),
		Mapped: reason,
		Error:  err.Error(),
	}
	mtx.ErrorMessage = latestError.Error
	mtx.ErrorHistory[0] = latestError
	for i := 1; i < newLen; i++ {
		mtx.ErrorHistory[i] = oldHistory[i-1]
	}
}

func (m *manager) execPolicy(ctx context.Context, pending *pendingState) (err error) {

	var updated bool
	completed := false

	// Check whether this has been confirmed by the confirmation manager
	m.mux.Lock()
	mtx := pending.mtx
	confirmed := pending.confirmed
	m.mux.Unlock()

	switch {
	case confirmed:
		updated = true
		completed = true
		if mtx.Receipt.Success {
			mtx.Status = apitypes.TxStatusSucceeded
			mtx.ErrorMessage = ""
		} else {
			mtx.Status = apitypes.TxStatusFailed
			mtx.ErrorMessage = i18n.NewError(ctx, tmmsgs.MsgTransactionFailed).Error()
		}

	default:
		// We get woken for lots of reasons to go through the policy loop, but we only want
		// to drive the policy engine at regular intervals.
		// So we track the last time we ran the policy engine against each pending item.
		if time.Since(pending.lastPolicyCycle) > m.policyLoopInterval {
			// Pass the state to the pluggable policy engine to potentially perform more actions against it,
			// such as submitting for the first time, or raising the gas etc.
			var reason ffcapi.ErrorReason
			updated, reason, err = m.policyEngine.Execute(ctx, m.connector, pending.mtx)
			if err != nil {
				log.L(ctx).Errorf("Policy engine returned error for transaction %s reason=%s: %s", mtx.ID, reason, err)
				m.addError(mtx, reason, err)
			} else {
				if mtx.FirstSubmit != nil && pending.trackingTransactionHash != mtx.TransactionHash {
					// If now submitted, add to confirmations manager for receipt checking
					m.trackSubmittedTransaction(ctx, pending)
				}
				pending.lastPolicyCycle = time.Now()
			}
		}
	}

	if updated || err != nil {
		mtx.Updated = fftypes.Now()
		err := m.persistence.WriteTransaction(ctx, mtx, false)
		if err != nil {
			log.L(ctx).Errorf("Failed to update transaction %s (status=%s): %s", mtx.ID, mtx.Status, err)
			return err
		}
		if completed {
			pending.remove = true // for the next time round the loop
			m.markInflightStale()
		}
		m.sendWSReply(mtx)
	}

	return nil
}

func (m *manager) sendWSReply(mtx *apitypes.ManagedTX) {
	wsr := &apitypes.TransactionUpdateReply{
		ManagedTX: *mtx,
		Headers: apitypes.ReplyHeaders{
			RequestID: mtx.ID,
		},
	}
	switch mtx.Status {
	case apitypes.TxStatusSucceeded:
		wsr.Headers.Type = apitypes.TransactionUpdateSuccess
	case apitypes.TxStatusFailed:
		wsr.Headers.Type = apitypes.TransactionUpdateFailure
	default:
		wsr.Headers.Type = apitypes.TransactionUpdate
	}
	// Notify on the websocket - this is best-effort (there is no subscription/acknowledgement)
	m.wsServer.SendReply(wsr)
}

func (m *manager) trackSubmittedTransaction(ctx context.Context, pending *pendingState) {
	var err error

	// Clear any old transaction hash
	if pending.trackingTransactionHash != "" {
		err = m.confirmations.Notify(&confirmations.Notification{
			NotificationType: confirmations.RemovedTransaction,
			Transaction: &confirmations.TransactionInfo{
				TransactionHash: pending.trackingTransactionHash,
			},
		})
	}

	// Notify of the new
	if err == nil {
		err = m.confirmations.Notify(&confirmations.Notification{
			NotificationType: confirmations.NewTransaction,
			Transaction: &confirmations.TransactionInfo{
				TransactionHash: pending.mtx.TransactionHash,
				Receipt: func(ctx context.Context, receipt *ffcapi.TransactionReceiptResponse) {
					// Will be picked up on the next policy loop cycle - guaranteed to occur before Confirmed
					m.mux.Lock()
					pending.mtx.Receipt = receipt
					m.mux.Unlock()
					log.L(m.ctx).Debugf("Receipt received for transaction %s at nonce %s / %d - hash: %s", pending.mtx.ID, pending.mtx.TransactionHeaders.From, pending.mtx.Nonce.Int64(), pending.mtx.TransactionHash)
					m.markInflightUpdate()
				},
				Confirmed: func(ctx context.Context, confirmations []confirmations.BlockInfo) {
					// Will be picked up on the next policy loop cycle
					m.mux.Lock()
					pending.confirmed = true
					pending.mtx.Confirmations = confirmations
					m.mux.Unlock()
					log.L(m.ctx).Debugf("Confirmed transaction %s at nonce %s / %d - hash: %s", pending.mtx.ID, pending.mtx.TransactionHeaders.From, pending.mtx.Nonce.Int64(), pending.mtx.TransactionHash)
					m.markInflightUpdate()
				},
			},
		})
	}

	// Only reason for error here should be a cancelled context
	if err != nil {
		log.L(ctx).Infof("Error detected notifying confirmation manager: %s", err)
	} else {
		pending.trackingTransactionHash = pending.mtx.TransactionHash
	}
}