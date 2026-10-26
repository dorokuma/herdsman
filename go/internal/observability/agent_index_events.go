package observability

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// This file ports #appendStatusEvents of
// src/observability/agent-index-service.ts: the status-transition dedup, the Pi
// turn-completion signal wait with its degrade/release judgement, and the
// non-Pi readiness gate.

// appendStatusEvents mirrors #appendStatusEvents. The returned cancelled flag
// stands in for the TS PLAN_CANCELLED sentinel: the agent row is gone, so
// appending would create an undeliverable dangling event and the plan must be
// settled through the salvage path instead of being skipped.
func (s *AgentIndexService) appendStatusEvents(
	input StatusEventPlan,
) (*AgentEventRecord, bool, error) {
	// Invariant: compactHistory is always populated as an object before calling
	// #appendStatusEvents; the nil check below is defensive and unreachable in
	// normal operation.
	paneClosed, err := s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
		HerdrSessionName: input.Agent.HerdrSessionName,
		PaneID:           input.Agent.PaneID,
		PaneGeneration:   input.Agent.PaneGeneration,
	})
	if err != nil {
		return nil, false, err
	}
	if paneClosed {
		fmt.Fprintf(os.Stderr,
			"Herdsman skipping status event generation because pane is closed"+
				" (agentId=%s, herdrSessionName=%s, paneId=%s)\n",
			input.Agent.ID, input.Agent.HerdrSessionName, input.Agent.PaneID)
		return nil, false, nil
	}
	latest, err := s.stores.AgentEvents.LatestStatusTransition(
		input.Agent.ID, input.Agent.HerdrSessionName)
	if err != nil {
		return nil, false, err
	}
	skipStatusChanged := false
	if latest != nil && statusTransitionMatches(*latest, input.From, input.To) {
		hasTerminalAfter, err := s.stores.AgentEvents.HasTerminalEventAfter(
			input.Agent.ID, input.Agent.HerdrSessionName, latest.ID)
		if err != nil {
			return nil, false, err
		}
		if !hasTerminalAfter {
			if statusEventType(input.To) == nil {
				fmt.Fprintf(os.Stderr,
					"Herdsman skipping duplicate status transition event (agentId=%s,"+
						" from=%s, to=%s)\n", input.Agent.ID, input.From, input.To)
				return nil, false, nil
			}
			skipStatusChanged = true
		}
		if hasTerminalAfter && input.HerdrEventKey == nil {
			isAgyHistory := (input.Agent.Agent != nil && *input.Agent.Agent == "agy") ||
				(input.CompactHistory != nil && input.CompactHistory.Source != nil &&
					*input.CompactHistory.Source == string(HistorySourceAntigravitySQLite))
			// For an agy/antigravity pane only a terminal event that already
			// represented a completed turn may count as the delivered baseline: an
			// `unknown -> idle` startup row would otherwise make the `working -> done`
			// row of the same round look like a duplicate.
			var lastTerminal *AgentEventRecord
			if isAgyHistory {
				lastTerminal, err = s.stores.AgentEvents.LatestCompletedTurnEvent(
					input.Agent.ID, input.Agent.HerdrSessionName)
			} else {
				lastTerminal, err = s.stores.AgentEvents.LatestTerminalEvent(
					input.Agent.ID, input.Agent.HerdrSessionName)
			}
			if err != nil {
				return nil, false, err
			}
			if sameTerminalAssistantContent(
				input.CompactHistory, compactHistoryOf(lastTerminal), input.Agent.Agent,
			) {
				// An agy round whose ref already reached a completed turn is settled: a
				// retry must skip it instead of exhausting the budget into `discarded`.
				attempts := 0
				if input.Attempts != nil {
					attempts = *input.Attempts
				}
				if attempts > 0 && !isAgyHistory {
					return nil, false, &PlanWaitingHistoryError{}
				}
				fmt.Fprintf(os.Stderr,
					"Herdsman skipping legacy duplicate status transition event with"+
						" identical history ref (agentId=%s, from=%s, to=%s)\n",
					input.Agent.ID, input.From, input.To)
				return nil, false, nil
			}
		}
	}
	observationID := fmt.Sprintf("transition:%d", latestEventID(latest))

	controller := newWaiterControllerFor(input.Agent)
	unregister := s.registerActiveWaiter(input.Agent, controller)
	defer unregister()

	compactHistory := input.CompactHistory
	payloadExtra := map[string]any{}
	terminalID := ""
	if input.Agent.TerminalID != nil {
		terminalID = *input.Agent.TerminalID
	}
	agentKind := ""
	if input.Agent.Agent != nil {
		agentKind = *input.Agent.Agent
	}
	attempts := 0
	if input.Attempts != nil {
		attempts = *input.Attempts
	}

	if s.turnCompletions != nil &&
		(input.To == AgentStatusDone || input.To == AgentStatusBlocked) &&
		terminalID != "" && agentKind == "pi" {
		isRetry := attempts > 0
		latestTerminal, err := s.stores.AgentEvents.LatestTerminalEvent(
			input.Agent.ID, input.Agent.HerdrSessionName)
		if err != nil {
			return nil, false, err
		}
		baseline := input.CompactHistory
		if isRetry && latestTerminal != nil && latestTerminal.CompactHistory != nil {
			baseline = latestTerminal.CompactHistory
		}
		isAlreadyEmittedInRetry := isRetry && latestTerminal != nil &&
			latestTerminal.CompactHistory != nil &&
			sameTerminalAssistantContent(
				input.CompactHistory, latestTerminal.CompactHistory, stringPointer("pi"))

		turn := s.turnCompletions.WaitForSignal(WaitForSignalInput{
			HerdrSessionName: input.Agent.HerdrSessionName,
			RecordedAfterMs:  s.now() - TurnSignalWaitMs,
			Signal:           controller.Done(),
			TerminalID:       terminalID,
		})

		abortedOrClosed, err := s.abortedOrPaneClosed(controller)
		if err != nil {
			return nil, false, err
		}
		if abortedOrClosed {
			fmt.Fprintf(os.Stderr,
				"Herdsman skipping status event generation because turn wait was aborted"+
					" or pane closed (aborted=%t, agentId=%s, herdrSessionName=%s, paneId=%s)\n",
				controller.Aborted(), input.Agent.ID, input.Agent.HerdrSessionName,
				input.Agent.PaneID)
			if err := s.throwIfShutdownAbort(controller); err != nil {
				return nil, false, err
			}
			return nil, false, nil
		}

		if turn.Received {
			fresh, err := s.refreshedCompactHistory(input.Agent)
			if err != nil {
				return nil, false, err
			}
			closed, err := s.isPaneClosedFor(input.Agent)
			if err != nil {
				return nil, false, err
			}
			if closed {
				fmt.Fprintf(os.Stderr,
					"Herdsman skipping status event generation because pane closed after"+
						" turn signal (agentId=%s, herdrSessionName=%s, paneId=%s)\n",
					input.Agent.ID, input.Agent.HerdrSessionName, input.Agent.PaneID)
				return nil, false, nil
			}
			freshIsTerminal := isTerminalAssistant(fresh)
			expectedText := ""
			if turn.HasExpectedText {
				expectedText = trimSpaceString(turn.ExpectedText)
			}
			lastAssistantText := ""
			if fresh.LastAssistantMessage != nil {
				lastAssistantText = trimSpaceString(fresh.LastAssistantMessage.Text)
			}
			textMatches := expectedText == "" || strings.HasSuffix(lastAssistantText, expectedText)
			// A client-confirmed turn is a deliverable terminal state: `confirmed`
			// means Pi verified the final assistant message is on disk, so a
			// confirmed round is delivered straight through and is never marked
			// degraded (which is what made #runPlanRow run
			// invalidateById(..., "degraded_retry") on already-written content).
			confirmedTerminal := turn.Confirmed
			// M2: the "trust the on-disk body" judgement is decoupled from
			// `confirmed`. That the turn-end signal arrived is already guaranteed by
			// the enclosing `if (turn.Received)`, so the deliverability judgement
			// below only has to decide whether the disk carries a deliverable body —
			// a round no longer needs a (systematically unreliable) client-side
			// `confirmed` to be released. `confirmedTerminal` keeps its own meaning
			// for the released/degraded logging and payload.
			degradeOrRelease := func(degradedReason DegradedReason) map[string]any {
				if confirmedTerminal {
					// A confirmed round stays non-degraded (see above), but the reason
					// is no longer swallowed: this path used to release silently with
					// an emptied body, so an empty `agent.done` could only be
					// diagnosed by reading the pane by hand. `degraded: true` must
					// stay off here — #runPlanRow would `invalidateById(...,
					// "degraded_retry")` the row it has already written.
					s.logWarn(
						"Herdsman released a confirmed pi status event with no deliverable text",
						map[string]any{
							"agentId":          input.Agent.ID,
							"degradedReason":   string(degradedReason),
							"herdrSessionName": input.Agent.HerdrSessionName,
							"paneId":           input.Agent.PaneID,
							"planId":           planIDValue(input),
							"terminalId":       input.Agent.TerminalID,
						})
					// The confirmed path never degrades, so it is the one empty-body
					// release that the degraded line cannot cover: alert it here
					// (same per-pane suppression).
					s.noteEmptyTurnAlert(noteEmptyTurnAlertInput{
						AgentID:          input.Agent.ID,
						Attempts:         attempts,
						HerdrSessionName: input.Agent.HerdrSessionName,
						PaneID:           &input.Agent.PaneID,
						PlanID:           planIDValue(input),
					})
					return map[string]any{"staleSnapshot": false}
				}
				s.logDegradedRelease(input, degradedReason)
				return map[string]any{
					"degraded":       true,
					"degradedReason": string(degradedReason),
					"staleSnapshot":  false,
				}
			}
			switch {
			case confirmedTerminal && freshIsTerminal && textMatches &&
				HasNonEmptyAssistantMessage(fresh):
				compactHistory = fresh
			case !isAlreadyEmittedInRetry && freshIsTerminal && textMatches &&
				HasNonEmptyAssistantMessage(fresh) &&
				(isTerminalAssistant(input.CompactHistory) ||
					HistoryHasAdvanced(*fresh, input.CompactHistory, nil)):
				compactHistory = fresh
			default:
				advanced, err := s.waitForHistoryAdvance(waitForHistoryAdvanceInput{
					Agent:                  input.Agent,
					Baseline:               baseline,
					Controller:             controller,
					Initial:                fresh,
					MaxAttempts:            8,
					RequireAssistantChange: isRetry,
				})
				if err != nil {
					return nil, false, err
				}
				if advanced == nil {
					return nil, false, nil
				}
				if isRetry && !HistoryHasAdvanced(*advanced, baseline,
					&HistoryHasAdvancedOptions{RequireAssistantChange: true}) {
					return nil, false, &PlanWaitingHistoryError{}
				}
				// The guard compares `advanced` against the prior terminal rows for
				// this agent that can have reached the orchestrator — a bounded
				// window, newest first. It only counts rows in
				// `delivered`/`acked` that represent a deliverable terminal *round*:
				// a never-deliverable row (`agent.idle` from a non-`working` status,
				// per isDeliverableAgentEvent) can sit in the window and must not
				// blank a deliverable round.
				deliveredTerminalHistories, err := s.stores.AgentEvents.DeliveredTerminalHistories(
					DeliveredTerminalHistoriesInput{
						AgentID:          input.Agent.ID,
						HerdrSessionName: input.Agent.HerdrSessionName,
						Limit:            STALE_DUPLICATE_GUARD_SCAN_LIMIT,
					})
				if err != nil {
					return nil, false, err
				}
				if len(deliveredTerminalHistories) >= STALE_DUPLICATE_GUARD_SCAN_LIMIT {
					// A full window (`scanned >= 500`) means the newest
					// STALE_DUPLICATE_GUARD_SCAN_LIMIT delivered/acked terminal rows
					// came back with no room to spare, so any 501st or older row is
					// invisible to the `staleBaselineDuplicate` check below.
					s.noteGuardWindowSaturated(noteWindowSaturatedInput{
						AgentID:          input.Agent.ID,
						HerdrSessionName: input.Agent.HerdrSessionName,
						PaneID:           input.Agent.PaneID,
						Scanned:          len(deliveredTerminalHistories),
					})
				}
				staleBaselineDuplicate := false
				for _, row := range deliveredTerminalHistories {
					if sameTerminalAssistantContent(
						advanced, row.CompactHistory, stringPointer("pi"),
					) {
						staleBaselineDuplicate = true
						break
					}
				}
				// Whether this agent ever delivered a terminal row. This is an
				// existence question over the bounded window, not a content question.
				hasDeliveredTerminalRow := len(deliveredTerminalHistories) > 0
				// M2 deliverability judgement: the turn-end signal already arrived
				// (the enclosing `if (turn.Received)`), so only the disk matters — a
				// non-empty terminal assistant message that was not already delivered
				// (`diskDeliverable`) is this round's answer. The client's `confirmed`
				// flag is deliberately not a conjunct here.
				diskDeliverable := isTerminalAssistant(advanced) &&
					HasNonEmptyAssistantMessage(advanced) && !staleBaselineDuplicate
				if !isRetry && !HistoryHasAdvanced(*advanced, input.CompactHistory,
					&HistoryHasAdvancedOptions{RequireAssistantChange: true}) {
					// Frozen-baseline release. "No advance" therefore does not mean
					// "nothing to deliver": it usually means the answer is already in
					// place, and blanking it is what lost finished work. Release is
					// judged from the disk (`diskDeliverable`), never from `confirmed`.
					if diskDeliverable && (!hasDeliveredTerminalRow || confirmedTerminal) {
						// Trust the already-in-place answer: release it as a non-stale
						// snapshot and without a `degraded` marker.
						compactHistory = advanced
						payloadExtra = map[string]any{"staleSnapshot": false}
						s.logWarn(
							"Herdsman released a pi status event from a frozen baseline"+
								" that already holds the on-disk answer",
							map[string]any{
								"agentId":          input.Agent.ID,
								"confirmed":        confirmedTerminal,
								"herdrSessionName": input.Agent.HerdrSessionName,
								"paneId":           input.Agent.PaneID,
								"planId":           planIDValue(input),
								"terminalId":       input.Agent.TerminalID,
							})
					} else {
						blanked := *advanced
						blanked.LastAssistantMessage = nil
						compactHistory = &blanked
						if staleBaselineDuplicate {
							payloadExtra = degradeOrRelease(DegradedReasonStaleBaselineDup)
						} else {
							payloadExtra = degradeOrRelease(DegradedReasonNoAdvanceFromInput)
						}
					}
				} else {
					advancedText := ""
					if advanced.LastAssistantMessage != nil {
						advancedText = trimSpaceString(advanced.LastAssistantMessage.Text)
					}
					advancedMatchesExpected := expectedText == "" ||
						strings.HasSuffix(advancedText, expectedText)
					// When the guess disagrees the body is still released as-is —
					// `degraded` stays off, because `degraded: true` makes #runPlanRow
					// call invalidateById(..., "degraded_retry") on content that is
					// already written — and the mismatch is recorded as a warning.
					switch {
					case diskDeliverable:
						if !advancedMatchesExpected {
							message := "Herdsman accepted an unconfirmed pi status event" +
								" despite an expectedText mismatch"
							if confirmedTerminal {
								message = "Herdsman accepted a confirmed pi status event" +
									" despite an expectedText mismatch"
							}
							s.logWarn(message, map[string]any{
								"agentId":          input.Agent.ID,
								"confirmed":        confirmedTerminal,
								"degradedReason":   string(DegradedReasonExpectedTextMismatch),
								"herdrSessionName": input.Agent.HerdrSessionName,
								"paneId":           input.Agent.PaneID,
								"planId":           planIDValue(input),
								"terminalId":       input.Agent.TerminalID,
							})
							payloadExtra = map[string]any{"staleSnapshot": false}
						}
						compactHistory = advanced
					case !advancedMatchesExpected:
						blanked := *advanced
						blanked.LastAssistantMessage = nil
						compactHistory = &blanked
						payloadExtra = degradeOrRelease(DegradedReasonExpectedTextMismatch)
					case !isTerminalAssistant(advanced) || !HasNonEmptyAssistantMessage(advanced):
						// `diskDeliverable` is false in this arm by construction, so a
						// round with a deliverable disk body never reaches it and
						// `non_terminal_assistant` keeps its pre-existing meaning.
						blanked := *advanced
						blanked.LastAssistantMessage = nil
						compactHistory = &blanked
						payloadExtra = degradeOrRelease(DegradedReasonNonTerminalAssistant)
					case staleBaselineDuplicate:
						// M2 guard, mismatch arm: blank the body and keep the guard's
						// reason instead of releasing an older body a second time.
						blanked := *advanced
						blanked.LastAssistantMessage = nil
						compactHistory = &blanked
						payloadExtra = degradeOrRelease(DegradedReasonStaleBaselineDup)
					default:
						// Defensive arm, reached only if `diskDeliverable`'s definition
						// grows a conjunct: kept as a plain (non-degrading) release
						// fallback.
						compactHistory = advanced
					}
				}
			}
			fmt.Printf(
				"Herdsman emitted pi agent.%s after turn completion signal (confirmed=%t)"+
					" (agentId=%s, herdrSessionName=%s, terminalId=%s)\n",
				input.To, turn.Confirmed, input.Agent.ID, input.Agent.HerdrSessionName,
				stringOrEmpty(input.Agent.TerminalID))
		} else if !controller.Aborted() {
			advanced, err := s.waitForHistoryAdvance(waitForHistoryAdvanceInput{
				Agent:                  input.Agent,
				Baseline:               baseline,
				Controller:             controller,
				MaxAttempts:            8,
				RequireAssistantChange: isRetry,
			})
			if err != nil {
				return nil, false, err
			}
			if advanced == nil {
				return nil, false, nil
			}
			if isRetry && !HistoryHasAdvanced(*advanced, baseline,
				&HistoryHasAdvancedOptions{RequireAssistantChange: true}) {
				return nil, false, &PlanWaitingHistoryError{}
			}
			if !isRetry && !HistoryHasAdvanced(*advanced, input.CompactHistory,
				&HistoryHasAdvancedOptions{RequireAssistantChange: true}) {
				blanked := *advanced
				blanked.LastAssistantMessage = nil
				compactHistory = &blanked
				s.logDegradedRelease(input, DegradedReasonNoAdvanceFromInput)
				payloadExtra = map[string]any{
					"degraded":       true,
					"degradedReason": string(DegradedReasonNoAdvanceFromInput),
					"staleSnapshot":  false,
				}
			} else if !isTerminalAssistant(advanced) || !HasNonEmptyAssistantMessage(advanced) {
				blanked := *advanced
				blanked.LastAssistantMessage = nil
				compactHistory = &blanked
				s.logDegradedRelease(input, DegradedReasonNonTerminalAssistant)
				payloadExtra = map[string]any{
					"degraded":       true,
					"degradedReason": string(DegradedReasonNonTerminalAssistant),
					"staleSnapshot":  false,
				}
			} else {
				compactHistory = advanced
			}
			s.logWarn(
				fmt.Sprintf(
					"Herdsman emitted pi agent.%s without a turn completion signal after %dms",
					input.To, TurnSignalWaitMs),
				map[string]any{
					"agentId":          input.Agent.ID,
					"herdrSessionName": input.Agent.HerdrSessionName,
					"terminalId":       input.Agent.TerminalID,
				})
		}
	} else if agentKind != "pi" && (input.To == AgentStatusIdle || input.To == AgentStatusDone) {
		// Note: blocked is an interactive intermediate state, exempt from the ready
		// gate and the empty delivery gate.
		isAgyHistory := func(compact *CompactAgentHistory) bool {
			return (compact != nil && compact.Source != nil &&
				*compact.Source == string(HistorySourceAntigravitySQLite)) || agentKind == "agy"
		}
		// Only a terminal event that already represented a completed turn may
		// consume the assistant ref: a late startup idle (`unknown -> idle`), an
		// `agent.status.changed` row or a discarded plan must not make the following
		// `working -> done` plan look like a duplicate round.
		latestCompletedTurnRef := func() string {
			event, err := s.stores.AgentEvents.LatestCompletedTurnEvent(
				input.Agent.ID, input.Agent.HerdrSessionName)
			if err != nil || event == nil || event.CompactHistory == nil ||
				event.CompactHistory.LastAssistantMessage == nil {
				return ""
			}
			return event.CompactHistory.LastAssistantMessage.Ref
		}
		isReadyNonPi := func(compact *CompactAgentHistory) bool {
			if compact == nil || !HasNonEmptyAssistantMessage(compact) {
				return false
			}
			if isAgyHistory(compact) {
				previousRef := latestCompletedTurnRef()
				currentRef := ""
				if compact.LastAssistantMessage != nil {
					currentRef = compact.LastAssistantMessage.Ref
				}
				return currentRef != "" && currentRef != previousRef
			}
			latestTerminal, err := s.stores.AgentEvents.LatestTerminalEvent(
				input.Agent.ID, input.Agent.HerdrSessionName)
			if err != nil || latestTerminal == nil {
				return true
			}
			currentRef, currentText := assistantRefAndText(compact)
			previousRef, previousText := assistantRefAndText(latestTerminal.CompactHistory)
			refChanged := currentRef != previousRef
			textChanged := currentText != previousText
			same := sameTerminalAssistantContent(
				compact, latestTerminal.CompactHistory, input.Agent.Agent)
			return !same || refChanged || textChanged
		}
		// A non-empty assistant ref that was already delivered by a completed turn
		// is a genuine skip (the round was announced), never a reason to exhaust the
		// retry budget into `discarded`.
		isAlreadyDeliveredCompletedTurn := func(compact *CompactAgentHistory) bool {
			if compact == nil || !isAgyHistory(compact) || !HasNonEmptyAssistantMessage(compact) {
				return false
			}
			currentRef := ""
			if compact.LastAssistantMessage != nil {
				currentRef = compact.LastAssistantMessage.Ref
			}
			return currentRef != "" && currentRef == latestCompletedTurnRef()
		}

		latestTerminalForSkip, err := s.stores.AgentEvents.LatestTerminalEvent(
			input.Agent.ID, input.Agent.HerdrSessionName)
		if err != nil {
			return nil, false, err
		}
		if agentKind != "agy" &&
			(compactHistory == nil || compactHistory.Source == nil ||
				*compactHistory.Source != string(HistorySourceAntigravitySQLite)) &&
			input.HerdrEventKey != nil && latestTerminalForSkip != nil &&
			compactHistory != nil && HasNonEmptyAssistantMessage(compactHistory) {
			currentRef, currentText := assistantRefAndText(compactHistory)
			previousRef, previousText := assistantRefAndText(latestTerminalForSkip.CompactHistory)
			refChanged := currentRef != previousRef
			textChanged := currentText != previousText
			same := sameTerminalAssistantContent(
				compactHistory, latestTerminalForSkip.CompactHistory, input.Agent.Agent)
			if same && !refChanged && !textChanged {
				return nil, false, nil
			}
		}

		if !isReadyNonPi(compactHistory) {
			waitStartedAt := s.now() - TurnSignalWaitMs
			for attempt := 0; attempt < 8; attempt++ {
				if s.turnCompletions != nil && terminalID != "" {
					turnWaitController := newWaiterController()
					parentDone := controller.Done()
					stopForwarding := make(chan struct{})
					go func() {
						select {
						case <-parentDone:
							turnWaitController.Abort()
						case <-stopForwarding:
						}
					}()
					waitDone := make(chan TurnCompletionWaitResult, 1)
					go func() {
						waitDone <- s.turnCompletions.WaitForSignal(WaitForSignalInput{
							HerdrSessionName: input.Agent.HerdrSessionName,
							RecordedAfterMs:  waitStartedAt,
							Signal:           turnWaitController.Done(),
							TerminalID:       terminalID,
						})
					}()
					sleepDone := make(chan struct{})
					go func() {
						s.sleep(500, controller.Done())
						close(sleepDone)
					}()
					select {
					case <-waitDone:
					case <-sleepDone:
					}
					close(stopForwarding)
					turnWaitController.Abort()
				} else {
					s.sleep(500, controller.Done())
				}
				abortedOrClosed, err := s.abortedOrPaneClosed(controller)
				if err != nil {
					return nil, false, err
				}
				if abortedOrClosed {
					fmt.Fprintf(os.Stderr,
						"Herdsman skipping status event generation because wait was aborted"+
							" or pane closed (aborted=%t, agentId=%s, herdrSessionName=%s,"+
							" paneId=%s)\n",
						controller.Aborted(), input.Agent.ID, input.Agent.HerdrSessionName,
						input.Agent.PaneID)
					if err := s.throwIfShutdownAbort(controller); err != nil {
						return nil, false, err
					}
					return nil, false, nil
				}
				refreshed, refreshErr := s.context.RefreshAgent(RefreshAgentContextInput{
					Agent:           input.Agent,
					ForceRefresh:    boolPointer(true),
					IdentityChanged: false,
				})
				if refreshErr != nil {
					fmt.Fprintf(os.Stderr,
						"Herdsman failed to refresh agent during ready wait (%v)\n", refreshErr)
				} else {
					compactHistory = &refreshed.Snapshot.CompactHistory
				}
				if isReadyNonPi(compactHistory) {
					break
				}
			}
		}

		if !isReadyNonPi(compactHistory) {
			if isAlreadyDeliveredCompletedTurn(compactHistory) {
				ref := ""
				if compactHistory != nil && compactHistory.LastAssistantMessage != nil {
					ref = compactHistory.LastAssistantMessage.Ref
				}
				fmt.Fprintf(os.Stderr,
					"Herdsman skipping non-pi terminal status event because the assistant"+
						" ref was already delivered by a completed turn (agent=%s,"+
						" agentId=%s, from=%s, ref=%s, to=%s)\n",
					agentKind, input.Agent.ID, input.From, ref, input.To)
				return nil, false, nil
			}
			return nil, false, &PlanWaitingHistoryError{}
		}
	}

	abortedOrClosed, err := s.abortedOrPaneClosed(controller)
	if err != nil {
		return nil, false, err
	}
	if abortedOrClosed {
		fmt.Fprintf(os.Stderr,
			"Herdsman skipping status event generation because turn wait was aborted or"+
				" pane closed before append (aborted=%t, agentId=%s, herdrSessionName=%s,"+
				" paneId=%s)\n",
			controller.Aborted(), input.Agent.ID, input.Agent.HerdrSessionName,
			input.Agent.PaneID)
		if err := s.throwIfShutdownAbort(controller); err != nil {
			return nil, false, err
		}
		return nil, false, nil
	}

	currentAgent, err := s.stores.Agents.FindByPane(FindAgentByPaneInput{
		HerdrSessionName: input.Agent.HerdrSessionName,
		PaneID:           input.Agent.PaneID,
		PaneGeneration:   input.Agent.PaneGeneration,
	})
	if err != nil {
		return nil, false, err
	}
	if currentAgent == nil {
		// The agent row is gone (pane retired mid-execution): appending would create
		// a dangling event with a dead agent_id that only the reconciler could sweep,
		// so no terminal row is appended here. `#runPlanRow` settles the plan from
		// here — salvaging its terminal content as an orphan event when there is any,
		// cancelling otherwise (the skip->completed branch must stay reserved for
		// genuine skips).
		fmt.Fprintf(os.Stderr,
			"Herdsman refusing to append a terminal status event because the agent row"+
				" is gone (agentId=%s, herdrSessionName=%s, paneId=%s, from=%s, to=%s)\n",
			input.Agent.ID, input.Agent.HerdrSessionName, input.Agent.PaneID,
			input.From, input.To)
		return nil, true, nil
	}
	targetAgent := *currentAgent
	if currentAgent.AgentStatus != input.To {
		currentAgentKind := ""
		if currentAgent.Agent != nil {
			currentAgentKind = *currentAgent.Agent
		}
		if input.To == AgentStatusDone || input.To == AgentStatusBlocked ||
			(input.To == AgentStatusIdle && currentAgentKind == "agy") {
			s.logInfo("Herdsman emitting terminal status event after subsequent status change",
				map[string]any{
					"agentId":          input.Agent.ID,
					"current":          string(currentAgent.AgentStatus),
					"expected":         string(input.To),
					"herdrSessionName": input.Agent.HerdrSessionName,
					"paneId":           input.Agent.PaneID,
				})
			targetAgent = *currentAgent
		} else {
			fmt.Fprintf(os.Stderr,
				"Herdsman skipping status event generation due to status mismatch"+
					" (agentId=%s, current=%s, expected=%s, herdrSessionName=%s, paneId=%s)\n",
				input.Agent.ID, currentAgent.AgentStatus, input.To,
				input.Agent.HerdrSessionName, input.Agent.PaneID)
			return nil, false, nil
		}
	}

	eventPayload := map[string]any{}
	for key, value := range payload(targetAgent, input.From, input.To) {
		eventPayload[key] = value
	}
	for key, value := range payloadExtra {
		eventPayload[key] = value
	}
	legacyObservation := "legacy"
	if input.HerdrEventKey != nil {
		legacyObservation = *input.HerdrEventKey
	}
	var lastEvent *AgentEventRecord
	if !skipStatusChanged {
		lastEvent = s.appendAndAckSelfEvent(AppendAgentEventInput{
			AgentID:          &targetAgent.ID,
			CompactHistory:   compactHistory,
			HerdrSessionName: targetAgent.HerdrSessionName,
			IdempotencyKey: stringPointer(idempotencyKey(
				AgentEventStatusChanged, targetAgent, input.From, input.To,
				fmt.Sprintf("%s:%s:agent.status.changed", observationID, legacyObservation))),
			PaneID:         stringPointer(targetAgent.PaneID),
			PaneGeneration: targetAgent.PaneGeneration,
			Payload:        eventPayload,
			TerminalID:     targetAgent.TerminalID,
			Type:           AgentEventStatusChanged,
			WorkspaceID:    stringPointer(targetAgent.WorkspaceID),
		})
	}
	statusType := statusEventType(input.To)
	if statusType != nil {
		return s.appendAndAckSelfEvent(AppendAgentEventInput{
			AgentID:          &targetAgent.ID,
			CompactHistory:   compactHistory,
			HerdrSessionName: targetAgent.HerdrSessionName,
			IdempotencyKey: stringPointer(idempotencyKey(
				*statusType, targetAgent, input.From, input.To,
				fmt.Sprintf("%s:%s:%s", observationID, legacyObservation, *statusType))),
			PaneID:         stringPointer(targetAgent.PaneID),
			PaneGeneration: targetAgent.PaneGeneration,
			Payload:        eventPayload,
			TerminalID:     targetAgent.TerminalID,
			Type:           *statusType,
			WorkspaceID:    stringPointer(targetAgent.WorkspaceID),
		}), false, nil
	}
	return lastEvent, false, nil
}

