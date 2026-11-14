package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// webhookMaxPayload is Meta's documented payload ceiling. The read limit
	// keeps a hostile request from growing the process.
	webhookMaxPayload = 3 << 20
	// webhookRetryWindow is how long Meta retries an unacknowledged webhook.
	// State entries older than that can no longer be re-delivered, so they are
	// dropped when the maps are written.
	webhookRetryWindow = 7 * 24 * time.Hour
	// signaturePrefix is the X-Hub-Signature-256 scheme Meta sends.
	signaturePrefix = "sha256="
)

type webhookStatusRecord struct {
	ID              string         `json:"id"`
	Status          string         `json:"status"`
	RecipientID     string         `json:"recipient_id"`
	RecipientUserID string         `json:"recipient_user_id"`
	Errors          []webhookError `json:"errors"`

	// Observed is the time the handler saw the status. It is the trim key: Meta's
	// own trigger time arrives under "timestamp" as a Unix timestamp string, and
	// is deliberately not parsed, because a time.Time field named timestamp makes
	// json.Unmarshal reject the payload, and the handler has already acknowledged
	// the delivery with 200, so Meta does not retry it and the status is lost.
	Observed time.Time `json:"-"`
}

// participant identifies the WhatsApp user whose device reported the status. Meta
// documents recipient_user_id (the business-scoped user id) as the identifier to
// use: recipient_id is the phone number and is omitted when the reply was sent to
// a BSUID and the participant has adopted a username, and recipient_user_id is
// omitted for a failed status reported for a reply sent to a phone number. Either
// identifier names the same participant, so the user id is preferred and the phone
// number is the fallback.
func (r webhookStatusRecord) participant() string {
	if r.RecipientUserID != "" {
		return r.RecipientUserID
	}

	return r.RecipientID
}

// statusKey is the state map key of a status. Both recipient identifiers are part
// of the key because either can be omitted: keying on one of them alone would make
// the same transition arrive under two keys depending on which fields Meta sent.
// The separator is a control character that cannot occur in a message id or BSUID,
// so the key is unambiguous.
func (r webhookStatusRecord) statusKey() string {
	return r.ID + "\x1f" + r.Status + "\x1f" + r.RecipientID + "\x1f" + r.RecipientUserID
}

