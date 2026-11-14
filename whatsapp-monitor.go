package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WhatsAppGraphBaseURL is the versioned Graph API root used for the Cloud API
// Messages endpoint. Pinned here so a version bump is a single-line change.
const WhatsAppGraphBaseURL = "https://graph.facebook.com/v27.0"

type whatsappTextPayload struct {
	PreviewURL bool   `json:"preview_url"`
	Body       string `json:"body"`
}

// whatsappReply is one status query answer, addressed to the participant who
// asked it. Meta reports a participant by phone number in `from` and omits the
// number when the participant has adopted a username, leaving only the
// business-scoped user id in `from_user_id`, so an inbound query carries one or
// the other.
type whatsappReply struct {
	Text   string
	Phone  string
	UserID string
}

// whatsappSendPayload is the request body of the Cloud API Messages endpoint,
// restricted to a 1:1 text message (the only shape this service sends). Meta
// documents `to` as the recipient's phone number and `recipient` as the
// business-scoped user id to send to instead when the number is unavailable, so
// publishLocked fills exactly one of them.
type whatsappSendPayload struct {
	MessagingProduct string              `json:"messaging_product"`
	RecipientType    string              `json:"recipient_type"`
	To               string              `json:"to,omitempty"`
	Recipient        string              `json:"recipient,omitempty"`
	Type             string              `json:"type"`
	Text             whatsappTextPayload `json:"text"`
}

type whatsappSendResponse struct {
	Messages []struct {
		ID string `json:"id"`
		// MessageStatus is only present for template messages; a text message
		// response carries no status, so it is not validated.
		MessageStatus string `json:"message_status"`
	} `json:"messages"`
}

// whatsappPublisher sends 1:1 text answers to the participants who status-query
// the number through the webhook. The webhook handler answers a query through it,
// and the responder re-sends the answer a retryable failure left queued. It is
// used from the responder goroutine and from the handler goroutines net/http
// starts per request, so its mutable state is guarded by mu.
//
// The reply is free-form (service) text, which Meta permits only inside the 24h
// customer service window that opens and resets with each message a participant
// sends. The query that triggers a reply is that participant's own message, so
// the reply goes out in the window the asker opened and is expected to be
// accepted. 131047 reports a window that has already passed; only a new message
// from the participant reopens it, so a reply rejected with it is not retried. A
// pre-approved template is the documented way to send outside a window; it is not
// implemented, because nothing here sends outside a window and a template needs
// approval in the Meta console.
//
// Service messages are charged per delivered recipient since 2026-10-01, and are
// free only while the phone number is inside its 1,000-message monthly service
// tier. Meta stops delivering service messages for a Messaging account with no
// payment method on file, which it required by 2026-09-30, so 131042 is an
// account-side condition a retry cannot clear.
//
// Reference for the rules and the codes quoted in this file:
// https://developers.facebook.com/documentation/business-messaging/whatsapp/messages/send-messages.md
// https://developers.facebook.com/documentation/business-messaging/whatsapp/pricing.md
// https://developers.facebook.com/documentation/business-messaging/whatsapp/business-scoped-user-ids.md
// https://developers.facebook.com/documentation/business-messaging/whatsapp/support/error-codes.md
type whatsappPublisher struct {
	mu            sync.Mutex
	client        *http.Client
	baseURL       string
	accessToken   string
	phoneNumberID string
	// pacingUntil is the earliest time the next request may be sent after a 429.
	// The flow-control limit is per phone number, so one deadline paces every
	// subsequent attempt, and it is kept in the publisher: the responder drives
	// retries at its own 1-minute cadence, which must not outrun the header.
	pacingUntil time.Time
	// pendingReply is the status query answer a retryable failure left queued for
	// the responder to send again, including the identifier of the participant who
	// asked, so the retry addresses the same 1:1 thread. A successful send clears
	// it. A terminal failure clears it too, because repeating a request Meta
	// rejected sends nothing. A newer query replaces the queued answer: the reply
	// states the current time, so the newest question is the answer worth sending,
	// and the older one is dropped.
	pendingReply whatsappReply
}

// newWhatsAppPublisher returns nil when WhatsApp answering is not configured
// (no access token or phone number ID), mirroring how the Yandex Disk worker is
// skipped when its token is missing. Partial configuration is logged instead of
// silently ignored: a half-configured WhatsApp section is usually an account-side
// problem the operator has to resolve, such as a system user token that was never
// created in the Meta developer app.
func newWhatsAppPublisher(secrets *Secrets) *whatsappPublisher {
	if secrets.WhatsAppAccessToken != "" && secrets.WhatsAppPhoneNumberID != "" {
		return &whatsappPublisher{
			client:        &http.Client{Timeout: time.Minute},
			baseURL:       WhatsAppGraphBaseURL,
			accessToken:   secrets.WhatsAppAccessToken,
			phoneNumberID: secrets.WhatsAppPhoneNumberID,
		}
	}

	if secrets.WhatsAppAccessToken != "" || secrets.WhatsAppPhoneNumberID != "" {
		log.Println("Warning: WhatsApp is partially configured, so WhatsApp answering is disabled")
	}

	return nil
}