// ---------------------------------------------------------------------------
// Helpers local to #appendStatusEvents
// ---------------------------------------------------------------------------

func (s *AgentIndexService) isPaneClosedFor(agent AgentIndexRecord) (bool, error) {
	return s.stores.Agents.IsPaneClosed(IsPaneClosedInput{
		HerdrSessionName: agent.HerdrSessionName,
		PaneID:           agent.PaneID,
		PaneGeneration:   agent.PaneGeneration,
	})
}

// abortedOrPaneClosed mirrors the repeated `controller.signal.aborted ||
// isPaneClosed(...)` guard.
func (s *AgentIndexService) abortedOrPaneClosed(controller *waiterController) (bool, error) {
	if controller.Aborted() {
		return true, nil
	}
	closed, err := s.isPaneClosedFor(controller.agent)
	if err != nil {
		return false, err
	}
	return closed, nil
}

func (s *AgentIndexService) refreshedCompactHistory(agent AgentIndexRecord) (*CompactAgentHistory, error) {
	result, err := s.context.RefreshAgent(RefreshAgentContextInput{
		Agent:           agent,
		ForceRefresh:    boolPointer(true),
		IdentityChanged: false,
	})
	if err != nil {
		return nil, err
	}
	return &result.Snapshot.CompactHistory, nil
}