type webhookError struct {
	// Code is kept as raw JSON because json.Unmarshal rejects the whole payload
	// when a field's type does not match the shape it arrives in, and the handler
	// has already acknowledged the delivery with 200 before parsing it, so the
	// rejection loses the delivery instead of causing a retry. A code typed as a
	// number therefore drops a payload whose code arrives quoted: codeValue accepts
	// both shapes.
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

// codeValue reports the error code, accepting both shapes Meta sends. It is 0
// when the code is absent or is not a number in either shape.
func (e webhookError) codeValue() int {
	if len(e.Code) == 0 {
		return 0
	}

	var number int
	if err := json.Unmarshal(e.Code, &number); err == nil {
		return number
	}

	var text string
	if err := json.Unmarshal(e.Code, &text); err == nil {
		if parsed, err := strconv.ParseInt(text, 10, 64); err == nil {
			return int(parsed)
		}
	}

	return 0
}

// webhookMessageRecord is one inbound message. Meta reports the sender in `from`
// as a phone number and in `from_user_id` as a business-scoped user id, and omits
// `from` when the sender has adopted a username. `group_id` marks a message sent
// in a group conversation, which this service does not answer.
type webhookMessageRecord struct {
	From       string `json:"from"`
	FromUserID string `json:"from_user_id"`
	GroupID    string `json:"group_id"`
	ID         string `json:"id"`
	Type       string `json:"type"`
	Text       struct {
		Body string `json:"body"`
	} `json:"text"`
}

type webhookChangeRecord struct {
	Field string `json:"field"`
	Value struct {
		MessagingProduct string `json:"messaging_product"`
		Metadata         struct {
			PhoneNumberID string `json:"phone_number_id"`
		} `json:"metadata"`
		Statuses []webhookStatusRecord  `json:"statuses"`
		Messages []webhookMessageRecord `json:"messages"`
	} `json:"value"`
}

type webhookPayload struct {
	Object string `json:"object"`
	Entry  []struct {
		ID      string                `json:"id"`
		Changes []webhookChangeRecord `json:"changes"`
	} `json:"entry"`
}

// webhookState keeps inbound message ids for deduplication and the delivery
// status transitions it has logged. net/http serves each request in its own
// goroutine, so handler invocations can overlap, and every access is locked.
//
// The state is in-memory and a restart drops it. Meta retries a webhook only when
// the endpoint did not answer 200, and the handler writes the acknowledgement
// before it processes the payload, so a delivery arriving in a new process is one
// this process never acknowledged, which has to be processed again. A persisted
// set cannot deduplicate any delivery: an entry is written only after the
// acknowledgement was sent, and Meta does not re-send that delivery. The in-memory
// set covers the duplicates Meta does send: batched payloads, and retried
// deliveries arriving inside the lifetime of one process.
type webhookState struct {
	mu          sync.Mutex
	seenInbound map[string]time.Time
	statuses    map[string]webhookStatusRecord
}

func newWebhookState() *webhookState {
	return &webhookState{
		seenInbound: make(map[string]time.Time),
		statuses:    make(map[string]webhookStatusRecord),
	}
}

// recordInbound records an inbound message id for deduplication. It returns false
// when the id was already processed, which deduplicates Meta's batched payloads
// and retried deliveries.
func (s *webhookState) recordInbound(wamid string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, seen := s.seenInbound[wamid]; seen {
		return false
	}
	s.seenInbound[wamid] = at
	s.trimLocked()

	return true
}

// noteStatus records a delivery status and returns true when it is new, so the
// handler logs each transition once. The key is the transition key, so a
// re-delivered status webhook is deduplicated, while the same status reported by
// another participant is a new event.
func (s *webhookState) noteStatus(record webhookStatusRecord, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := record.statusKey()
	if _, seen := s.statuses[key]; seen {
		return false
	}
	record.Observed = at
	s.statuses[key] = record
	s.trimLocked()

	return true
}

// trimLocked drops entries Meta can no longer re-deliver, so the maps do not grow
// without a bound. Inbound ids are keyed by the time the handler saw them, and
// statuses by the handler's observation time, which covers outbound messages
// whose ids never appear in the inbound map.
func (s *webhookState) trimLocked() {
	cutoff := time.Now().Add(-webhookRetryWindow)
	for wamid, at := range s.seenInbound {
		if at.Before(cutoff) {
			delete(s.seenInbound, wamid)
		}
	}
	for key, record := range s.statuses {
		if record.Observed.Before(cutoff) {
			delete(s.statuses, key)
		}
	}
}

// webhookServer serves the Cloud API webhook endpoint. TLS is terminated in
// front of it (Cloudflare Tunnel), so it listens on plain HTTP on a loopback
// address. The endpoint must be reachable from the internet with a valid
// certificate at the registered Callback URL.
type webhookServer struct {
	appSecret     string
	verifyToken   string
	listen        string
	phoneNumberID string
	statusRegexp  *regexp.Regexp
	publisher     *whatsappPublisher
	state         *webhookState
}

// newWebhookServer returns nil when the webhook is not fully configured. With
// an empty listen address, ListenAndServe would bind to port 80, so no webhook
// fields means no server. Missing fields are logged in every case. The caller
// passes a configured publisher: the phone number ID and the send path both
// come from it, so a webhook without a send path cannot answer a query. The
// state is built here: the handler is the only reader and writer, so the maps
// have no consumer outside the handler goroutines.
func missingWebhookFields(secrets *Secrets, settings *Settings) []string {
	var missing []string
	if secrets.WhatsAppAppSecret == "" {
		missing = append(missing, "whatsapp_app_secret")
	}
	if secrets.WebhookVerifyToken == "" {
		missing = append(missing, "webhook_verify_token")
	}
	if settings.WebhookListen == "" {
		missing = append(missing, "webhook_listen")
	}

	return missing
}

func newWebhookServer(secrets *Secrets, settings *Settings, publisher *whatsappPublisher) *webhookServer {
	missing := missingWebhookFields(secrets, settings)
	if len(missing) != 0 {
		log.Printf("Warning: webhook server is disabled: missing required configuration: %s", strings.Join(missing, ", "))
		return nil
	}

	statusRegexp, err := regexp.Compile(settings.StatusKeywords)
	if err != nil {
		log.Printf("Warning: invalid status_keywords regexp, so the webhook server is disabled: %v", err)
		return nil
	}

	return &webhookServer{
		appSecret:     secrets.WhatsAppAppSecret,
		verifyToken:   secrets.WebhookVerifyToken,
		listen:        settings.WebhookListen,
		phoneNumberID: publisher.phoneNumberID,
		statusRegexp:  statusRegexp,
		publisher:     publisher,
		state:         newWebhookState(),
	}
}

// handler routes the registered webhook path. The path is what the Callback URL
// in the Meta app must point at.
func (s *webhookServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/webhook", s.handle)

	return mux
}

// serve serves the endpoint on a listener the caller bound, so the caller's log
// distinguishes a bind failure from a serving failure. It returns the reason it
// stopped serving.
func (s *webhookServer) serve(l net.Listener) error {
	// The server faces the internet through the tunnel, so connections have to
	// be bounded: without these timeouts a slow client can hold a connection and
	// a handler goroutine. The values are explicit, not defaults, because the
	// payload ceiling is 3 MB and Meta's posts are small and fast.
	// WriteTimeout starts when the request headers are read, so it has to leave
	// room after a body that takes most of the read budget: the acknowledgement
	// is written after the body is read.
	server := &http.Server{
		Addr:              s.listen,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("Webhook server listening on %s", l.Addr())
	if err := server.Serve(l); err != nil {
		return fmt.Errorf("webhook server on %s: %w", s.listen, err)
	}

	return nil
}

func (s *webhookServer) handle(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.verify(w, r)
		return
	case http.MethodPost:
		// Fall through to the payload handling below.
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	// Reading one byte past the documented ceiling detects an oversized body
	// instead of verifying and processing a truncated prefix.
	body, err := io.ReadAll(io.LimitReader(r.Body, webhookMaxPayload+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > webhookMaxPayload {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if !s.signatureValid(body, r.Header.Get("X-Hub-Signature-256")) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	// Meta retries a webhook that is not acknowledged with 200, so acknowledge
	// before doing any work. Writing the header is not enough: it stays in the
	// connection buffer until the handler returns, which is after the deduplication
	// maps are updated and the reply send, so the acknowledgement has to be flushed.
	w.WriteHeader(http.StatusOK)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	s.process(body)
}

// verify answers the hub.challenge handshake Meta sends whenever the Callback
// URL or Verify token is set in the App Dashboard.
func (s *webhookServer) verify(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if query.Get("hub.mode") == "subscribe" && s.verifyToken != "" && query.Get("hub.verify_token") == s.verifyToken {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, query.Get("hub.challenge"))
		return
	}
	w.WriteHeader(http.StatusForbidden)
}

// signatureValid compares X-Hub-Signature-256 with the HMAC-SHA256 of the body
// keyed by the app secret. hmac.Equal keeps the comparison constant time.
func (s *webhookServer) signatureValid(body []byte, header string) bool {
	if s.appSecret == "" || !strings.HasPrefix(header, signaturePrefix) {
		return false
	}

	want, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil {
		return false
	}

	mac := hmac.New(sha256.New, []byte(s.appSecret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

func (s *webhookServer) process(body []byte) {
	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		log.Printf("Error parsing webhook payload: %v", err)
		return
	}
	if payload.Object != "whatsapp_business_account" {
		return
	}

	for _, entry := range payload.Entry {
		for _, change := range entry.Changes {
			// Payloads from other products, and from phone numbers this service
			// does not answer through, are not ours to handle.
			if change.Value.MessagingProduct != "whatsapp" {
				continue
			}
			if change.Value.Metadata.PhoneNumberID != s.phoneNumberID {
				continue
			}

			// The messages field is the only one this service acts on. The other
			// fields an app can subscribe to, such as account_review_update, carry
			// no status query and no delivery to log, so they are acknowledged and
			// dropped.
			switch change.Field {
			case "messages":
				s.handleStatuses(change.Value.Statuses)
				s.handleMessages(change.Value.Messages)
			}
		}
	}
}

// metaErrorCode reads the error code from the Graph API error envelope Meta
// returns for a rejected request, `{"error":{"code":...,"message":...}}`. Meta
// documents Cloud API errors as arriving synchronously, asynchronously, or both,
// and recommends building error handling on the code. The synchronous send
// response uses this code for retry classification; status webhooks are only
// logged. The code is raw JSON, the same shape rule as webhookError. It is 0 when
// the body is not the envelope, the code is absent, or it is not a number in
// either shape.
func metaErrorCode(body []byte) int {
	var envelope struct {
		Error webhookError `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return 0
	}

	return envelope.Error.codeValue()
}

// transientSendCodes classify synchronous send responses only: a listed Meta
// error code is a condition that can change, so a send rejected with one is
// retried. Status webhooks are logged and do not use this table. The list is
// deliberately short: every code that is not listed is terminal, which is the safe
// default, because repeating a policy, auth, billing, or window rejection once per
// minute sends nothing and is the pattern Meta enforces with account-level limits.
// The codes and their documented resolutions are quoted from
// https://developers.facebook.com/documentation/business-messaging/whatsapp/support/error-codes.md
// so the table can be rechecked against the page. Excluded: 131047 (the customer
// service window has passed, and only a new message from the participant reopens
// it), 131042 (payment method: Meta lists billing corrections, not a waiting
// period), 131049 (per-user marketing template limit, documented as a 24-hour
// wait) and 131064 (messaging limit from template classification violations,
// lifted after an enforcement period Meta does not date). None is a condition a
// 1-minute retry can clear, and Meta gives no short interval for any of them.
// 131064 is documented as applying to both template and direct-send messages, so
// its exclusion is the fix, not the message type; 131049 concerns marketing
// templates, which this service does not send.
var transientSendCodes = map[int]bool{
	130429: true, // Cloud API message throughput has been reached
	131000: true, // unknown error, documented as "Try again"
	131016: true, // a service is temporarily unavailable
	131056: true, // too many messages to the same recipient in a short period
	131057: true, // business account in maintenance mode
	133004: true, // server temporarily unavailable
}

// handleStatuses logs delivery transitions, once per message-status-recipient
// transition. The codes of a failed status are logged with it. Nothing acts on
// them: the only message this service sends is a status query answer, and a reply
// is retried from the send response, not from a status webhook.
func (s *webhookServer) handleStatuses(statuses []webhookStatusRecord) {
	for _, record := range statuses {
		if !s.state.noteStatus(record, time.Now()) {
			continue
		}

		// Both recipient identifiers can be omitted depending on how the reply was
		// addressed and on the status Meta reports, so the log names the message
		// alone when neither arrived.
		participant := record.participant()
		if participant == "" {
			log.Printf("WhatsApp message %s: %s", record.ID, record.Status)
		} else {
			log.Printf("WhatsApp message %s for %s: %s", record.ID, participant, record.Status)
		}

		for _, e := range record.Errors {
			log.Printf("WhatsApp delivery error %d: %s", e.codeValue(), e.Message)
		}
	}
}

// handleMessages deduplicates inbound 1:1 messages, then answers text matching
// the configured status expression with the current status, addressed to the
// participant who sent it. A message that carries a group_id is not answered:
// the bot replies only in the 1:1 thread a participant opens with the number.
func (s *webhookServer) handleMessages(messages []webhookMessageRecord) {
	for _, record := range messages {
		if record.GroupID != "" {
			continue
		}

		if !s.state.recordInbound(record.ID, time.Now()) {
			continue
		}

		if record.Type != "text" {
			continue
		}
		if !s.statusRegexp.MatchString(record.Text.Body) {
			continue
		}

		if record.From == "" && record.FromUserID == "" {
			// Meta sent no identifier to answer, so there is no thread to reply in.
			// The deduplication entry is kept: the same delivery carries the same
			// absence, so re-processing it would log the same warning again.
			log.Printf("Warning: WhatsApp status query %s has no sender identifier, so it is not answered", record.ID)

			continue
		}

		reply := whatsappReply{
			Text:   fmt.Sprintf("Связь есть %s", time.Now().Format(time.RFC1123)),
			Phone:  record.From,
			UserID: record.FromUserID,
		}
		// Replies go through the pacing gate too: sending one inside a Retry-After
		// window recorded from an earlier 429 would outrun Meta's flow control for
		// the phone number. An answer that cannot be sent now, or that fails with
		// a retryable error, is queued and the responder sends it again at its
		// 1-minute cadence; a rejection Meta reports for the answer is not queued,
		// so the participant sees the failure in the log and can ask again.
		attempted, retryable, err := s.publisher.publishReply(reply)
		reportReplyOutcome(attempted, retryable, err)
	}
}