// maxPacing bounds a numeric Retry-After so the time.Duration multiplication
// cannot overflow, and an implausible deadline cannot stall the reply send for
// days.
const maxPacing = time.Hour

// setPacing records how long the phone number must wait after a 429. Meta sends
// Retry-After as seconds; an unparseable value falls back to RetryDelay, the
// pacing the Telegram retries use, so an unexpected header cannot start a tight
// retry loop. The value is parsed as 64-bit and capped before narrowing, so a
// header that no 32-bit int can hold is still capped at maxPacing instead of
// falling back to the shorter RetryDelay.
func (p *whatsappPublisher) setPacing(retryAfter string) {
	delay := RetryDelay
	if seconds, err := strconv.ParseInt(retryAfter, 10, 64); err == nil && seconds > 0 {
		if seconds > int64(maxPacing/time.Second) {
			seconds = int64(maxPacing / time.Second)
		}
		delay = time.Duration(seconds) * time.Second
	}
	p.pacingUntil = time.Now().Add(delay)
}

// readyLocked reports whether a request may be sent now, that is whether a
// pacing deadline recorded from an earlier 429 has elapsed. It is called under
// the publisher lock by publishReply, the only send path this service has.
func (p *whatsappPublisher) readyLocked() bool {
	return p.pacingUntil.IsZero() || !time.Now().Before(p.pacingUntil)
}

// sendOutcome is the result of one send request. retryable reports whether
// repeating the request can produce a different answer, which is the question
// the retry queue needs: a request Meta rejected for auth, billing, policy, or the
// customer service window produces the same answer every time, so repeating it
// sends nothing. A failure before the request reaches the network is terminal for
// the same reason: payload encoding and request construction are properties of
// this configuration, so the local error repeats on every attempt.
type sendOutcome struct {
	retryable bool
	err       error
}

// permanentSendStatus reports whether an HTTP status is Meta rejecting the
// request itself. 429 and 408 are the 4xx whose semantics ask for a repeat: 429
// carries the delay Meta requires, and 408 says the server timed out reading the
// request. Meta's Graph API does not send 408, so the case is the tunnel, proxy,
// or edge in front of it. Every other 4xx is terminal: auth, billing, policy, and
// window rejections get the same answer on every repeat. 5xx and network failures
// are Meta-side conditions, so they are worth repeating at the 1-minute cadence
// the responder's retry uses.
func permanentSendStatus(status int) bool {
	return status >= 400 && status < 500 &&
		status != http.StatusTooManyRequests &&
		status != http.StatusRequestTimeout
}

// publishReply sends the reply under the pacing gate, and leaves the answer
// queued when the failure is worth repeating so the responder sends it again. A
// successful send clears the queue, and a terminal failure clears it with the
// answer: repeating a request Meta rejected sends nothing. A newer query replaces
// the queued answer, including its recipient, because the reply states the current
// time and the newest question is the answer worth sending: an earlier query from
// another participant is dropped rather than answered. The results report whether
// the request was attempted, whether a failure is worth repeating, and the failure.
func (p *whatsappPublisher) publishReply(reply whatsappReply) (attempted, retryable bool, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if !p.readyLocked() {
		// Inside a Retry-After window recorded from an earlier 429: no request is
		// made, and the answer stays queued for the responder's next attempt.
		p.pendingReply = reply

		return false, true, nil
	}

	outcome := p.publishLocked(reply)
	if outcome.err == nil || !outcome.retryable {
		p.pendingReply = whatsappReply{}
	} else {
		p.pendingReply = reply
	}

	return true, outcome.retryable, outcome.err
}

// takePendingReply reports the status query answer queued for the responder's
// next attempt. Its Text is empty when nothing is queued.
func (p *whatsappPublisher) takePendingReply() whatsappReply {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.pendingReply
}

// reportReplyOutcome reports what one status query answer attempt did. The handler
// and the responder share this wording, so a queued answer is reported the same
// way at both sites.
func reportReplyOutcome(attempted, retryable bool, err error) {
	switch {
	case err != nil && retryable:
		log.Printf("Failed to answer a WhatsApp status query, so the answer is sent again: %v", err)
	case err != nil:
		log.Printf("Failed to answer a WhatsApp status query, so it is not sent again: %v", err)
	case !attempted:
		log.Printf("Status query answer is deferred by the Retry-After pacing window")
	}
}