func compactHistoryOf(event *AgentEventRecord) *CompactAgentHistory {
	if event == nil {
		return nil
	}
	return event.CompactHistory
}

func latestEventID(event *AgentEventRecord) int64 {
	if event == nil {
		return 0
	}
	return event.ID
}

// assistantRefAndText mirrors the `currentMsg?.ref ?? null` /
// `currentMsg?.text ?? ""` pair the non-Pi readiness gate compares.
// removeString mirrors `occupied.delete(...)` over the TS Set: the current
// agent's own path is excluded from the occupied set it is checked against.
func removeString(values []string, target string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == target {
			continue
		}
		result = append(result, value)
	}
	return result
}

// stringOrEmpty renders a *string as the TS `${terminalId}` would: the empty
// string for an unset value.
func stringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// trimSpaceString mirrors the TS `.trim()` on the compared texts: a trailing
// newline must not read as a mismatch.
func trimSpaceString(value string) string { return strings.TrimSpace(value) }

// parseEventTimeMs mirrors the TS `new Date(createdAt).getTime()` used as the
// fallback timestamp evidence in #isAgentStalled. An unparseable createdAt is
// treated as 0, exactly like `getTime()` on an invalid date.
func parseEventTimeMs(value string) int64 {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		parsed, err = time.Parse(time.RFC3339, value)
		if err != nil {
			return 0
		}
	}
	return parsed.UnixMilli()
}

func assistantRefAndText(compact *CompactAgentHistory) (string, string) {
	if compact == nil || compact.LastAssistantMessage == nil {
		return "", ""
	}
	return compact.LastAssistantMessage.Ref, compact.LastAssistantMessage.Text
}