func (p *whatsappPublisher) publishLocked(reply whatsappReply) sendOutcome {
	payload := whatsappSendPayload{
		MessagingProduct: "whatsapp",
		RecipientType:    "individual",
		Type:             "text",
		Text:             whatsappTextPayload{PreviewURL: false, Body: reply.Text},
	}
	switch {
	case reply.Phone != "":
		payload.To = reply.Phone
	case reply.UserID != "":
		// Meta omits the phone number when the participant has adopted a username,
		// so the business-scoped user id is the only identifier to answer.
		payload.Recipient = reply.UserID
	default:
		// Neither identifier reached the publisher, so no request can address
		// anyone: repeating it produces the same local error.
		return sendOutcome{
			retryable: false,
			err:       fmt.Errorf("WhatsApp status query carries neither a phone number nor a business-scoped user id"),
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return sendOutcome{retryable: false, err: fmt.Errorf("error encoding WhatsApp message payload: %w", err)}
	}

	request, err := http.NewRequest(
		http.MethodPost,
		p.baseURL+"/"+p.phoneNumberID+"/messages",
		bytes.NewBuffer(encoded),
	)
	if err != nil {
		// The request cannot be built from this configuration, so repeating it
		// gets the same local error: a phone number id with URL control
		// characters fails at url.Parse on every attempt.
		return sendOutcome{retryable: false, err: fmt.Errorf("error creating WhatsApp send request: %w", err)}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+p.accessToken)

	response, err := p.client.Do(request)
	if err != nil {
		// A transport failure says nothing about the request, so the send is
		// worth repeating: this is the path a proxy, a tunnel, or Meta's edge
		// dropping a connection takes.
		return sendOutcome{retryable: true, err: fmt.Errorf("error sending WhatsApp request: %w", err)}
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		// 429 responses carry the pacing delay Meta requires before the next
		// request; surfacing it distinguishes flow control from auth errors.
		retryAfter := response.Header.Get("Retry-After")
		if response.StatusCode == http.StatusTooManyRequests {
			// Any 429 paces the publisher, including one without the header.
			p.setPacing(retryAfter)
		}
		// Meta documents Cloud API errors as synchronous, asynchronous, or both,
		// and recommends handling them by code, so the code in the envelope
		// overrides the HTTP status rule: a transient code is worth repeating in
		// the response too, and 429 paces the publisher as before.
		retryable := !permanentSendStatus(response.StatusCode) || transientSendCodes[metaErrorCode(body)]
		if retryAfter != "" {
			return sendOutcome{
				retryable: retryable,
				err:       fmt.Errorf("WhatsApp send returned %s (Retry-After %s): %s", response.Status, retryAfter, strings.TrimSpace(string(body))),
			}
		}

		return sendOutcome{
			retryable: retryable,
			err:       fmt.Errorf("WhatsApp send returned %s: %s", response.Status, strings.TrimSpace(string(body))),
		}
	}

	var sendResponse whatsappSendResponse
	if err := json.NewDecoder(response.Body).Decode(&sendResponse); err != nil {
		// A 200 body that does not parse is a response-shape problem, not a
		// rejection, so the send is worth repeating.
		return sendOutcome{retryable: true, err: fmt.Errorf("error parsing WhatsApp send response: %w", err)}
	}
	if len(sendResponse.Messages) == 0 {
		log.Println("Warning: WhatsApp send response contains no messages")

		return sendOutcome{retryable: true, err: fmt.Errorf("WhatsApp send response contains no messages")}
	}
	if sendResponse.Messages[0].ID == "" {
		log.Println("Warning: WhatsApp send response has no message id")

		return sendOutcome{retryable: true, err: fmt.Errorf("WhatsApp send response has no message id")}
	}

	return sendOutcome{retryable: true}
}

// runWhatsappResponder serves the Cloud API webhook endpoint and re-sends the
// status query answers a retryable failure left queued. The handler answers
// queries; this loop only schedules retries. main starts the responder when a
// publisher exists and keeps no WhatsApp state of its own.
func runWhatsappResponder(publisher *whatsappPublisher, server *webhookServer) {
	if server == nil {
		// No webhook, so no query can arrive and nothing can be queued.
		return
	}

	listener, err := net.Listen("tcp", server.listen)
	if err != nil {
		// Nothing arrives on a socket that is not bound, so no answer can be
		// queued and no retry is left to schedule.
		log.Printf("Warning: webhook server is not serving: %v", err)

		return
	}

	// Serving blocks, so it runs beside the retry loop.
	go func() {
		if err := server.serve(listener); err != nil {
			log.Printf("Warning: webhook server stopped serving: %v", err)
		}
	}()

	ticker := time.NewTicker(RetryDelay)
	for range ticker.C {
		if reply := publisher.takePendingReply(); reply.Text != "" {
			attempted, retryable, err := publisher.publishReply(reply)
			reportReplyOutcome(attempted, retryable, err)
		}
	}
}
